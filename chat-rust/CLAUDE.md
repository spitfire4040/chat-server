# RustChat — Client/Server Chat Application

## Project Overview

A TCP-based chat application with a TUI client. The server handles multiple concurrent connections, persists users and messages to JSON files, and broadcasts messages to all connected clients. The client is a ratatui terminal UI.

## Architecture

```
src/
├── lib.rs              # re-exports: protocol, store, server
├── protocol.rs         # Packet, MessageType, all payload structs
├── store.rs            # file-backed Store (users.json, messages.json)
├── server/
│   ├── mod.rs          # Server, ClientState, WorkerPool, connection handling
│   └── hub.rs          # broadcast hub (fans packets to all connected clients)
└── bin/
    ├── server.rs       # server entry point (clap CLI)
    └── client.rs       # ratatui TUI client entry point
```

## Build & Run

```bash
# Build both binaries
make build

# Run the server (default: :8080, ./data, 4 workers)
make run-server
# or directly:
cargo run --bin server -- --addr 0.0.0.0:8080 --data ./data --workers 4

# Run the client (default: localhost:8080)
make run-client
# or directly:
cargo run --bin client -- --addr localhost:8080

# Clean build artifacts and data directory
make clean
```

## Protocol

Newline-delimited JSON over raw TCP. Every packet is a JSON object ending with `\n`.

```json
{"type": "<MessageType>", "payload": { ... }}
```

**Client → Server message types:** `register`, `login`, `chat`, `search`, `history`, `users`, `quit`

**Server → Client message types:** `response`, `broadcast`, `system`

Key payload types are defined in `src/protocol.rs`: `AuthPayload`, `ChatPayload`, `SearchPayload`, `HistoryPayload`, `ResponsePayload`, `BroadcastPayload`, `StoredMessage`, `UserInfo`.

## TUI Client Screens & Keybindings

**Login screen:**
- `Tab` / `Shift+Tab` — switch between Username and Password fields
- `Ctrl+R` — toggle between Login and Register mode
- `Enter` — submit
- `Ctrl+C` / `Ctrl+Q` — quit

**Chat screen:**
- `Enter` — send message
- `Ctrl+F` — open search overlay
- `PgUp` / `PgDn` — scroll message history
- `Ctrl+C` / `Ctrl+Q` — quit

**Search overlay:**
- `Tab` / `Shift+Tab` — cycle through fields (Content, Username, From, To)
- `Enter` — execute search
- `PgUp` / `PgDn` — scroll results
- `Esc` — close overlay

Date fields accept `YYYY-MM-DD` (treated as midnight UTC) or RFC 3339.

## Data Persistence

The `Store` (`src/store.rs`) holds an in-memory `RwLock<Inner>` and flushes to two JSON files on every write:
- `<data_dir>/users.json` — array of `User` objects
- `<data_dir>/messages.json` — array of `StoredMessage` objects

Passwords are stored as SHA-256 hashes (unsalted).

## Concurrency Model

- One tokio task per TCP connection (read pump); a separate spawned task acts as write pump.
- A single `run_hub` task (`src/server/hub.rs`) fans broadcast packets out to all connected clients via `mpsc::Sender<Vec<u8>>`.
- A `WorkerPool` of `n` tokio tasks drains a shared `Mutex<mpsc::Receiver<StoredMessage>>` and calls `store.save_message` asynchronously.

## Key Dependencies

| Crate | Purpose |
|---|---|
| `tokio` (full) | async runtime, TCP, channels |
| `serde` / `serde_json` | JSON serialization |
| `ratatui` | terminal UI widgets |
| `crossterm` | terminal raw mode, keyboard events |
| `clap` (derive) | CLI argument parsing |
| `sha2` / `hex` | password hashing |
| `chrono` | timestamps, date parsing |
| `anyhow` | error handling |
| `rand` | random suffix in message IDs |
