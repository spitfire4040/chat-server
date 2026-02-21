use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

/// MessageType identifies what kind of packet is being sent.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum MessageType {
    // Client → Server
    Register,
    Login,
    Chat,
    Search,
    History,
    Users,
    Quit,
    // Server → Client
    Response,
    Broadcast,
    System,
}

/// Every packet is a single JSON object followed by a newline character (\n).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Packet {
    #[serde(rename = "type")]
    pub msg_type: MessageType,
    pub payload: serde_json::Value,
}

impl Packet {
    pub fn new(msg_type: MessageType, payload: impl Serialize) -> anyhow::Result<Self> {
        Ok(Self {
            msg_type,
            payload: serde_json::to_value(payload)?,
        })
    }

    pub fn encode(&self) -> anyhow::Result<Vec<u8>> {
        Ok(serde_json::to_vec(self)?)
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuthPayload {
    pub username: String,
    pub password: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ChatPayload {
    pub content: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SearchPayload {
    #[serde(default)]
    pub query: String,
    #[serde(default)]
    pub username: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub from: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub to: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HistoryPayload {
    pub limit: usize,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResponsePayload {
    pub success: bool,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub data: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BroadcastPayload {
    pub user_id: String,
    pub username: String,
    pub content: String,
    pub timestamp: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StoredMessage {
    pub id: String,
    pub user_id: String,
    pub username: String,
    pub content: String,
    pub timestamp: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UserInfo {
    pub user_id: String,
    pub username: String,
}
