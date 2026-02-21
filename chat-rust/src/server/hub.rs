use std::collections::HashMap;
use tokio::sync::mpsc;

pub struct ClientHandle {
    pub id: String,
    pub username: String,
    pub tx: mpsc::Sender<Vec<u8>>,
}

pub enum HubCommand {
    Register(ClientHandle),
    Unregister(String), // client id
    Broadcast(Vec<u8>),
}

/// run_hub fans out every broadcast to all connected clients.
/// It must be spawned as a tokio task.
pub async fn run_hub(mut rx: mpsc::Receiver<HubCommand>) {
    let mut clients: HashMap<String, ClientHandle> = HashMap::new();

    while let Some(cmd) = rx.recv().await {
        match cmd {
            HubCommand::Register(handle) => {
                eprintln!(
                    "[hub] +client {} ({})  total={}",
                    handle.username,
                    handle.id,
                    clients.len() + 1
                );
                clients.insert(handle.id.clone(), handle);
            }
            HubCommand::Unregister(id) => {
                if let Some(handle) = clients.remove(&id) {
                    eprintln!(
                        "[hub] -client {} ({})  total={}",
                        handle.username,
                        handle.id,
                        clients.len()
                    );
                }
            }
            HubCommand::Broadcast(data) => {
                let mut to_remove = Vec::new();
                for (id, handle) in &clients {
                    if handle.tx.try_send(data.clone()).is_err() {
                        eprintln!("[hub] dropped slow client {}", handle.username);
                        to_remove.push(id.clone());
                    }
                }
                for id in to_remove {
                    clients.remove(&id);
                }
            }
        }
    }
}
