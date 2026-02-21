use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::RwLock;
use std::fs;

use anyhow::Result;
use chrono::{DateTime, Utc};
use hex;
use rand::Rng;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::protocol::StoredMessage;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct User {
    pub id: String,
    pub username: String,
    pub password_hash: String,
    pub created_at: DateTime<Utc>,
}

struct Inner {
    users: HashMap<String, User>,  // keyed by lowercase username
    by_id: HashMap<String, User>,  // keyed by user ID
    messages: Vec<StoredMessage>,
}

pub struct Store {
    inner: RwLock<Inner>,
    data_dir: PathBuf,
}

impl Store {
    pub fn new(data_dir: impl AsRef<Path>) -> Result<Self> {
        let data_dir = data_dir.as_ref().to_path_buf();
        fs::create_dir_all(&data_dir)?;

        let mut inner = Inner {
            users: HashMap::new(),
            by_id: HashMap::new(),
            messages: Vec::new(),
        };

        let users_path = data_dir.join("users.json");
        if users_path.exists() {
            let data = fs::read_to_string(&users_path)?;
            let users: Vec<User> = serde_json::from_str(&data)?;
            for u in users {
                inner.users.insert(u.username.to_lowercase(), u.clone());
                inner.by_id.insert(u.id.clone(), u);
            }
        }

        let msgs_path = data_dir.join("messages.json");
        if msgs_path.exists() {
            let data = fs::read_to_string(&msgs_path)?;
            inner.messages = serde_json::from_str(&data)?;
        }

        Ok(Self { inner: RwLock::new(inner), data_dir })
    }

    pub fn register_user(&self, username: &str, password: &str) -> Result<User> {
        let mut inner = self.inner.write().unwrap();
        let key = username.to_lowercase();

        if inner.users.contains_key(&key) {
            anyhow::bail!("username {:?} is already taken", username);
        }

        let user = User {
            id: generate_id(),
            username: username.to_string(),
            password_hash: hash_password(password),
            created_at: Utc::now(),
        };

        inner.users.insert(key, user.clone());
        inner.by_id.insert(user.id.clone(), user.clone());

        let users: Vec<User> = inner.users.values().cloned().collect();
        let path = self.data_dir.join("users.json");
        drop(inner);
        write_json(&path, &users)?;

        Ok(user)
    }

    pub fn authenticate(&self, username: &str, password: &str) -> Result<User> {
        let inner = self.inner.read().unwrap();
        let key = username.to_lowercase();

        let user = inner
            .users
            .get(&key)
            .ok_or_else(|| anyhow::anyhow!("user {:?} not found", username))?;

        if user.password_hash != hash_password(password) {
            anyhow::bail!("incorrect password");
        }

        Ok(user.clone())
    }

    pub fn save_message(&self, msg: StoredMessage) -> Result<()> {
        let mut inner = self.inner.write().unwrap();
        inner.messages.push(msg);
        let msgs = inner.messages.clone();
        let path = self.data_dir.join("messages.json");
        drop(inner);
        write_json(&path, &msgs)?;
        Ok(())
    }

    pub fn get_history(&self, n: usize) -> Vec<StoredMessage> {
        let inner = self.inner.read().unwrap();
        let total = inner.messages.len();
        if n == 0 || n >= total {
            inner.messages.clone()
        } else {
            inner.messages[total - n..].to_vec()
        }
    }

    pub fn search(
        &self,
        query: &str,
        username: &str,
        from: Option<DateTime<Utc>>,
        to: Option<DateTime<Utc>>,
    ) -> Vec<StoredMessage> {
        let inner = self.inner.read().unwrap();
        let q = query.to_lowercase();
        let u = username.to_lowercase();

        inner
            .messages
            .iter()
            .filter(|m| {
                if !q.is_empty() && !m.content.to_lowercase().contains(&q) {
                    return false;
                }
                if !u.is_empty() && m.username.to_lowercase() != u {
                    return false;
                }
                if let Some(from) = from {
                    if m.timestamp < from {
                        return false;
                    }
                }
                if let Some(to) = to {
                    if m.timestamp > to {
                        return false;
                    }
                }
                true
            })
            .cloned()
            .collect()
    }
}

fn hash_password(pw: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(pw.as_bytes());
    hex::encode(hasher.finalize())
}

fn generate_id() -> String {
    let ts = Utc::now().timestamp_nanos_opt().unwrap_or(0);
    let rand_part: u16 = rand::thread_rng().gen_range(0..0xFFFF);
    format!("{}-{:04x}", ts, rand_part)
}

fn write_json(path: &Path, v: &impl Serialize) -> Result<()> {
    let data = serde_json::to_string_pretty(v)?;
    fs::write(path, data)?;
    Ok(())
}
