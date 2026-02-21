// Package protocol defines the wire format for all client-server communication.
// Each message is a newline-delimited JSON Packet.
package protocol

import (
	"encoding/json"
	"time"
)

// MessageType identifies what kind of packet is being sent.
type MessageType string

const (
	// Client → Server
	TypeRegister MessageType = "register"
	TypeLogin    MessageType = "login"
	TypeChat     MessageType = "chat"
	TypeSearch   MessageType = "search"
	TypeHistory  MessageType = "history"
	TypeUsers    MessageType = "users"
	TypeQuit     MessageType = "quit"

	// Server → Client
	TypeResponse  MessageType = "response"
	TypeBroadcast MessageType = "broadcast"
	TypeSystem    MessageType = "system"
)

// Packet is the top-level wire format.  Every packet is a single JSON object
// followed by a newline character (\n).
type Packet struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// NewPacket marshals payload and returns a ready-to-send Packet.
func NewPacket(t MessageType, payload any) (*Packet, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &Packet{Type: t, Payload: raw}, nil
}

// Encode returns the JSON bytes for p (no trailing newline).
func (p *Packet) Encode() ([]byte, error) {
	return json.Marshal(p)
}

// ---------------------------------------------------------------------------
// Payload types
// ---------------------------------------------------------------------------

// AuthPayload is used for both /register and /login.
type AuthPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ChatPayload carries a user's chat message.
type ChatPayload struct {
	Content string `json:"content"`
}

// SearchPayload carries search criteria.  All fields are optional and are
// combined with AND logic: only messages matching every non-empty criterion
// are returned.
type SearchPayload struct {
	Query    string     `json:"query"`              // case-insensitive content substring
	Username string     `json:"username,omitempty"` // exact username (case-insensitive)
	From     *time.Time `json:"from,omitempty"`     // inclusive start of timestamp range
	To       *time.Time `json:"to,omitempty"`       // inclusive end of timestamp range
}

// HistoryPayload requests the last N messages.
type HistoryPayload struct {
	Limit int `json:"limit"`
}

// ResponsePayload is the generic server acknowledgement.
type ResponsePayload struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// BroadcastPayload is sent to every connected client when a message is posted.
type BroadcastPayload struct {
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// StoredMessage is the on-disk representation of a chat message.
type StoredMessage struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// UserInfo describes a currently online user.
type UserInfo struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
}
