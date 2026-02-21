pub mod hub;

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use anyhow::Result;
use chrono::Utc;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{mpsc, RwLock};

use crate::protocol::*;
use crate::store::Store;
use hub::{ClientHandle, HubCommand, run_hub};

const SEND_BUF: usize = 256;
const WORKER_JOBS: usize = 1024;

// ─── Per-connection identity ───────────────────────────────────────────────

#[derive(Clone)]
struct Identity {
    user_id: String,
    username: String,
}

struct ClientState {
    #[allow(dead_code)]
    id: String,
    send_tx: mpsc::Sender<Vec<u8>>,
    identity: RwLock<Option<Identity>>,
}

impl ClientState {
    fn new(id: String, send_tx: mpsc::Sender<Vec<u8>>) -> Arc<Self> {
        Arc::new(Self {
            id,
            send_tx,
            identity: RwLock::new(None),
        })
    }

    async fn is_authenticated(&self) -> bool {
        self.identity.read().await.is_some()
    }

    async fn set_identity(&self, user_id: String, username: String) {
        *self.identity.write().await = Some(Identity { user_id, username });
    }

    async fn get_identity(&self) -> Option<Identity> {
        self.identity.read().await.clone()
    }

    fn send_packet(&self, pkt: &Packet) {
        if let Ok(mut data) = serde_json::to_vec(pkt) {
            data.push(b'\n');
            self.send_tx.try_send(data).ok();
        }
    }

    fn send_response(&self, success: bool, message: &str, data: Option<serde_json::Value>) {
        let payload = ResponsePayload {
            success,
            message: message.to_string(),
            data,
        };
        if let Ok(pkt) = Packet::new(MessageType::Response, payload) {
            self.send_packet(&pkt);
        }
    }

    fn send_error(&self, msg: &str) {
        self.send_response(false, &format!("error: {}", msg), None);
    }

    fn send_system(&self, msg: &str) {
        let payload = serde_json::json!({"message": msg});
        if let Ok(pkt) = Packet::new(MessageType::System, payload) {
            self.send_packet(&pkt);
        }
    }
}

// ─── Worker pool for async persistence ─────────────────────────────────────

struct WorkerPool {
    tx: mpsc::Sender<StoredMessage>,
}

impl WorkerPool {
    fn new(n: usize, store: Arc<Store>) -> Self {
        let (tx, rx) = mpsc::channel::<StoredMessage>(WORKER_JOBS);
        // Single tokio task handles the channel; spawn n workers via rayon-style approach
        // (For simplicity: one async task per worker draining the same channel via Arc<Mutex>)
        // Actually: use n independent tasks that all share the same receiver via Arc<Mutex>
        use std::sync::Mutex;
        let rx = Arc::new(Mutex::new(rx));
        for _ in 0..n {
            let store = store.clone();
            let rx = rx.clone();
            tokio::spawn(async move {
                loop {
                    let msg = {
                        let mut guard = rx.lock().unwrap();
                        // poll — if channel empty, yield
                        match guard.try_recv() {
                            Ok(m) => Some(m),
                            Err(_) => None,
                        }
                    };
                    if let Some(msg) = msg {
                        if let Err(e) = store.save_message(msg) {
                            eprintln!("[store] save error: {}", e);
                        }
                    } else {
                        tokio::time::sleep(tokio::time::Duration::from_millis(5)).await;
                    }
                }
            });
        }
        Self { tx }
    }

    fn submit(&self, msg: StoredMessage) {
        if self.tx.try_send(msg).is_err() {
            eprintln!("[pool] job queue full – message dropped from persistence");
        }
    }
}

// ─── Server ─────────────────────────────────────────────────────────────────

pub struct Server {
    store: Arc<Store>,
    pool: Arc<WorkerPool>,
    hub_tx: mpsc::Sender<HubCommand>,
    online: Arc<RwLock<HashMap<String, Arc<ClientState>>>>,
    conn_counter: Arc<AtomicU64>,
}

impl Server {
    pub fn new(data_dir: &str, workers: usize) -> Result<Self> {
        let store = Arc::new(Store::new(data_dir)?);
        let (hub_tx, hub_rx) = mpsc::channel(256);
        tokio::spawn(run_hub(hub_rx));

        let pool = Arc::new(WorkerPool::new(workers, store.clone()));

        Ok(Self {
            store,
            pool,
            hub_tx,
            online: Arc::new(RwLock::new(HashMap::new())),
            conn_counter: Arc::new(AtomicU64::new(0)),
        })
    }

    pub async fn listen_and_serve(self: Arc<Self>, addr: &str) -> Result<()> {
        let listener = TcpListener::bind(addr).await?;
        eprintln!("[server] listening on {}", addr);

        loop {
            match listener.accept().await {
                Ok((conn, _)) => {
                    let srv = self.clone();
                    tokio::spawn(srv.serve_conn(conn));
                }
                Err(e) => {
                    eprintln!("[server] accept error: {}", e);
                    return Ok(());
                }
            }
        }
    }

    async fn serve_conn(self: Arc<Self>, conn: TcpStream) {
        let id = format!("conn-{}", self.conn_counter.fetch_add(1, Ordering::Relaxed));
        let (send_tx, mut send_rx) = mpsc::channel::<Vec<u8>>(SEND_BUF);
        let client = ClientState::new(id.clone(), send_tx);

        // Register with hub (unauthenticated placeholder username)
        self.hub_tx
            .send(HubCommand::Register(ClientHandle {
                id: id.clone(),
                username: String::new(),
                tx: client.send_tx.clone(),
            }))
            .await
            .ok();

        // Split TCP stream
        let (reader, mut writer) = conn.into_split();

        // Write pump
        let write_id = id.clone();
        tokio::spawn(async move {
            while let Some(data) = send_rx.recv().await {
                if writer.write_all(&data).await.is_err() {
                    break;
                }
            }
            eprintln!("[server] write pump ended for {}", write_id);
        });

        // Send welcome
        client.send_system("Welcome to RustChat! Use /register or /login to get started.");

        // Read pump (runs in this task)
        let srv = self.clone();
        let c = client.clone();
        let mut lines = BufReader::new(reader).lines();

        while let Ok(Some(line)) = lines.next_line().await {
            let pkt: Packet = match serde_json::from_str(&line) {
                Ok(p) => p,
                Err(_) => {
                    c.send_error("malformed packet");
                    continue;
                }
            };
            srv.handle_packet(&c, pkt).await;
        }

        // Cleanup
        srv.hub_tx.send(HubCommand::Unregister(id.clone())).await.ok();
        if let Some(ident) = client.get_identity().await {
            srv.online.write().await.remove(&ident.user_id);
        }
        eprintln!("[server] connection {} closed", id);
    }

    async fn handle_packet(self: &Arc<Self>, client: &Arc<ClientState>, pkt: Packet) {
        match pkt.msg_type {
            MessageType::Register => self.handle_register(client, pkt.payload).await,
            MessageType::Login => self.handle_login(client, pkt.payload).await,
            MessageType::Chat => self.handle_chat(client, pkt.payload).await,
            MessageType::Search => self.handle_search(client, pkt.payload).await,
            MessageType::History => self.handle_history(client, pkt.payload).await,
            MessageType::Users => self.handle_users(client).await,
            MessageType::Quit => { /* connection will close when read pump exits */ }
            _ => client.send_error(&format!("unknown packet type")),
        }
    }

    async fn handle_register(self: &Arc<Self>, client: &Arc<ClientState>, raw: serde_json::Value) {
        let p: AuthPayload = match serde_json::from_value::<AuthPayload>(raw) {
            Ok(p) if !p.username.is_empty() && !p.password.is_empty() => p,
            _ => {
                client.send_error("register requires {username, password}");
                return;
            }
        };

        match self.store.register_user(&p.username, &p.password) {
            Err(e) => client.send_error(&e.to_string()),
            Ok(user) => {
                client.set_identity(user.id.clone(), user.username.clone()).await;
                self.online.write().await.insert(user.id.clone(), client.clone());
                client.send_response(
                    true,
                    &format!("registered and logged in as {:?}", user.username),
                    None,
                );
                self.broadcast_system(&format!("{} joined the chat", user.username)).await;
                eprintln!("[server] registered {} ({})", user.username, user.id);
            }
        }
    }

    async fn handle_login(self: &Arc<Self>, client: &Arc<ClientState>, raw: serde_json::Value) {
        let p: AuthPayload = match serde_json::from_value::<AuthPayload>(raw) {
            Ok(p) if !p.username.is_empty() && !p.password.is_empty() => p,
            _ => {
                client.send_error("login requires {username, password}");
                return;
            }
        };

        match self.store.authenticate(&p.username, &p.password) {
            Err(e) => client.send_error(&e.to_string()),
            Ok(user) => {
                client.set_identity(user.id.clone(), user.username.clone()).await;
                self.online.write().await.insert(user.id.clone(), client.clone());
                client.send_response(
                    true,
                    &format!("logged in as {:?}", user.username),
                    None,
                );
                self.broadcast_system(&format!("{} joined the chat", user.username)).await;
                eprintln!("[server] login {} ({})", user.username, user.id);
            }
        }
    }

    async fn handle_chat(self: &Arc<Self>, client: &Arc<ClientState>, raw: serde_json::Value) {
        if !client.is_authenticated().await {
            client.send_error("you must login or register first");
            return;
        }

        let p: ChatPayload = match serde_json::from_value::<ChatPayload>(raw) {
            Ok(p) if !p.content.is_empty() => p,
            _ => {
                client.send_error("chat requires {content}");
                return;
            }
        };

        let ident = client.get_identity().await.unwrap();
        let now = Utc::now();
        let msg = StoredMessage {
            id: format!("{}", now.timestamp_nanos_opt().unwrap_or(0)),
            user_id: ident.user_id.clone(),
            username: ident.username.clone(),
            content: p.content.clone(),
            timestamp: now,
        };

        // Broadcast immediately
        let bcast_payload = BroadcastPayload {
            user_id: msg.user_id.clone(),
            username: msg.username.clone(),
            content: msg.content.clone(),
            timestamp: msg.timestamp,
        };
        if let Ok(pkt) = Packet::new(MessageType::Broadcast, bcast_payload) {
            if let Ok(mut data) = serde_json::to_vec(&pkt) {
                data.push(b'\n');
                self.hub_tx.send(HubCommand::Broadcast(data)).await.ok();
            }
        }

        // Persist asynchronously
        self.pool.submit(msg);
    }

    async fn handle_search(self: &Arc<Self>, client: &Arc<ClientState>, raw: serde_json::Value) {
        if !client.is_authenticated().await {
            client.send_error("you must login first");
            return;
        }

        let p: SearchPayload = match serde_json::from_value(raw) {
            Ok(p) => p,
            Err(_) => {
                client.send_error("malformed search payload");
                return;
            }
        };

        if p.query.is_empty() && p.username.is_empty() && p.from.is_none() && p.to.is_none() {
            client.send_error(
                "provide at least one search criterion (query, username, from, or to)",
            );
            return;
        }

        let results = self.store.search(&p.query, &p.username, p.from, p.to);
        let count = results.len();
        let data = serde_json::to_value(results).ok();
        client.send_response(true, &format!("{} result(s)", count), data);
    }

    async fn handle_history(self: &Arc<Self>, client: &Arc<ClientState>, raw: serde_json::Value) {
        if !client.is_authenticated().await {
            client.send_error("you must login first");
            return;
        }

        let limit = serde_json::from_value::<HistoryPayload>(raw)
            .map(|p| if p.limit == 0 { 20 } else { p.limit })
            .unwrap_or(20);

        let msgs = self.store.get_history(limit);
        let count = msgs.len();
        let data = serde_json::to_value(msgs).ok();
        client.send_response(true, &format!("last {} message(s)", count), data);
    }

    async fn handle_users(self: &Arc<Self>, client: &Arc<ClientState>) {
        if !client.is_authenticated().await {
            client.send_error("you must login first");
            return;
        }

        let online = self.online.read().await;
        let mut users = Vec::new();
        for (user_id, c) in online.iter() {
            if let Some(ident) = c.get_identity().await {
                users.push(UserInfo {
                    user_id: user_id.clone(),
                    username: ident.username,
                });
            }
        }
        let count = users.len();
        let data = serde_json::to_value(users).ok();
        client.send_response(true, &format!("{} user(s) online", count), data);
    }

    async fn broadcast_system(self: &Arc<Self>, msg: &str) {
        let payload = serde_json::json!({"message": msg});
        if let Ok(pkt) = Packet::new(MessageType::System, payload) {
            if let Ok(mut data) = serde_json::to_vec(&pkt) {
                data.push(b'\n');
                self.hub_tx.send(HubCommand::Broadcast(data)).await.ok();
            }
        }
    }
}
