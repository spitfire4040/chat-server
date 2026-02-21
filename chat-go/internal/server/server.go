// Package server implements the TCP chat server.
//
// Concurrency overview
// --------------------
//
//  ┌─────────────────────────────────────────────────────────┐
//  │  Listener goroutine                                      │
//  │  Accepts TCP connections; spawns readPump + writePump    │
//  │  goroutines for each Client.                             │
//  └───────────────────┬─────────────────────────────────────┘
//                      │  register / unregister / broadcast channels
//                      ▼
//  ┌─────────────────────────────────────────────────────────┐
//  │  Hub goroutine                                           │
//  │  Owns the clients map; fans out broadcasts.              │
//  └─────────────────────────────────────────────────────────┘
//
//  ┌─────────────────────────────────────────────────────────┐
//  │  Worker Pool  (N goroutines)                             │
//  │  Asynchronously persist messages to disk so the hot      │
//  │  broadcast path is never blocked by I/O.                 │
//  └─────────────────────────────────────────────────────────┘
//
//  ┌─────────────────────────────────────────────────────────┐
//  │  Store  (sync.RWMutex)                                   │
//  │  In-memory user + message store backed by JSON files.    │
//  └─────────────────────────────────────────────────────────┘
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"chat/internal/protocol"
	"chat/internal/store"
)

// ---------------------------------------------------------------------------
// Worker pool – async message persistence
// ---------------------------------------------------------------------------

// workerPool persists chat messages in the background so the broadcast path
// (which runs inside the Hub goroutine) is never blocked by disk I/O.
type workerPool struct {
	jobs chan *protocol.StoredMessage
	wg   sync.WaitGroup
}

func newWorkerPool(n int, s *store.Store) *workerPool {
	p := &workerPool{
		jobs: make(chan *protocol.StoredMessage, 1024),
	}
	for i := 0; i < n; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for msg := range p.jobs {
				if err := s.SaveMessage(msg); err != nil {
					log.Printf("[store] save error: %v", err)
				}
			}
		}()
	}
	return p
}

func (p *workerPool) submit(msg *protocol.StoredMessage) {
	// Non-blocking submit; drop silently if the queue is full.
	select {
	case p.jobs <- msg:
	default:
		log.Printf("[pool] job queue full – message dropped from persistence")
	}
}

func (p *workerPool) stop() {
	close(p.jobs)
	p.wg.Wait()
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// Server ties together the Hub, Store, and WorkerPool.
type Server struct {
	hub      *Hub
	store    *store.Store
	pool     *workerPool
	listener net.Listener

	// online tracks authenticated clients for /users queries.
	// A separate RWMutex is used here so listing online users does not
	// require a round-trip through the Hub's event channel.
	onlineMu sync.RWMutex
	online   map[string]*Client // userID → Client

	connID atomic.Uint64 // monotonically increasing connection counter
}

// New creates a Server.  dataDir is where users.json and messages.json live.
// workers controls the number of persistence goroutines in the pool.
func New(dataDir string, workers int) (*Server, error) {
	st, err := store.New(dataDir)
	if err != nil {
		return nil, err
	}
	h := newHub()
	return &Server{
		hub:    h,
		store:  st,
		pool:   newWorkerPool(workers, st),
		online: make(map[string]*Client),
	}, nil
}

// ListenAndServe starts the Hub and then accepts TCP connections on addr.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = ln
	log.Printf("[server] listening on %s", addr)

	go s.hub.Run()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Closed by Shutdown.
			return nil
		}
		go s.serveConn(conn)
	}
}

// Shutdown cleanly stops the server.
func (s *Server) Shutdown() {
	if s.listener != nil {
		s.listener.Close()
	}
	s.hub.Stop()
	s.pool.stop()
}

// serveConn creates a Client for conn and launches its read/write pumps.
func (s *Server) serveConn(conn net.Conn) {
	id := fmt.Sprintf("conn-%d", s.connID.Add(1))
	c := newClient(id, conn, s)
	s.hub.register <- c

	// writePump runs in its own goroutine; readPump runs in this one.
	go c.writePump()
	c.sendSystem("Welcome to GoChat! Use /register or /login to get started.")
	c.readPump()
}

// ---------------------------------------------------------------------------
// Online user tracking
// ---------------------------------------------------------------------------

func (s *Server) addOnline(c *Client) {
	s.onlineMu.Lock()
	defer s.onlineMu.Unlock()
	s.online[c.userID] = c
}

func (s *Server) removeOnline(c *Client) {
	if !c.isAuthenticated() {
		return
	}
	s.onlineMu.Lock()
	defer s.onlineMu.Unlock()
	delete(s.online, c.userID)
}

func (s *Server) onlineUsers() []protocol.UserInfo {
	s.onlineMu.RLock()
	defer s.onlineMu.RUnlock()

	out := make([]protocol.UserInfo, 0, len(s.online))
	for _, c := range s.online {
		out = append(out, protocol.UserInfo{UserID: c.userID, Username: c.username})
	}
	return out
}

// ---------------------------------------------------------------------------
// Packet dispatch
// ---------------------------------------------------------------------------

func (s *Server) handlePacket(c *Client, pkt *protocol.Packet) {
	switch pkt.Type {
	case protocol.TypeRegister:
		s.handleRegister(c, pkt.Payload)
	case protocol.TypeLogin:
		s.handleLogin(c, pkt.Payload)
	case protocol.TypeChat:
		s.handleChat(c, pkt.Payload)
	case protocol.TypeSearch:
		s.handleSearch(c, pkt.Payload)
	case protocol.TypeHistory:
		s.handleHistory(c, pkt.Payload)
	case protocol.TypeUsers:
		s.handleUsers(c)
	case protocol.TypeQuit:
		c.conn.Close()
	default:
		c.sendError(fmt.Sprintf("unknown packet type %q", pkt.Type))
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleRegister(c *Client, raw json.RawMessage) {
	var p protocol.AuthPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Username == "" || p.Password == "" {
		c.sendError("register requires {username, password}")
		return
	}
	u, err := s.store.RegisterUser(p.Username, p.Password)
	if err != nil {
		c.sendError(err.Error())
		return
	}
	c.setIdentity(u.ID, u.Username)
	s.addOnline(c)
	c.sendResponse(true, fmt.Sprintf("registered and logged in as %q", u.Username), nil)
	s.broadcastSystem(fmt.Sprintf("%s joined the chat", u.Username))
	log.Printf("[server] registered %s (%s)", u.Username, u.ID)
}

func (s *Server) handleLogin(c *Client, raw json.RawMessage) {
	var p protocol.AuthPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Username == "" || p.Password == "" {
		c.sendError("login requires {username, password}")
		return
	}
	u, err := s.store.Authenticate(p.Username, p.Password)
	if err != nil {
		c.sendError(err.Error())
		return
	}
	c.setIdentity(u.ID, u.Username)
	s.addOnline(c)
	c.sendResponse(true, fmt.Sprintf("logged in as %q", u.Username), nil)
	s.broadcastSystem(fmt.Sprintf("%s joined the chat", u.Username))
	log.Printf("[server] login %s (%s)", u.Username, u.ID)
}

func (s *Server) handleChat(c *Client, raw json.RawMessage) {
	if !c.isAuthenticated() {
		c.sendError("you must login or register first")
		return
	}
	var p protocol.ChatPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Content == "" {
		c.sendError("chat requires {content}")
		return
	}

	now := time.Now().UTC()
	msg := &protocol.StoredMessage{
		ID:        fmt.Sprintf("%d", now.UnixNano()),
		UserID:    c.userID,
		Username:  c.username,
		Content:   p.Content,
		Timestamp: now,
	}

	// 1. Broadcast immediately to all connected clients (fast path).
	bcast, _ := protocol.NewPacket(protocol.TypeBroadcast, protocol.BroadcastPayload{
		UserID:    msg.UserID,
		Username:  msg.Username,
		Content:   msg.Content,
		Timestamp: msg.Timestamp,
	})
	data, _ := bcast.Encode()
	s.hub.broadcast <- append(data, '\n')

	// 2. Persist asynchronously via the worker pool (slow path).
	s.pool.submit(msg)
}

func (s *Server) handleSearch(c *Client, raw json.RawMessage) {
	if !c.isAuthenticated() {
		c.sendError("you must login first")
		return
	}
	var p protocol.SearchPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		c.sendError("malformed search payload")
		return
	}
	if p.Query == "" && p.Username == "" && p.From == nil && p.To == nil {
		c.sendError("provide at least one search criterion (query, username, from, or to)")
		return
	}
	results := s.store.Search(p.Query, p.Username, p.From, p.To)
	c.sendResponse(true, fmt.Sprintf("%d result(s)", len(results)), results)
}

func (s *Server) handleHistory(c *Client, raw json.RawMessage) {
	if !c.isAuthenticated() {
		c.sendError("you must login first")
		return
	}
	var p protocol.HistoryPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		p.Limit = 20
	}
	if p.Limit <= 0 {
		p.Limit = 20
	}
	msgs := s.store.GetHistory(p.Limit)
	c.sendResponse(true, fmt.Sprintf("last %d message(s)", len(msgs)), msgs)
}

func (s *Server) handleUsers(c *Client) {
	if !c.isAuthenticated() {
		c.sendError("you must login first")
		return
	}
	users := s.onlineUsers()
	c.sendResponse(true, fmt.Sprintf("%d user(s) online", len(users)), users)
}

// broadcastSystem sends a system notice to every connected client.
func (s *Server) broadcastSystem(msg string) {
	pkt, _ := protocol.NewPacket(protocol.TypeSystem, map[string]string{"message": msg})
	data, _ := pkt.Encode()
	s.hub.broadcast <- append(data, '\n')
}
