package server

import "log"

// Hub is the central message router.  It owns the set of connected clients and
// fans out every broadcast to all of them.
//
// Concurrency model
// -----------------
//   • The Hub runs in a single dedicated goroutine (Hub.Run).
//   • All mutations to the clients map happen inside that goroutine, so no
//     mutex is needed for the map itself.
//   • Other goroutines communicate with the Hub exclusively through channels:
//       register   – add a new client
//       unregister – remove a client and close its send channel
//       broadcast  – deliver a JSON-encoded packet to every client
//   • Each Client has a buffered send channel (size 256).  If the buffer fills
//     up (slow/stuck client), the Hub drops that client rather than blocking
//     the entire broadcast.
type Hub struct {
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte // newline-terminated JSON packet
	done       chan struct{}
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte, 256),
		done:       make(chan struct{}),
	}
}

// Run processes hub events.  It must be launched as a goroutine.
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = true
			log.Printf("[hub] +client %s (%s)  total=%d", c.username, c.id, len(h.clients))

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				log.Printf("[hub] -client %s (%s)  total=%d", c.username, c.id, len(h.clients))
			}

		case data := <-h.broadcast:
			for c := range h.clients {
				select {
				case c.send <- data:
				default:
					// Client is not draining its send channel; drop it.
					delete(h.clients, c)
					close(c.send)
					log.Printf("[hub] dropped slow client %s", c.username)
				}
			}

		case <-h.done:
			// Close every outstanding send channel so writePumps unblock.
			for c := range h.clients {
				close(c.send)
			}
			return
		}
	}
}

// Stop signals the hub to shut down.
func (h *Hub) Stop() { close(h.done) }
