use serde::{Deserialize, Deserializer, Serialize};
use serde_json::value::RawValue;

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Agent {
    Claude,
    Codex,
}

impl Agent {
    pub fn as_str(self) -> &'static str {
        match self {
            Agent::Claude => "claude",
            Agent::Codex => "codex",
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Kind {
    TurnStarted,
    UserMessage,
    Text,
    Reasoning,
    ToolCall,
    ToolResult,
    TurnFinished,
    TurnFailed,
}

impl Kind {
    pub fn as_str(self) -> &'static str {
        match self {
            Kind::TurnStarted => "turn_started",
            Kind::UserMessage => "user_message",
            Kind::Text => "text",
            Kind::Reasoning => "reasoning",
            Kind::ToolCall => "tool_call",
            Kind::ToolResult => "tool_result",
            Kind::TurnFinished => "turn_finished",
            Kind::TurnFailed => "turn_failed",
        }
    }
}

// Field order and omitempty rules match the Go ToolCall so the client sees
// the same JSON in the tool column.
#[derive(Serialize, Debug, Default)]
pub struct ToolCall {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub call_id: String,
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub input: Option<Box<RawValue>>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub output: String,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub is_error: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<ToolResult>,
}

// Go's agent.ToolResult less Commands, which only a Codex cell has and Go reads.
#[derive(Serialize, Debug, Default)]
pub struct ToolResult {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i64>,
    #[serde(skip_serializing_if = "is_zero")]
    pub duration_ms: i64,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub interrupted: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub timed_out: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub stderr: String,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub created: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub urls: Vec<ToolUrl>,
    #[serde(skip_serializing_if = "is_zero")]
    pub http_status: i64,
}

#[derive(Serialize, Deserialize, Debug, Default)]
#[serde(default)]
pub struct ToolUrl {
    #[serde(skip_serializing_if = "String::is_empty", deserialize_with = "s")]
    pub title: String,
    #[serde(skip_serializing_if = "String::is_empty", deserialize_with = "s")]
    pub url: String,
}

fn is_zero(n: &i64) -> bool {
    *n == 0
}

#[derive(Debug, Default)]
pub struct Event {
    pub turn_id: String,
    pub kind: Kind,
    pub at: i64,
    pub text: String,
    pub tool: Option<ToolCall>,
    pub error: String,
}

impl Default for Kind {
    fn default() -> Self {
        Kind::Text
    }
}

#[derive(Clone, Debug, Default)]
pub struct Meta {
    pub id: String,
    pub agent: Option<Agent>,
    pub cwd: String,
    pub title: String,
    pub path: String,
    pub paths: Vec<String>,
    pub updated_at_ms: i64,
    pub updated_at_ns: i128,
    pub size_bytes: i64,
    pub context_used: i64,
    pub context_size: i64,
}

impl Meta {
    pub fn files(&self) -> Vec<&str> {
        if self.paths.is_empty() {
            vec![self.path.as_str()]
        } else {
            self.paths.iter().map(String::as_str).collect()
        }
    }
}

pub struct Entry {
    pub meta: Meta,
    pub events: Vec<Event>,
}

// Go's decoder treats JSON null as "leave the zero value"; serde rejects it
// unless the field is an Option. These helpers give every field Go's leniency.
pub fn s<'de, D: Deserializer<'de>>(d: D) -> Result<String, D::Error> {
    Ok(Option::<String>::deserialize(d)?.unwrap_or_default())
}

pub fn b<'de, D: Deserializer<'de>>(d: D) -> Result<bool, D::Error> {
    Ok(Option::<bool>::deserialize(d)?.unwrap_or_default())
}

pub fn i<'de, D: Deserializer<'de>>(d: D) -> Result<i64, D::Error> {
    Ok(Option::<i64>::deserialize(d)?.unwrap_or_default())
}

pub fn f<'de, D: Deserializer<'de>>(d: D) -> Result<f64, D::Error> {
    Ok(Option::<f64>::deserialize(d)?.unwrap_or_default())
}

pub fn v<'de, D: Deserializer<'de>, T: Deserialize<'de>>(d: D) -> Result<Vec<T>, D::Error> {
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}

// A present null is kept as the literal `null`, the way json.RawMessage does.
pub fn raw<'de, D: Deserializer<'de>>(d: D) -> Result<Option<Box<RawValue>>, D::Error> {
    Box::<RawValue>::deserialize(d).map(Some)
}

pub fn unix_ms(timestamp: &str) -> i64 {
    if timestamp.is_empty() {
        return 0;
    }
    chrono::DateTime::parse_from_rfc3339(timestamp)
        .map(|t| t.timestamp_millis())
        .unwrap_or(0)
}
