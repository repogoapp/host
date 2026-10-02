use std::collections::HashMap;
use std::fs;
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};

use rayon::prelude::*;
use serde::Deserialize;
use serde_json::value::RawValue;

use crate::event::{self, Agent, Event, Kind, Meta, ToolCall};
use crate::home::codex_home;
use crate::images;
use crate::peek::{scan_head, scan_tail, PeekResult};
use crate::reader::Provider;

pub struct Codex;

pub fn list() -> std::io::Result<Vec<Meta>> {
    let root = codex_home();
    let sessions = root.join("sessions");
    if !sessions.exists() {
        return Ok(Vec::new());
    }
    let titles = titles(&root.join("session_index.jsonl"));

    let mut files: Vec<(String, fs::Metadata)> = Vec::new();
    walk(&sessions, &mut files);

    let peeked: Vec<(PeekResult, &String, &fs::Metadata)> = files
        .par_iter()
        .map(|(path, info)| (peek(path, info.len()), path, info))
        .collect();

    let mut by_id: HashMap<String, Meta> = HashMap::new();
    let mut order: Vec<String> = Vec::new();
    for (p, path, info) in peeked {
        if p.id.is_empty() {
            continue;
        }
        let m = by_id.entry(p.id.clone()).or_insert_with(|| {
            order.push(p.id.clone());
            Meta {
                id: p.id.clone(),
                agent: Some(Agent::Codex),
                cwd: p.cwd.clone(),
                title: titles.get(&p.id).cloned().unwrap_or_default(),
                ..Default::default()
            }
        });
        m.paths.push(path.clone());
        m.size_bytes += info.len() as i64;
        let (ms, ns) = crate::mtime(info);
        if ns > m.updated_at_ns {
            m.updated_at_ns = ns;
            m.updated_at_ms = ms;
            m.path = path.clone();
            // Context comes from the newest segment only.
            if p.context_used > 0 {
                m.context_used = p.context_used;
                m.context_size = p.context_size;
            }
        }
    }

    Ok(order
        .into_iter()
        .map(|id| {
            let mut m = by_id.remove(&id).unwrap();
            m.paths.sort();
            m
        })
        .collect())
}

// Lexical walk order, the same as filepath.WalkDir.
fn walk(dir: &Path, out: &mut Vec<(String, fs::Metadata)>) {
    let Ok(rd) = fs::read_dir(dir) else { return };
    let mut entries: Vec<PathBuf> = rd.flatten().map(|e| e.path()).collect();
    entries.sort();
    for p in entries {
        if p.is_dir() {
            walk(&p, out);
            continue;
        }
        let Some(name) = p.file_name().and_then(|n| n.to_str()) else { continue };
        if !name.ends_with(".jsonl") {
            continue;
        }
        if let Ok(info) = p.metadata() {
            out.push((p.to_string_lossy().into_owned(), info));
        }
    }
}

fn titles(path: &Path) -> HashMap<String, String> {
    let mut out = HashMap::new();
    let Ok(f) = fs::File::open(path) else { return out };
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Row {
        #[serde(deserialize_with = "event::s")]
        id: String,
        #[serde(deserialize_with = "event::s")]
        thread_name: String,
    }
    for line in BufReader::new(f).split(b'\n').flatten() {
        if let Ok(row) = serde_json::from_slice::<Row>(&line) {
            if !row.id.is_empty() && !row.thread_name.is_empty() {
                out.insert(row.id, row.thread_name);
            }
        }
    }
    out
}

fn peek(path: &str, size: u64) -> PeekResult {
    let mut out = PeekResult::default();

    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct HeadPayload {
        #[serde(deserialize_with = "event::s")]
        session_id: String,
        #[serde(deserialize_with = "event::s")]
        cwd: String,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Head {
        #[serde(rename = "type", deserialize_with = "event::s")]
        typ: String,
        payload: Option<HeadPayload>,
    }
    scan_head(path, |line| {
        let Ok(l) = serde_json::from_slice::<Head>(line) else { return true };
        if l.typ == "session_meta" {
            let p = l.payload.unwrap_or_default();
            out.id = p.session_id;
            out.cwd = p.cwd;
            return false;
        }
        true
    });

    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Last {
        #[serde(deserialize_with = "event::i")]
        input_tokens: i64,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Info {
        last_token_usage: Option<Last>,
        #[serde(deserialize_with = "event::i")]
        model_context_window: i64,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct TailPayload {
        #[serde(rename = "type", deserialize_with = "event::s")]
        typ: String,
        info: Option<Info>,
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Tail {
        #[serde(rename = "type", deserialize_with = "event::s")]
        typ: String,
        payload: Option<TailPayload>,
    }
    scan_tail(path, size, |line| {
        let Ok(l) = serde_json::from_slice::<Tail>(line) else { return true };
        if l.typ != "event_msg" {
            return true;
        }
        let Some(p) = l.payload else { return true };
        if p.typ != "token_count" {
            return true;
        }
        if let Some(info) = p.info {
            let used = info.last_token_usage.map(|l| l.input_tokens).unwrap_or(0);
            if used > 0 {
                out.context_used = used;
                out.context_size = info.model_context_window;
            }
        }
        true
    });
    out
}

const SYNTHETIC_USER_PREFIXES: &[&str] = &[
    "<codex_internal_context>",
    "<codex_internal_context ",
    "<skills_instructions>",
    "<multi_agent_mode>",
    "<multi_agent_role>",
    "<recommended_plugins>",
    "<permissions instructions>",
    "<environment_context>",
    "<user_instructions>",
    "# AGENTS.md instructions for ",
    "The following is the Codex agent history",
    "You are `/root`, the primary agent",
    "{\"risk_level\":",
    "<turn_aborted>",
    "<subagent_notification>",
    "<realtime_delegation>",
    "<no retained transcript delta entries>",
];

#[derive(Deserialize, Default)]
#[serde(default)]
struct Part {
    #[serde(rename = "type", deserialize_with = "event::s")]
    typ: String,
    #[serde(deserialize_with = "event::s")]
    text: String,
    #[serde(deserialize_with = "event::s")]
    image_url: String,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Summary {
    #[serde(deserialize_with = "event::s")]
    text: String,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Goal {
    #[serde(deserialize_with = "event::s")]
    objective: String,
    #[serde(rename = "createdAt", deserialize_with = "event::i")]
    created_at: i64,
    #[serde(rename = "updatedAt", deserialize_with = "event::i")]
    updated_at: i64,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Payload {
    #[serde(rename = "type", deserialize_with = "event::s")]
    typ: String,
    goal: Option<Goal>,
    #[serde(deserialize_with = "event::s")]
    message: String,
    #[serde(deserialize_with = "event::s")]
    turn_id: String,
    #[serde(deserialize_with = "event::s")]
    role: String,
    #[serde(deserialize_with = "event::v")]
    content: Vec<Part>,
    #[serde(deserialize_with = "event::s")]
    call_id: String,
    #[serde(deserialize_with = "event::s")]
    name: String,
    #[serde(deserialize_with = "event::raw")]
    input: Option<Box<RawValue>>,
    #[serde(deserialize_with = "event::s")]
    arguments: String,
    #[serde(deserialize_with = "event::raw")]
    output: Option<Box<RawValue>>,
    #[serde(deserialize_with = "event::v")]
    summary: Vec<Summary>,
    // web_search_call
    #[serde(deserialize_with = "event::s")]
    id: String,
    #[serde(deserialize_with = "event::s")]
    status: String,
    #[serde(deserialize_with = "event::raw")]
    action: Option<Box<RawValue>>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Line {
    #[serde(deserialize_with = "event::s")]
    timestamp: String,
    #[serde(rename = "type", deserialize_with = "event::s")]
    typ: String,
    payload: Option<Payload>,
}

impl Provider for Codex {
    fn parse(&self, line: &[u8]) -> Vec<Event> {
        let Ok(l) = serde_json::from_slice::<Line>(line) else { return Vec::new() };
        let at = event::unix_ms(&l.timestamp);
        let p = l.payload.unwrap_or_default();
        let mut e = match (l.typ.as_str(), p.typ.as_str()) {
            ("event_msg", "task_started") => Event { kind: Kind::TurnStarted, turn_id: p.turn_id, ..Default::default() },
            ("event_msg", "task_complete") => Event { kind: Kind::TurnFinished, turn_id: p.turn_id, ..Default::default() },
            ("event_msg", "turn_aborted") => {
                Event { kind: Kind::TurnFailed, turn_id: p.turn_id, error: "aborted".into(), ..Default::default() }
            }
            ("event_msg", "error") => Event {
                kind: Kind::TurnFailed,
                turn_id: p.turn_id,
                error: if p.message.is_empty() { "codex reported an error".into() } else { p.message },
                ..Default::default()
            },
            // Mirrors the thread_goal_updated case in agents/codex/sessions.go.
            ("event_msg", "thread_goal_updated") => {
                let g = p.goal.unwrap_or_default();
                let text = g.objective.trim();
                if text.is_empty() || g.updated_at != g.created_at {
                    return Vec::new();
                }
                Event { kind: Kind::UserMessage, text: text.to_string(), ..Default::default() }
            }
            ("response_item", "message") => {
                let kind = match p.role.as_str() {
                    "user" => Kind::UserMessage,
                    "assistant" => Kind::Text,
                    _ => return Vec::new(),
                };
                let parts: Vec<&str> = p
                    .content
                    .iter()
                    .filter(|c| matches!(c.typ.as_str(), "input_text" | "output_text" | "text"))
                    .map(|c| c.text.as_str())
                    .collect();
                // Assistant parts are chunks of one text; a user's are items.
                let value = if kind == Kind::UserMessage {
                    let joined = join_lines(&parts);
                    if !joined.is_empty() && SYNTHETIC_USER_PREFIXES.iter().any(|pre| joined.starts_with(pre)) {
                        return Vec::new();
                    }
                    user_prompt(&p.content)
                } else {
                    strip_directives(&parts.concat())
                };
                if value.is_empty() {
                    return Vec::new();
                }
                Event { kind, text: value, turn_id: p.turn_id, ..Default::default() }
            }
            ("response_item", "custom_tool_call" | "function_call") => Event {
                kind: Kind::ToolCall,
                tool: Some(ToolCall {
                    call_id: p.call_id,
                    name: p.name,
                    input: raw_or_string(p.input, &p.arguments),
                    ..Default::default()
                }),
                ..Default::default()
            },
            ("response_item", "custom_tool_call_output" | "function_call_output") => Event {
                kind: Kind::ToolResult,
                tool: Some(ToolCall {
                    call_id: p.call_id,
                    output: flatten_output(p.output.as_deref()),
                    ..Default::default()
                }),
                ..Default::default()
            },
            ("response_item", "web_search_call") => {
                let mut events = web_search(&p.id, &p.status, p.action.as_deref(), &l.timestamp);
                for e in &mut events {
                    e.at = at;
                }
                return events;
            }
            ("response_item", "reasoning") => {
                let text: String = p.summary.iter().map(|s| s.text.as_str()).collect();
                if text.is_empty() {
                    return Vec::new();
                }
                Event { kind: Kind::Reasoning, text, ..Default::default() }
            }
            _ => return Vec::new(),
        };
        e.at = at;
        vec![e]
    }
}

// Remark leaf directives such as `::inbox-item{title="…"}` are markup the
// Codex app renders as its own UI.
fn strip_directives(text: &str) -> String {
    if !text.contains("::") {
        return text.trim().to_string();
    }
    text.split('\n').filter(|line| !is_directive(line.trim())).collect::<Vec<_>>().join("\n").trim().to_string()
}

fn is_directive(line: &str) -> bool {
    let Some(rest) = line.strip_prefix("::") else { return false };
    let Some((name, _)) = rest.split_once('{') else { return false };
    !name.is_empty() && line.ends_with('}') && name.bytes().all(|b| b.is_ascii_lowercase() || b == b'-')
}

fn join_lines(parts: &[&str]) -> String {
    parts.iter().map(|s| s.trim()).filter(|s| !s.is_empty()).collect::<Vec<_>>().join("\n")
}

// Mirrors codexUserPrompt in agents/codex/sessions.go.
fn user_prompt(content: &[Part]) -> String {
    let mut texts: Vec<String> = Vec::new();
    let mut links: Vec<String> = Vec::new();
    let mut image_urls: Vec<&str> = Vec::new();
    let mut linked = false;
    for part in content {
        match part.typ.as_str() {
            "input_text" | "text" => {
                if is_image_tag(&part.text) {
                    continue;
                }
                let (text, files) = unwrap_request(&part.text);
                linked = linked || !files.is_empty() || text.contains("](file://");
                texts.push(text);
                links.extend(files);
            }
            "input_image" => image_urls.push(&part.image_url),
            _ => {}
        }
    }
    if !linked {
        for url in image_urls {
            let link = images::image_link(links.len() + 1, url, "", "");
            if !link.is_empty() {
                links.push(link);
            }
        }
    }
    let refs: Vec<&str> = texts.iter().map(|s| s.as_str()).collect();
    images::with_links(&join_lines(&refs), &links).trim().to_string()
}

fn is_image_tag(s: &str) -> bool {
    let s = s.trim();
    s == "<image>" || s == "</image>" || (s.starts_with("<image name=") && s.ends_with('>'))
}

const REQUEST_HEADINGS: [&str; 2] = ["## My request for Codex:", "## My request:"];

// Mirrors unwrapCodexRequest in agents/codex/sessions.go.
fn unwrap_request(s: &str) -> (String, Vec<String>) {
    let trimmed = s.trim();
    if !trimmed.starts_with("# Context from my IDE setup:") && !trimmed.starts_with("# Files mentioned by the user:") {
        return (s.to_string(), Vec::new());
    }
    let Some((head, request)) = REQUEST_HEADINGS.iter().find_map(|h| trimmed.split_once(h)) else {
        return (s.to_string(), Vec::new());
    };
    let mut links = Vec::new();
    let mut in_files = false;
    for line in head.split('\n') {
        if line.starts_with("# ") {
            in_files = line.starts_with("# Files mentioned by the user:");
        } else if in_files && line.starts_with("## ") {
            if let Some((name, path)) = line["## ".len()..].split_once(": ") {
                let path = path.trim();
                if !name.is_empty() && path.starts_with('/') {
                    links.push(format!("[@{name}](file://{path})"));
                }
            }
        }
    }
    (request.trim().to_string(), links)
}

// Mirrors codexWebSearch in agents/codex/sessions.go.
fn web_search(id: &str, status: &str, action: Option<&RawValue>, timestamp: &str) -> Vec<Event> {
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Action {
        #[serde(rename = "type", deserialize_with = "event::s")]
        typ: String,
    }
    let raw = action.map(|a| a.get()).unwrap_or("");
    let typ = serde_json::from_str::<Action>(raw).map(|a| a.typ).unwrap_or_default();
    let name = if typ == "open_page" || typ == "find_in_page" { "web_fetch" } else { "web_search" };
    let call_id = if id.is_empty() { format!("ws:{timestamp}:{:016x}", images::fnv64(raw)) } else { id.to_string() };
    let input = if raw.starts_with('{') { RawValue::from_string(raw.to_string()).ok() } else { None };
    let mut events = vec![Event {
        kind: Kind::ToolCall,
        tool: Some(ToolCall { call_id: call_id.clone(), name: name.to_string(), input, ..Default::default() }),
        ..Default::default()
    }];
    if status == "completed" || status == "failed" {
        events.push(Event {
            kind: Kind::ToolResult,
            tool: Some(ToolCall { call_id, name: name.to_string(), is_error: status == "failed", ..Default::default() }),
            ..Default::default()
        });
    }
    events
}

fn raw_or_string(input: Option<Box<RawValue>>, arguments: &str) -> Option<Box<RawValue>> {
    if input.is_some() {
        return input;
    }
    if arguments.is_empty() {
        return None;
    }
    RawValue::from_string(crate::gojson::string(arguments)).ok()
}

// Output is a bare string on some tools and an object with a nested field on
// others.
fn flatten_output(raw: Option<&RawValue>) -> String {
    let Some(raw) = raw else { return String::new() };
    let text = raw.get();
    if text == "null" {
        return String::new();
    }
    if let Ok(s) = serde_json::from_str::<String>(text) {
        // Computer-use tools answer with a JSON-encoded string of parts, and
        // a screenshot part is megabytes of base64 no transcript row can carry.
        if s.starts_with('[') {
            if let Some(parts) = flatten_parts(&s) {
                return parts;
            }
        }
        return s;
    }
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Obj {
        #[serde(deserialize_with = "event::s")]
        output: String,
        #[serde(deserialize_with = "event::s")]
        content: String,
        #[serde(deserialize_with = "event::s")]
        text: String,
    }
    if let Ok(obj) = serde_json::from_str::<Obj>(text) {
        for v in [obj.output, obj.content, obj.text] {
            if !v.is_empty() {
                return v;
            }
        }
    }
    if let Some(parts) = flatten_parts(text) {
        return parts;
    }
    text.to_string()
}

fn flatten_parts(text: &str) -> Option<String> {
    #[derive(Deserialize, Default)]
    #[serde(default)]
    struct Part {
        #[serde(rename = "type")]
        kind: String,
        #[serde(deserialize_with = "event::s")]
        text: String,
    }
    let parts = serde_json::from_str::<Vec<Part>>(text).ok()?;
    if parts.is_empty() {
        return None;
    }
    let out: Vec<&str> = parts
        .iter()
        .filter_map(|p| match p.kind.as_str() {
            "input_image" => Some("[image]"),
            _ if !p.text.is_empty() => Some(p.text.as_str()),
            _ => None,
        })
        .collect();
    Some(out.join("\n"))
}
