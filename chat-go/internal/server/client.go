package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"chat/internal/protocol"
)

const (
	sendBufSize  = 256           // buffered send channel capacity
	writeTimeout = 10 * time.Second
	readTimeout  = 5 * time.Minute // idle connection timeout
)

// Client represents one TCP connection.
//
// Two goroutines are spawned per client:
//
//	readPump  – reads newline-delimited JSON from the TCP connection and
//	            dispatches to the Server for processing.
//	writePump – drains the send channel and writes packets to the TCP
//	            connection.
//
// This decouples reading from writing so a slow writer never blocks readers.
type Client struct {
	id       string // unique connection identifier
	server   *Server
	conn     net.Conn
	send     chan []byte // outbound newline-terminated JSON packets

	// Authenticated identity.  Protected by mu because readPump sets them
	// after a successful login/register, and other goroutines may read them.
	mu       sync.RWMutex
	userID   string
	username string
}

func newClient(id string, conn net.Conn, srv *Server) *Client {
	return &Client{
		id:     id,
		conn:   conn,
		server: srv,
		send:   make(chan []byte, sendBufSize),
	}
}

func (c *Client) getUsername() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.username
}

func (c *Client) isAuthenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.userID != ""
}

func (c *Client) setIdentity(userID, username string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userID = userID
	c.username = username
}

// readPump reads packets from the TCP connection line by line and dispatches
// them to the Server.  When the connection drops it unregisters the client.
func (c *Client) readPump() {
	defer func() {
		c.server.hub.unregister <- c
		c.server.removeOnline(c)
		c.conn.Close()
	}()

	scanner := bufio.NewScanner(c.conn)
	for scanner.Scan() {
		c.conn.SetDeadline(time.Now().Add(readTimeout))

		var pkt protocol.Packet
		if err := json.Unmarshal(scanner.Bytes(), &pkt); err != nil {
			c.sendError("malformed packet")
			continue
		}
		c.server.handlePacket(c, &pkt)
	}
}

// writePump drains the send channel and writes each payload to the TCP
// connection.  A write deadline is set for every write to prevent blocking
// indefinitely on a stuck client.
func (c *Client) writePump() {
	defer c.conn.Close()

	for data := range c.send {
		c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := c.conn.Write(data); err != nil {
			return
		}
	}
}

// sendPacket marshals pkt, appends a newline, and queues it on the send channel.
// Non-blocking: if the buffer is full the packet is silently dropped.
func (c *Client) sendPacket(pkt *protocol.Packet) {
	data, err := pkt.Encode()
	if err != nil {
		return
	}
	line := append(data, '\n')
	select {
	case c.send <- line:
	default:
	}
}

// sendResponse is a convenience helper for TypeResponse packets.
func (c *Client) sendResponse(success bool, msg string, data any) {
	var raw json.RawMessage
	if data != nil {
		b, _ := json.Marshal(data)
		raw = b
	}
	pkt, _ := protocol.NewPacket(protocol.TypeResponse, protocol.ResponsePayload{
		Success: success,
		Message: msg,
		Data:    raw,
	})
	c.sendPacket(pkt)
}

// sendError sends a typed error packet.
func (c *Client) sendError(msg string) {
	pkt, _ := protocol.NewPacket(protocol.TypeResponse, protocol.ResponsePayload{
		Success: false,
		Message: fmt.Sprintf("error: %s", msg),
	})
	c.sendPacket(pkt)
}

// sendSystem sends a server system-notice to this client only.
func (c *Client) sendSystem(msg string) {
	pkt, _ := protocol.NewPacket(protocol.TypeSystem, map[string]string{"message": msg})
	c.sendPacket(pkt)
}
