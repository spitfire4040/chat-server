// Package store provides persistent, concurrent-safe storage for users and
// chat messages.  Data is written as JSON files inside a configurable directory.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"chat/internal/protocol"
)

// User is a registered account.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

// Store holds users and messages in memory and persists them to disk.
// A sync.RWMutex protects the in-memory state so multiple goroutines can read
// concurrently while writes are serialised.
type Store struct {
	mu       sync.RWMutex
	users    map[string]*User          // keyed by lower-case username
	byID     map[string]*User          // keyed by user ID
	messages []*protocol.StoredMessage // ordered by insertion time
	dataDir  string
}

// New creates (or reopens) a Store backed by files in dataDir.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	s := &Store{
		users:   make(map[string]*User),
		byID:    make(map[string]*User),
		dataDir: dataDir,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// RegisterUser creates a new user account.  Returns an error when the username
// is already taken.
func (s *Store) RegisterUser(username, password string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := strings.ToLower(username)
	if _, exists := s.users[key]; exists {
		return nil, fmt.Errorf("username %q is already taken", username)
	}

	u := &User{
		ID:           generateID(),
		Username:     username,
		PasswordHash: hashPassword(password),
		CreatedAt:    time.Now().UTC(),
	}
	s.users[key] = u
	s.byID[u.ID] = u
	return u, s.saveUsersLocked()
}

// Authenticate verifies credentials and returns the matching User.
func (s *Store) Authenticate(username, password string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	u, ok := s.users[strings.ToLower(username)]
	if !ok {
		return nil, fmt.Errorf("user %q not found", username)
	}
	if u.PasswordHash != hashPassword(password) {
		return nil, fmt.Errorf("incorrect password")
	}
	return u, nil
}

// SaveMessage appends msg to the in-memory list and persists it to disk.
func (s *Store) SaveMessage(msg *protocol.StoredMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = append(s.messages, msg)
	return s.saveMessagesLocked()
}

// GetHistory returns the last n messages.  When n <= 0 all messages are
// returned.
func (s *Store) GetHistory(n int) []*protocol.StoredMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := len(s.messages)
	if n <= 0 || n >= total {
		out := make([]*protocol.StoredMessage, total)
		copy(out, s.messages)
		return out
	}
	out := make([]*protocol.StoredMessage, n)
	copy(out, s.messages[total-n:])
	return out
}

// Search returns messages matching all non-empty criteria (AND logic):
//   - query    – case-insensitive substring match against content
//   - username – case-insensitive exact match against the sender's username
//   - from     – message timestamp must be >= from (inclusive)
//   - to       – message timestamp must be <= to   (inclusive)
func (s *Store) Search(query, username string, from, to *time.Time) []*protocol.StoredMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(query)
	u := strings.ToLower(username)

	var out []*protocol.StoredMessage
	for _, m := range s.messages {
		if q != "" && !strings.Contains(strings.ToLower(m.Content), q) {
			continue
		}
		if u != "" && !strings.EqualFold(m.Username, u) {
			continue
		}
		if from != nil && m.Timestamp.Before(*from) {
			continue
		}
		if to != nil && m.Timestamp.After(*to) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func (s *Store) load() error {
	usersPath := filepath.Join(s.dataDir, "users.json")
	if data, err := os.ReadFile(usersPath); err == nil {
		var users []*User
		if err := json.Unmarshal(data, &users); err != nil {
			return fmt.Errorf("store: parse users.json: %w", err)
		}
		for _, u := range users {
			s.users[strings.ToLower(u.Username)] = u
			s.byID[u.ID] = u
		}
	}

	msgsPath := filepath.Join(s.dataDir, "messages.json")
	if data, err := os.ReadFile(msgsPath); err == nil {
		if err := json.Unmarshal(data, &s.messages); err != nil {
			return fmt.Errorf("store: parse messages.json: %w", err)
		}
	}
	return nil
}

func (s *Store) saveUsersLocked() error {
	users := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	return writeJSON(filepath.Join(s.dataDir, "users.json"), users)
}

func (s *Store) saveMessagesLocked() error {
	return writeJSON(filepath.Join(s.dataDir, "messages.json"), s.messages)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func hashPassword(pw string) string {
	h := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(h[:])
}

func generateID() string {
	// nano-timestamp + random hex nibbles — sufficient for a local demo.
	return fmt.Sprintf("%d-%04x", time.Now().UnixNano(), rand.Intn(0xFFFF))
}
