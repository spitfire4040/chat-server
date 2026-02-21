# GoChat — Developer Reference

A concurrent TCP chat server and interactive terminal client written in Go.
No external dependencies — only the standard library.

---

## Project Layout

```
client-server-chat/
├── go.mod                        # module: chat, requires go 1.21
├── Makefile                      # build / run helpers
├── data/                         # created at runtime by the server
│   ├── users.json                # persisted user accounts
│   └── messages.json             # persisted chat messages
│
├── cmd/
│   ├── server/
│   │   └── main.go               # server entry point; CLI flags; signal handling
│   └── client/
│       └── main.go               # interactive terminal client
│
└── internal/
    ├── protocol/
    │   └── protocol.go           # wire-format types (Packet, payloads)
    ├── store/
    │   └── store.go              # persistent user + message storage
    └── server/
        ├── hub.go                # central message router (channel-based)
        ├── client.go             # per-connection read/write goroutines
        └── server.go             # TCP listener, worker pool, packet dispatch
```

---

## Building and Running

### Prerequisites

- Go 1.21 or later (`go version` to check)
- A terminal that supports ANSI colour codes (any modern macOS/Linux terminal)

### Quick start with `go run`

Open **two or more** terminal windows.

**Terminal 1 — server**
```
go run ./cmd/server
```

**Terminal 2 — first client**
```
go run ./cmd/client
```

**Terminal 3 — second client** (simultaneous session)
```
go run ./cmd/client
```

### Build binaries with Make

```bash
make build          # compiles both binaries into bin/
make run-server     # go run ./cmd/server with default flags
make run-client     # go run ./cmd/client with default flags
make clean          # removes bin/ and data/
```

### CLI flags

**Server** (`cmd/server/main.go`)

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `:8080` | TCP address to listen on |
| `-data` | `./data` | Directory for `users.json` and `messages.json` |
| `-workers` | `4` | Number of message-persistence worker goroutines |

Example:
```
go run ./cmd/server -addr :9000 -data /tmp/chat -workers 8
```

**Client** (`cmd/client/main.go`)

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `localhost:8080` | Server address to connect to |

Example:
```
go run ./cmd/client -addr localhost:9000
```

---

## Client Interface

The client is a full-screen terminal UI built with **Bubbletea + Lipgloss**.
It occupies the alternate screen buffer, so your shell history is preserved when you exit.

### Screens

**Login screen** (shown on startup)
```
         GoChat Terminal

Username    alice__________________________
Password    ••••••_________________________

Tab: switch field   Enter: Login   Ctrl+R: switch to Register
Ctrl+C: quit
```
- `Tab` — cycle between Username / Password fields
- `Enter` — submit (login or register based on current mode)
- `Ctrl+R` — toggle between Login and Register mode

**Chat screen** (after authentication)
```
 GoChat · alice · 3 online · Ctrl+F: Search  PgUp/Dn: Scroll  Ctrl+C: Quit
 [10:30:15] alice: Hello everyone!
 [10:30:20] bob:   Hey Alice!
 ──────────────────────────────────────────────────────────────────
 Type a message…
```
- `Enter` — send chat message to all participants
- `Ctrl+F` — open the search overlay
- `PgUp / PgDn` — scroll the message history
- `Ctrl+C` — disconnect and quit

**Search overlay** (Ctrl+F from chat)
```
 Search History  ·  Esc: return to chat  Ctrl+C: quit
 Content    hello________________________________
 User       alice________________________________
 From       2026-02-01______  (YYYY-MM-DD, optional)
 To         ________________  (YYYY-MM-DD, optional)

 Tab: next field   Enter: search   Esc: close
────────────────────────────────────────────────
 2 result(s)
 [2026-02-21 10:30:15] alice: Hello everyone!
 [2026-02-21 10:31:00] alice: How are you?
```
- `Tab / Shift+Tab` — cycle between the four search fields
- `Enter` — execute search (all non-empty fields are ANDed together)
- `Esc` — close the overlay and return to chat

### Search criteria (all optional, combined with AND)

| Field | Matches |
|-------|---------|
| Content | Case-insensitive substring of message text |
| User | Exact username match (case-insensitive) |
| From | Messages with timestamp ≥ date (start of day, local time) |
| To | Messages with timestamp ≤ date (end of day, local time) |

Multiple clients logged in as different users will see each other's messages in real time.
A user may hold simultaneous sessions (multiple logins with the same credentials are permitted).

---

## Architecture

### Components

```
  ┌─────────────────────────────────────────────────────────┐
  │  Listener goroutine  (server.ListenAndServe)             │
  │  Accepts TCP connections; spawns one serveConn           │
  │  goroutine per connection.                               │
  └───────────────┬─────────────────────────────────────────┘
                  │ per-connection goroutine
                  ▼
  ┌─────────────────────────────────────────────────────────┐
  │  Client  (internal/server/client.go)                     │
  │  readPump goroutine  — TCP → Server.handlePacket         │
  │  writePump goroutine — client.send channel → TCP         │
  └───────┬───────────────────────┬─────────────────────────┘
          │ register/unregister/  │ pool.submit(msg)
          │ broadcast channels    │
          ▼                       ▼
  ┌───────────────┐     ┌─────────────────────────────────┐
  │  Hub          │     │  Worker Pool  (N goroutines)     │
  │  (hub.go)     │     │  Asynchronously saves messages   │
  │  Single       │     │  to disk via store.SaveMessage.  │
  │  goroutine.   │     │  Broadcast never waits on I/O.   │
  │  Owns clients │     └──────────────┬──────────────────┘
  │  map; fans    │                    │
  │  out messages.│                    ▼
  └───────────────┘     ┌─────────────────────────────────┐
                        │  Store  (store.go)               │
                        │  sync.RWMutex-protected maps.    │
                        │  Backed by users.json and        │
                        │  messages.json.                  │
                        └─────────────────────────────────┘
```

### Wire Protocol (`internal/protocol/protocol.go`)

All communication uses **newline-delimited JSON**. Each packet is a single
JSON object on one line, terminated by `\n`. The top-level shape is:

```json
{"type": "<MessageType>", "payload": { ... }}
```

**Client → Server message types**

| `type` | Payload struct | Description |
|--------|---------------|-------------|
| `register` | `AuthPayload{username, password}` | Create account + auto-login |
| `login` | `AuthPayload{username, password}` | Authenticate |
| `chat` | `ChatPayload{content}` | Send a message |
| `search` | `SearchPayload{query?, username?, from?, to?}` | Search — all fields optional, ANDed |
| `history` | `HistoryPayload{limit}` | Fetch recent messages |
| `users` | *(empty object)* | List online users |
| `quit` | *(empty object)* | Signal clean disconnect |

**Server → Client message types**

| `type` | Payload struct | Description |
|--------|---------------|-------------|
| `response` | `ResponsePayload{success, message, data?}` | ACK/NACK for any client request |
| `broadcast` | `BroadcastPayload{user_id, username, content, timestamp}` | A chat message from any user |
| `system` | `{"message": "..."}` | Server notices (join, welcome, etc.) |

### Hub (`internal/server/hub.go`)

The Hub is a single goroutine that owns the `clients map[*Client]bool`.
Because only one goroutine ever touches the map, no mutex is needed for it.
All other goroutines communicate with the Hub exclusively through three channels:

```
register   chan *Client   — adds a client to the map
unregister chan *Client   — removes a client; closes its send channel
broadcast  chan []byte    — fans the payload out to every client's send channel
```

If a client's `send` channel buffer (capacity 256) is full at broadcast time,
the Hub treats it as a stuck client: it removes it from the map and closes
its channel, causing `writePump` to exit.

### Client connection lifecycle (`internal/server/client.go`)

For every accepted TCP connection, `serveConn` does:

1. Creates a `Client` struct with a buffered `send chan []byte` (cap 256).
2. Sends a `register` request to the Hub.
3. Starts `writePump` in a new goroutine.
4. Calls `readPump` directly (blocks until connection closes).

**`readPump`** reads lines from the TCP connection with `bufio.Scanner`,
unmarshals each line as a `protocol.Packet`, and calls `server.handlePacket`.
On exit it sends an `unregister` request to the Hub and removes the user
from the online-users map.

**`writePump`** ranges over `client.send`. For each `[]byte` it sets a
10-second write deadline and writes to the TCP connection. When the Hub
closes `client.send`, the range loop exits and `writePump` returns.

A `sync.RWMutex` on `Client` protects `userID` and `username`, which are
set by `readPump` after a successful login but may be read by other goroutines
(e.g. the Hub logging a disconnect).

### Worker Pool (`internal/server/server.go`)

When a chat message arrives:

1. **Broadcast immediately** — the Hub fans the `BroadcastPayload` out to
   every connected client's `send` channel. This is pure in-memory work
   and completes in microseconds.

2. **Persist asynchronously** — the message is submitted to the worker pool's
   `jobs` channel (capacity 1024) via a non-blocking `select`. One of the N
   worker goroutines picks it up and calls `store.SaveMessage`, which holds
   a mutex and writes to disk. If the queue is full the message is logged and
   dropped from persistence (broadcast already happened).

This two-path design means disk I/O never adds latency to the broadcast.

### Online-user tracking

The Hub's `clients` map is not safe to read from outside the Hub goroutine,
so a separate `online map[string]*Client` (keyed by `userID`) is maintained
in `Server`, protected by its own `sync.RWMutex`. It is updated in
`handleRegister`/`handleLogin` (add) and in `readPump`'s deferred cleanup
(remove). The `/users` handler reads it with `RLock`, allowing concurrent
reads from multiple client goroutines without blocking.

### Store (`internal/store/store.go`)

`Store` keeps two in-memory data structures:

- `users map[string]*User` — keyed by lower-cased username for case-insensitive lookup
- `byID map[string]*User` — keyed by user ID (available for future use)
- `messages []*StoredMessage` — insertion-ordered slice

A single `sync.RWMutex` protects all three. Public methods that only read
use `RLock`/`RUnlock`; methods that write use `Lock`/`Unlock`.

On every write, the full slice/map is marshalled to JSON and written to disk
with `os.WriteFile` (atomic from the OS perspective on most systems). The
`_Locked` suffix on internal helpers signals that the caller already holds
the write lock.

Passwords are stored as SHA-256 hex digests. IDs are generated as
`UnixNano-randomHex`, which is unique enough for a local demo.

---

## Go Concurrency Features — Quick Reference

| Feature | Where used |
|---------|-----------|
| **Goroutines** | Listener, Hub.Run, serveConn, readPump, writePump, each worker, TCP reader in client |
| **Unbuffered channels** | `hub.register`, `hub.unregister` — synchronise client lifecycle |
| **Buffered channels** | `hub.broadcast` (256), `client.send` (256), `pool.jobs` (1024), `pkts` (64) in client |
| **`select`** | Hub event loop; non-blocking submit to pool; client send drop |
| **`sync.RWMutex`** | `Store` (users + messages), `Server.onlineMu`, `Client.mu` |
| **`sync.WaitGroup`** | Worker pool — `stop()` waits for all jobs to finish before returning |
| **`sync/atomic`** | `Server.connID` — lock-free monotonic connection counter |
| **Done channel pattern** | Hub shutdown (`hub.done`); `pkts` channel close signals TUI disconnect |
| **tea.Cmd chaining** | `waitForPkt` re-queues itself after each packet — non-blocking TUI integration |

---

## Data Files

Both files are plain JSON arrays and are human-readable.

**`data/users.json`** — array of user objects:
```json
[
  {
    "id": "1771633148156388000-97c9",
    "username": "alice",
    "password_hash": "4e738ca5...",
    "created_at": "2026-02-21T00:19:08.156404Z"
  }
]
```

**`data/messages.json`** — array of message objects, ordered oldest-first:
```json
[
  {
    "id": "1771633148208998000",
    "user_id": "1771633148156388000-97c9",
    "username": "alice",
    "content": "Hello everyone!",
    "timestamp": "2026-02-21T00:19:08.208998Z"
  }
]
```

Both files are loaded into memory at server startup and updated on every write.
Delete them (or run `make clean`) to reset all state.

---

## Graceful Shutdown

Sending `SIGINT` (Ctrl-C) or `SIGTERM` to the server triggers:

1. `listener.Close()` — stops accepting new connections.
2. `hub.Stop()` — closes `hub.done`; Hub closes every `client.send` channel,
   which causes all `writePump` goroutines to return.
3. `pool.stop()` — closes `pool.jobs`; waits for all in-flight persistence
   jobs to complete before returning.

---

## Extending the Application

**Add a command:** define a new `TypeXxx` constant in `protocol.go`, add a
payload struct if needed, add a `case` in `server.handlePacket`, implement
the handler, and add a `case` in the client's `processInput` switch.

**Replace JSON storage with a database:** swap out `internal/store/store.go`.
The public API (`RegisterUser`, `Authenticate`, `SaveMessage`, `GetHistory`,
`Search`) is the only surface the rest of the application uses.

**Add TLS:** wrap the `net.Listener` in `tls.NewListener` in `ListenAndServe`
and wrap the `net.Dial` in `tls.Dial` in the client.

**Add a web client:** the protocol is plain newline-delimited JSON over TCP,
so a WebSocket bridge or an HTTP gateway can sit in front of the server
without changing any internal packages.
