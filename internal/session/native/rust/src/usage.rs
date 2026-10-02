// Token usage per API request, for the host's usage ledger. The same rules as
// the Go parsers in agents/claude/shipping.go and agents/codex/shipping.go,
// which a build without this library uses; TestNativeUsageParity holds them
// together. Only lines carrying usage are decoded.
use serde::{Deserialize, Deserializer};

use crate::event::{self, unix_ms, Agent};
use crate::peek::LINE_LIMIT;

#[derive(Debug, Default, PartialEq)]
pub struct Record {
    pub key: String,
    pub session: String,
    pub at: i64,
    pub model: String,
    pub uncached: i64,
    pub cached: i64,
    pub creation: i64,
    pub creation_1h: i64,
    pub output: i64,
    pub reasoning: i64,
    pub fast: bool,
    pub reported_usd: Option<f64>,
}

// A token count as Go's shipping.Positive reads it: a whole positive number,
// anything else zero.
fn count<'de, D: Deserializer<'de>>(d: D) -> Result<i64, D::Error> {
    let v = Option::<serde_json::Value>::deserialize(d)?;
    Ok(match v.as_ref().and_then(|v| v.as_f64()) {
        Some(n) if n > 0.0 && n.fract() == 0.0 => n as i64,
        _ => 0,
    })
}

fn number<'de, D: Deserializer<'de>>(d: D) -> Result<Option<f64>, D::Error> {
    let v = Option::<serde_json::Value>::deserialize(d)?;
    Ok(v.and_then(|v| v.as_f64()))
}

pub fn scan(agent: Agent, path: &str) -> std::io::Result<Vec<Record>> {
    let buf = std::fs::read(path)?;
    let mut out = Vec::new();
    let mut codex = CodexState::default();
    let mut rest = buf.as_slice();
    // A trailing fragment without its newline is a write in progress.
    while let Some(i) = memchr::memchr(b'\n', rest) {
        let line = &rest[..=i];
        rest = &rest[i + 1..];
        if line.len() > LINE_LIMIT {
            continue;
        }
        let rec = match agent {
            Agent::Claude => claude(line),
            Agent::Codex => codex.parse(line),
        };
        out.extend(rec);
    }
    Ok(out)
}

// Model names that are not one billable model: aliases naming a family.
const UNPRICED: &[&str] = &["<synthetic>", "synthetic", "opus", "sonnet", "haiku", "fable"];

fn claude(line: &[u8]) -> Option<Record> {
    memchr::memmem::find(line, b"\"usage\"")?;

    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct CacheCreation {
        #[serde(rename = "ephemeral_5m_input_tokens", deserialize_with = "count")]
        five: i64,
        #[serde(rename = "ephemeral_1h_input_tokens", deserialize_with = "count")]
        hour: i64,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Usage {
        #[serde(deserialize_with = "count")]
        input_tokens: i64,
        #[serde(deserialize_with = "count")]
        cache_read_input_tokens: i64,
        #[serde(deserialize_with = "count")]
        cache_creation_input_tokens: i64,
        #[serde(deserialize_with = "count")]
        output_tokens: i64,
        #[serde(deserialize_with = "event::s")]
        speed: String,
        cache_creation: Option<CacheCreation>,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Message {
        #[serde(deserialize_with = "event::s")]
        id: String,
        #[serde(deserialize_with = "event::s")]
        model: String,
        usage: Option<Usage>,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Line {
        #[serde(rename = "type", deserialize_with = "event::s")]
        typ: String,
        #[serde(deserialize_with = "event::s")]
        timestamp: String,
        #[serde(rename = "sessionId", deserialize_with = "event::s")]
        session_id: String,
        #[serde(rename = "requestId", deserialize_with = "event::s")]
        request_id: String,
        #[serde(rename = "costUSD", deserialize_with = "number")]
        cost_usd: Option<f64>,
        message: Option<Message>,
    }

    let l: Line = serde_json::from_slice(line).ok()?;
    if l.typ != "assistant" {
        return None;
    }
    let msg = l.message?;
    let usage = msg.usage?;
    let at = unix_ms(&l.timestamp);
    if at == 0 || msg.model.is_empty() || msg.model == "<synthetic>" {
        return None;
    }
    let model = if UNPRICED.contains(&msg.model.to_lowercase().as_str()) { String::new() } else { msg.model };
    let mut r = Record {
        session: l.session_id,
        at,
        model,
        uncached: usage.input_tokens,
        cached: usage.cache_read_input_tokens,
        creation: usage.cache_creation_input_tokens,
        output: usage.output_tokens,
        fast: usage.speed == "fast",
        reported_usd: l.cost_usd.filter(|c| *c >= 0.0),
        ..Default::default()
    };
    if let Some(c) = usage.cache_creation {
        if c.five + c.hour > 0 {
            r.creation = c.five;
            r.creation_1h = c.hour;
        }
    }
    if !msg.id.is_empty() || !l.request_id.is_empty() {
        r.key = format!("{}:{}", msg.id, l.request_id);
    }
    Some(r)
}

// A fork's copied history is written in one burst; its own first token count
// comes seconds later.
const FORK_COPY_GAP: i64 = 1000;

#[derive(Default)]
struct CodexState {
    // The rollout's own thread keys its requests; session is the chat they
    // count under, the parent's for a spawned subagent.
    id: String,
    session: String,
    saw_meta: bool,
    model: String,
    copying: bool,
    copy_anchor: i64,
    last_total: i64,
    last: [i64; 5],
}

impl CodexState {
    fn parse(&mut self, line: &[u8]) -> Option<Record> {
        let wanted = [&b"\"token_count\""[..], b"\"turn_context\"", b"\"session_meta\""];
        if !wanted.iter().any(|n| memchr::memmem::find(line, n).is_some()) {
            return None;
        }

        #[derive(Deserialize, Default)]
        #[serde(default)]
        struct Last {
            #[serde(deserialize_with = "count")]
            input_tokens: i64,
            #[serde(deserialize_with = "count")]
            cached_input_tokens: i64,
            #[serde(deserialize_with = "count")]
            cache_write_input_tokens: i64,
            #[serde(deserialize_with = "count")]
            output_tokens: i64,
            #[serde(deserialize_with = "count")]
            reasoning_output_tokens: i64,
        }
        #[derive(Deserialize, Default)]
        #[serde(default)]
        struct Total {
            #[serde(deserialize_with = "count")]
            total_tokens: i64,
        }
        #[derive(Deserialize, Default)]
        #[serde(default)]
        struct Info {
            last_token_usage: Option<Last>,
            total_token_usage: Option<Total>,
        }
        #[derive(Deserialize, Default)]
        #[serde(default)]
        struct Payload {
            #[serde(deserialize_with = "event::s")]
            id: String,
            #[serde(deserialize_with = "event::s")]
            forked_from_id: String,
            source: Option<serde_json::Value>,
            #[serde(deserialize_with = "event::s")]
            model: String,
            #[serde(rename = "type", deserialize_with = "event::s")]
            typ: String,
            info: Option<Info>,
        }
        #[derive(Deserialize, Default)]
        #[serde(default)]
        struct Line {
            #[serde(rename = "type", deserialize_with = "event::s")]
            typ: String,
            #[serde(deserialize_with = "event::s")]
            timestamp: String,
            payload: Option<Payload>,
        }

        let l: Line = serde_json::from_slice(line).ok()?;
        let p = l.payload.unwrap_or_default();
        let at = unix_ms(&l.timestamp);
        match l.typ.as_str() {
            "session_meta" => {
                // Only the first names this file's session; a fork repeats its ancestors' after it.
                if !self.saw_meta {
                    self.saw_meta = true;
                    let parent = spawned(p.source.as_ref());
                    self.session = if parent.is_empty() { p.id.clone() } else { parent.clone() };
                    self.id = p.id;
                    if !p.forked_from_id.is_empty() || !parent.is_empty() {
                        self.copying = true;
                        self.copy_anchor = at;
                    }
                }
                return None;
            }
            "turn_context" => {
                if !p.model.is_empty() {
                    self.model = p.model;
                }
                return None;
            }
            _ => {}
        }
        if p.typ != "token_count" {
            return None;
        }
        let info = p.info?;
        let last = info.last_token_usage?;
        if self.model.is_empty() || at == 0 {
            return None;
        }
        let sig = [last.input_tokens, last.cached_input_tokens, last.cache_write_input_tokens, last.output_tokens, last.reasoning_output_tokens];
        let total = info.total_token_usage.map(|t| t.total_tokens).unwrap_or(0);
        let repeat = (total > 0 && total == self.last_total) || (total == 0 && sig == self.last);
        self.last_total = total;
        self.last = sig;
        if self.copying {
            if at - self.copy_anchor < FORK_COPY_GAP {
                self.copy_anchor = at;
                return None;
            }
            self.copying = false;
        }
        if repeat {
            return None;
        }
        // input_tokens includes the cached and cache-write portions.
        let uncached = 0.max(0.max(sig[0] - sig[1]) - sig[2]);
        if uncached + sig[1] + sig[2] + sig[3] == 0 {
            return None;
        }
        let key = if total > 0 && !self.id.is_empty() { format!("{}:{}", self.id, total) } else { String::new() };
        Some(Record {
            key,
            session: self.session.clone(),
            at,
            model: self.model.clone(),
            uncached,
            cached: sig[1],
            creation: sig[2],
            output: sig[3],
            reasoning: sig[4],
            ..Default::default()
        })
    }
}

// The parent thread a spawned subagent's meta names, "" for none.
fn spawned(source: Option<&serde_json::Value>) -> String {
    source
        .and_then(|s| s.get("subagent"))
        .and_then(|s| s.get("thread_spawn"))
        .and_then(|s| s.get("parent_thread_id"))
        .and_then(|p| p.as_str())
        .unwrap_or_default()
        .to_string()
}
