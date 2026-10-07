use std::fs;

use rayon::prelude::*;
use serde::Deserialize;
use serde_json::value::RawValue;

use crate::contextwindow::context_window_for;
use crate::event::{self, Agent, Event, Kind, Meta, ToolCall};
use crate::home::claude_home;
use crate::images;
use crate::peek::{scan_head, scan_tail, PeekResult};
use crate::reader::Provider;

pub struct Claude;

pub fn list() -> std::io::Result<Vec<Meta>> {
    let projects = claude_home().join("projects");
    let dirs = match fs::read_dir(&projects) {
        Ok(d) => d,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(e),
    };

    let mut files: Vec<(String, String, String, fs::Metadata)> = Vec::new();
    for d in dirs.flatten() {
        if !d.file_type().map(|t| t.is_dir()).unwrap_or(false) {
            continue;
        }
        let dir_name = d.file_name().to_string_lossy().into_owned();
        let Ok(entries) = fs::read_dir(d.path()) else { continue };
        for e in entries.flatten() {
            let name = e.file_name().to_string_lossy().into_owned();
            if !name.ends_with(".jsonl") || e.file_type().map(|t| t.is_dir()).unwrap_or(true) {
                continue;
            }
            let Ok(info) = e.metadata() else { continue };
            let path = e.path().to_string_lossy().into_owned();
            files.push((dir_name.clone(), name, path, info));
        }
    }

    Ok(files
        .par_iter()
        .map(|(dir, name, path, info)| {
            let peeked = peek(path, info.len());
            let cwd = if peeked.cwd.is_empty() { dir.replace('-', "/") } else { peeked.cwd };
            let (ms, ns) = crate::mtime(info);
            Meta {
                id: name.trim_end_matches(".jsonl").to_string(),
                agent: Some(Agent::Claude),
                cwd,
                title: peeked.title,
                path: path.clone(),
                paths: Vec::new(),
                updated_at_ms: ms,
                updated_at_ns: ns,
                size_bytes: info.len() as i64,
                context_used: peeked.context_used,
                context_size: peeked.context_size,
            }
        })
        .collect())
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Usage {
    #[serde(deserialize_with = "event::i")]
    input_tokens: i64,
    #[serde(deserialize_with = "event::i")]
    cache_creation_input_tokens: i64,
    #[serde(deserialize_with = "event::i")]
    cache_read_input_tokens: i64,
}

fn peek(path: &str, size: u64) -> PeekResult {
    let mut out = PeekResult::default();

    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Head {
        #[serde(deserialize_with = "event::s")]
        cwd: String,
    }
    scan_head(path, |line| {
        if let Ok(probe) = serde_json::from_slice::<Head>(line) {
            if !probe.cwd.is_empty() {
                out.cwd = probe.cwd;
                return false;
            }
        }
        true
    });

    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct TailMessage {
        #[serde(deserialize_with = "event::s")]
        model: String,
        usage: Option<Usage>,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Tail {
        #[serde(rename = "type", deserialize_with = "event::s")]
        typ: String,
        #[serde(rename = "aiTitle", deserialize_with = "event::s")]
        ai_title: String,
        #[serde(rename = "customTitle", deserialize_with = "event::s")]
        custom_title: String,
        #[serde(rename = "isSidechain", deserialize_with = "event::b")]
        is_sidechain: bool,
        message: Option<TailMessage>,
    }
    let mut custom_title = String::new();
    scan_tail(path, size, |line| {
        let Ok(probe) = serde_json::from_slice::<Tail>(line) else { return true };
        if probe.typ == "custom-title" && !probe.custom_title.is_empty() {
            custom_title = probe.custom_title;
        } else if probe.typ == "ai-title" && !probe.ai_title.is_empty() {
            out.title = probe.ai_title;
        }
        if probe.typ == "assistant" && !probe.is_sidechain {
            if let Some(msg) = probe.message {
                if let Some(u) = msg.usage {
                    let used = u.input_tokens + u.cache_creation_input_tokens + u.cache_read_input_tokens;
                    if used > 0 {
                        out.context_used = used;
                        out.context_size = context_window_for(&msg.model);
                    }
                }
            }
        }
        true
    });
    if !custom_title.is_empty() {
        out.title = custom_title;
    }
    out
}

#[derive(Deserialize, Default)]
#[serde(default)]
pub struct Block {
    #[serde(rename = "type", deserialize_with = "event::s")]
    typ: String,
    #[serde(deserialize_with = "event::s")]
    text: String,
    #[serde(deserialize_with = "event::s")]
    thinking: String,
    #[serde(deserialize_with = "event::s")]
    id: String,
    #[serde(deserialize_with = "event::s")]
    name: String,
    #[serde(deserialize_with = "event::raw")]
    input: Option<Box<RawValue>>,
    #[serde(deserialize_with = "event::s")]
    tool_use_id: String,
    #[serde(deserialize_with = "event::b")]
    is_error: bool,
    #[serde(deserialize_with = "event::raw")]
    content: Option<Box<RawValue>>,
    source: Option<ImageSource>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
pub struct ImageSource {
    #[serde(deserialize_with = "event::s")]
    url: String,
    #[serde(deserialize_with = "event::s")]
    media_type: String,
    #[serde(deserialize_with = "event::s")]
    data: String,
}

// A message's content: a bare string or an array of blocks. Any other shape
// yields no blocks rather than an error. Hand-written because an untagged
// enum buffers the input, which RawValue fields cannot be read through.
#[derive(Default)]
enum Content {
    Text(String),
    Blocks(Vec<Block>),
    #[default]
    Other,
}

impl<'de> Deserialize<'de> for Content {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> serde::de::Visitor<'de> for V {
            type Value = Content;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("string or array of blocks")
            }
            fn visit_str<E: serde::de::Error>(self, s: &str) -> Result<Content, E> {
                Ok(Content::Text(s.to_string()))
            }
            fn visit_string<E: serde::de::Error>(self, s: String) -> Result<Content, E> {
                Ok(Content::Text(s))
            }
            fn visit_seq<A: serde::de::SeqAccess<'de>>(self, mut seq: A) -> Result<Content, A::Error> {
                let mut blocks = Vec::new();
                while let Some(b) = seq.next_element::<Block>()? {
                    blocks.push(b);
                }
                Ok(Content::Blocks(blocks))
            }
            fn visit_unit<E: serde::de::Error>(self) -> Result<Content, E> {
                Ok(Content::Other)
            }
            fn visit_bool<E: serde::de::Error>(self, _: bool) -> Result<Content, E> {
                Ok(Content::Other)
            }
            fn visit_i64<E: serde::de::Error>(self, _: i64) -> Result<Content, E> {
                Ok(Content::Other)
            }
            fn visit_u64<E: serde::de::Error>(self, _: u64) -> Result<Content, E> {
                Ok(Content::Other)
            }
            fn visit_f64<E: serde::de::Error>(self, _: f64) -> Result<Content, E> {
                Ok(Content::Other)
            }
            fn visit_map<A: serde::de::MapAccess<'de>>(self, mut map: A) -> Result<Content, A::Error> {
                while map.next_entry::<serde::de::IgnoredAny, serde::de::IgnoredAny>()?.is_some() {}
                Ok(Content::Other)
            }
        }
        d.deserialize_any(V)
    }
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Message {
    #[serde(deserialize_with = "event::s")]
    model: String,
    content: Content,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Attachment {
    #[serde(rename = "type", deserialize_with = "event::s")]
    typ: String,
    #[serde(rename = "commandMode", deserialize_with = "event::s")]
    command_mode: String,
    #[serde(rename = "isMeta", deserialize_with = "event::b")]
    is_meta: bool,
    prompt: Content,
}

// Who sent a user record; mirrors Origin in agents/claude/sessions.go.
#[derive(Deserialize, Default)]
#[serde(default)]
struct Origin {
    #[serde(deserialize_with = "event::s")]
    kind: String,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Line {
    #[serde(rename = "type", deserialize_with = "event::s")]
    typ: String,
    #[serde(deserialize_with = "event::s")]
    timestamp: String,
    // A subagent's own work written in among the parent's; its own file under
    // subagents/ is read by the Go side, never here.
    #[serde(rename = "isSidechain", deserialize_with = "event::b")]
    is_sidechain: bool,
    #[serde(rename = "isMeta", deserialize_with = "event::b")]
    is_meta: bool,
    #[serde(rename = "isCompactSummary", deserialize_with = "event::b")]
    is_compact_summary: bool,
    #[serde(rename = "isApiErrorMessage", deserialize_with = "event::b")]
    is_api_error: bool,
    // Names the turn on a prompt and its tool results; mirrors PromptID in agents/claude/sessions.go.
    #[serde(rename = "promptId", deserialize_with = "event::s")]
    prompt_id: String,
    message: Option<Message>,
    attachment: Option<Attachment>,
    origin: Option<Origin>,
}

impl Provider for Claude {
    fn parse(&self, line: &[u8]) -> Vec<Event> {
        let Ok(l) = serde_json::from_slice::<Line>(line) else { return Vec::new() };
        if l.is_sidechain {
            return Vec::new();
        }
        let at = event::unix_ms(&l.timestamp);
        let (model, content) = l.message.map(|m| (m.model, m.content)).unwrap_or_default();
        let mut out = Vec::new();
        if let (Some(origin), Content::Text(text)) = (&l.origin, &content) {
            if let Some(prompt) = peer_prompt(&l.typ, &origin.kind, text) {
                out.push(Event { kind: Kind::UserMessage, text: prompt, turn_id: l.prompt_id, at, ..Default::default() });
                return out;
            }
        }
        match l.typ.as_str() {
            // What the CLI wrote in the user's place, and the compact summary.
            "user" if l.is_meta || l.is_compact_summary => return out,
            "attachment" => {
                let Some(a) = l.attachment else { return out };
                if a.typ != "queued_command" || a.is_meta {
                    return out;
                }
                let txt = match a.prompt {
                    Content::Text(t) => t.trim().to_string(),
                    Content::Blocks(b) => b
                        .into_iter()
                        .filter(|b| b.typ == "text")
                        .map(|b| b.text)
                        .collect::<Vec<_>>()
                        .join("\n")
                        .trim()
                        .to_string(),
                    Content::Other => String::new(),
                };
                if let Some(result) = task_notification_result(&txt) {
                    out.push(Event { kind: Kind::ToolResult, tool: Some(result), at, ..Default::default() });
                    return out;
                }
                if !a.command_mode.is_empty() && a.command_mode != "prompt" {
                    return out;
                }
                let prose = user_prose(&txt);
                if !prose.is_empty() {
                    out.push(Event { kind: Kind::UserMessage, text: prose, at, ..Default::default() });
                }
                return out;
            }
            // The CLI's own reply: an API error fails the turn, the rest is dropped.
            "assistant" if model == "<synthetic>" => {
                if !l.is_api_error {
                    return out;
                }
                let text = match content {
                    Content::Blocks(b) => b
                        .into_iter()
                        .filter(|b| b.typ == "text" && !b.text.is_empty())
                        .map(|b| b.text)
                        .collect::<Vec<_>>()
                        .join("\n"),
                    _ => String::new(),
                };
                let error = if text.is_empty() { "The API returned an error.".to_string() } else { text };
                out.push(Event { kind: Kind::TurnFailed, error, at, ..Default::default() });
                return out;
            }
            "user" => {
                let (text, blocks) = match content {
                    Content::Text(t) => (t, Vec::new()),
                    Content::Blocks(b) => (String::new(), b),
                    Content::Other => (String::new(), Vec::new()),
                };
                let txt = text.trim();
                if !txt.is_empty() {
                    if let Some(result) = task_notification_result(txt) {
                        out.push(Event { kind: Kind::ToolResult, tool: Some(result), at, ..Default::default() });
                        return out;
                    }
                    let prose = user_prose(txt);
                    if !prose.is_empty() {
                        out.push(Event { kind: Kind::UserMessage, text: prose, turn_id: l.prompt_id, at, ..Default::default() });
                    }
                    return out;
                }
                let mut prose: Vec<String> = Vec::new();
                let mut images: Vec<ImageSource> = Vec::new();
                for b in blocks {
                    match b.typ.as_str() {
                        "image" => images.push(b.source.unwrap_or_default()),
                        "tool_result" => out.push(Event {
                            kind: Kind::ToolResult,
                            turn_id: l.prompt_id.clone(),
                            tool: Some(ToolCall {
                                call_id: b.tool_use_id,
                                output: flatten_blocks(b.content.as_deref()),
                                is_error: b.is_error,
                                ..Default::default()
                            }),
                            ..Default::default()
                        }),
                        "text" => {
                            let p = user_prose(&b.text);
                            if !p.is_empty() {
                                prose.push(p);
                            }
                        }
                        _ => {}
                    }
                }
                // A prompt sent from the app already links each attachment.
                let mut links: Vec<String> = Vec::new();
                if !prose.iter().any(|p| p.contains("](file://")) {
                    for img in &images {
                        let link = images::image_link(links.len() + 1, &img.url, &img.media_type, &img.data);
                        if !link.is_empty() {
                            links.push(link);
                        }
                    }
                }
                let text = images::with_links(&prose.join("\n"), &links);
                if !text.is_empty() {
                    out.push(Event { kind: Kind::UserMessage, text, turn_id: l.prompt_id, ..Default::default() });
                }
            }
            "assistant" => {
                let Content::Blocks(blocks) = content else { return out };
                for b in blocks {
                    match b.typ.as_str() {
                        "text" if !b.text.is_empty() => {
                            out.push(Event { kind: Kind::Text, text: b.text, ..Default::default() })
                        }
                        "thinking" if !b.thinking.is_empty() => {
                            out.push(Event { kind: Kind::Reasoning, text: b.thinking, ..Default::default() })
                        }
                        "tool_use" => out.push(Event {
                            kind: Kind::ToolCall,
                            tool: Some(ToolCall { call_id: b.id, name: b.name, input: b.input, ..Default::default() }),
                            ..Default::default()
                        }),
                        _ => {}
                    }
                }
            }
            _ => return out,
        }
        for e in &mut out {
            e.at = at;
        }
        out
    }
}

const SYNTHETIC_USER_PREFIXES: &[&str] = &[
    "<task-notification>",
    "<local-command-caveat>",
    "<local-command-stdout>",
    "<command-name>",
    "[Request interrupted by user",
    "Base directory for this skill: ",
    "[Image: source: ",
];

const REMINDER_OPEN: &str = "<system-reminder>";
const REMINDER_CLOSE: &str = "</system-reminder>";

// Strips reminder blocks the CLI appends to a prompt; an unterminated one
// takes everything after it.
// The prompt inside an inbox message; mirrors peerPrompt in agents/claude/sessions.go.
fn peer_prompt(typ: &str, origin: &str, text: &str) -> Option<String> {
    if typ != "user" || origin != "peer" || !text.starts_with("Another Claude session sent a message") {
        return None;
    }
    let body = text.split_once('\n').map(|(_, b)| b).unwrap_or("");
    let body = body.split_once("\n\nThis came from another Claude session").map(|(b, _)| b).unwrap_or(body).trim();
    (!body.is_empty()).then(|| body.to_string())
}

fn user_prose(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(i) = rest.find(REMINDER_OPEN) {
        out.push_str(&rest[..i]);
        match rest[i + REMINDER_OPEN.len()..].find(REMINDER_CLOSE) {
            Some(j) => rest = &rest[i + REMINDER_OPEN.len() + j + REMINDER_CLOSE.len()..],
            None => {
                rest = "";
                break;
            }
        }
    }
    out.push_str(rest);
    let t = out.trim();
    if t.is_empty() || SYNTHETIC_USER_PREFIXES.iter().any(|p| t.starts_with(p)) {
        return String::new();
    }
    t.to_string()
}

// A background agent's final report, from the notice the CLI writes when it
// finishes; the Go side folds it onto the call that launched the agent. Mirrors
// taskNotificationResult in agents/claude/sessions.go.
fn task_notification_result(s: &str) -> Option<ToolCall> {
    if !s.starts_with("<task-notification>") {
        return None;
    }
    let call_id = tag_text(s, "tool-use-id");
    let result = tag_text(s, "result");
    if call_id.is_empty() || result.is_empty() {
        return None;
    }
    let status = tag_text(s, "status");
    Some(ToolCall {
        call_id,
        output: result,
        is_error: !status.is_empty() && status != "completed",
        ..Default::default()
    })
}

// The text of the first <tag>…</tag> in s, trimmed; a report's end is its
// last closing tag, since it can quote tags of its own.
fn tag_text(s: &str, tag: &str) -> String {
    let start = format!("<{tag}>");
    let end = format!("</{tag}>");
    let Some(i) = s.find(&start) else { return String::new() };
    let rest = &s[i + start.len()..];
    let j = if tag == "result" { rest.rfind(&end) } else { rest.find(&end) };
    match j {
        Some(j) => rest[..j].trim().to_string(),
        None => String::new(),
    }
}

pub fn flatten_blocks(raw: Option<&RawValue>) -> String {
    let Some(raw) = raw else { return String::new() };
    let text = raw.get();
    if text == "null" {
        return String::new();
    }
    if let Ok(s) = serde_json::from_str::<String>(text) {
        return s;
    }
    if let Ok(blocks) = serde_json::from_str::<Vec<Block>>(text) {
        return blocks
            .into_iter()
            .filter_map(|b| match b.typ.as_str() {
                "text" => Some(b.text),
                "image" => Some("[image]".to_string()),
                _ => None,
            })
            .collect();
    }
    text.to_string()
}
