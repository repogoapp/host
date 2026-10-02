use std::time::{SystemTime, UNIX_EPOCH};

use rusqlite::{params, Connection, OptionalExtension};

use repogo_import::event::{Entry, Event, Kind, Meta};

// The host's own schema, so a database written here opens in the host.
const SCHEMA: &str = include_str!("../../../../store/schema.sql");

const SESSION_UPSERT: &str = "
INSERT INTO sessions (agent, session_id, cwd, title, last_reply, path, created_at, updated_at, size_bytes, event_count, content_hash, generation, synced_at, context_used, context_size)
VALUES (?,?,?,?,?,?,?,?,?,?,?,1,?,?,?)
ON CONFLICT(agent, session_id) DO UPDATE SET
  cwd = excluded.cwd, title = excluded.title, last_reply = excluded.last_reply, path = excluded.path,
  created_at = CASE WHEN sessions.created_at > 0 THEN sessions.created_at ELSE excluded.created_at END,
  updated_at = excluded.updated_at, size_bytes = excluded.size_bytes,
  event_count = excluded.event_count, content_hash = excluded.content_hash,
  synced_at = excluded.synced_at,
  context_used = CASE WHEN excluded.context_used > 0 THEN excluded.context_used ELSE sessions.context_used END,
  context_size = CASE WHEN excluded.context_size > 0 THEN excluded.context_size ELSE sessions.context_size END,
  generation = sessions.generation + ?
RETURNING sid";

pub fn open(path: &str) -> rusqlite::Result<Connection> {
    let db = Connection::open(path)?;
    db.execute_batch(
        "PRAGMA journal_mode=WAL;
         PRAGMA synchronous=NORMAL;
         PRAGMA busy_timeout=5000;
         PRAGMA temp_store=MEMORY;
         PRAGMA cache_size=-64000;",
    )?;
    db.execute_batch(SCHEMA)?;
    db.set_prepared_statement_cache_capacity(256);
    Ok(db)
}

// FNV-1a over the fields that make up a chat's content, with a separator
// after each field. prefix is the hash of the first prefix_len events, which
// tells an append from a rewrite.
pub fn hash_events(events: &[Event], prefix_len: usize) -> (u64, u64) {
    const OFFSET: u64 = 14695981039346656037;
    const PRIME: u64 = 1099511628211;
    fn mix(h: &mut u64, s: &[u8]) {
        for &b in s {
            *h ^= b as u64;
            *h = h.wrapping_mul(PRIME);
        }
        *h ^= 0xff;
        *h = h.wrapping_mul(PRIME);
    }
    let mut h = OFFSET;
    let mut prefix = OFFSET;
    for (i, e) in events.iter().enumerate() {
        if i == prefix_len {
            prefix = h;
        }
        mix(&mut h, e.kind.as_str().as_bytes());
        mix(&mut h, e.turn_id.as_bytes());
        mix(&mut h, e.text.as_bytes());
        if let Some(t) = &e.tool {
            mix(&mut h, t.call_id.as_bytes());
            mix(&mut h, t.name.as_bytes());
            mix(&mut h, t.input.as_deref().map(|r| r.get()).unwrap_or("").as_bytes());
            mix(&mut h, t.output.as_bytes());
        }
    }
    if prefix_len >= events.len() {
        prefix = h;
    }
    (h, prefix)
}

pub fn sync_batch(db: &mut Connection, entries: &[Entry]) -> rusqlite::Result<()> {
    let tx = db.transaction()?;
    let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_millis() as i64;
    for e in entries {
        let (old_count, old_hash, turn_key, turn_started, turn_finished): (i64, i64, String, Option<i64>, Option<i64>) = tx
            .prepare_cached(
                "SELECT event_count, content_hash, last_turn_key, last_turn_started_at, last_turn_finished_at
                 FROM sessions WHERE agent = ? AND session_id = ?",
            )?
            .query_row(params![e.meta.agent.unwrap().as_str(), e.meta.id], |r| {
                Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?, r.get(4)?))
            })
            .optional()?
            .unwrap_or_default();

        let (full, prefix) = hash_events(&e.events, old_count as usize);
        let bump = if old_count > e.events.len() as i64 || prefix != old_hash as u64 { 1 } else { 0 };

        let sid: i64 = tx.prepare_cached(SESSION_UPSERT)?.query_row(
            params![
                e.meta.agent.unwrap().as_str(),
                e.meta.id,
                e.meta.cwd,
                title_for(&e.meta, &e.events),
                last_reply(&e.events),
                e.meta.path,
                created_at(&e.meta, &e.events),
                e.meta.updated_at_ms,
                e.meta.size_bytes,
                e.events.len() as i64,
                full as i64,
                now,
                e.meta.context_used,
                e.meta.context_size,
                bump,
            ],
            |r| r.get(0),
        )?;
        tx.prepare_cached("DELETE FROM events WHERE sid = ?")?.execute([sid])?;
        insert_events(&tx, sid, &e.events)?;

        let (key, started, finished) = merge_turn(turn_key.clone(), turn_started, turn_finished, transcript_turn(&e.events));
        if key != turn_key || started != turn_started || finished != turn_finished {
            tx.prepare_cached(
                "UPDATE sessions SET last_turn_key = ?, last_turn_started_at = ?, last_turn_finished_at = ? WHERE sid = ?",
            )?
            .execute(params![key, started, finished, sid])?;
        }
        if let Some((status, at)) = transcript_status(&e.events) {
            tx.prepare_cached("UPDATE sessions SET status = ?, status_at = ? WHERE sid = ? AND status_at < ?")?
                .execute(params![status, at, sid, at])?;
        }
    }
    tx.commit()
}

const EVENTS_PER_STATEMENT: usize = 128;

fn insert_events(tx: &Connection, sid: i64, events: &[Event]) -> rusqlite::Result<()> {
    for (chunk_i, chunk) in events.chunks(EVENTS_PER_STATEMENT).enumerate() {
        let mut sql = String::from("INSERT INTO events (sid, idx, kind, turn, text, tool, at) VALUES ");
        for i in 0..chunk.len() {
            if i > 0 {
                sql.push(',');
            }
            sql.push_str("(?,?,?,?,?,?,?)");
        }
        let tools: Vec<Option<String>> =
            chunk.iter().map(|e| e.tool.as_ref().and_then(|t| serde_json::to_string(t).ok())).collect();
        let idxs: Vec<i64> = (0..chunk.len()).map(|i| (chunk_i * EVENTS_PER_STATEMENT + i) as i64).collect();
        let kinds: Vec<&str> = chunk.iter().map(|e| e.kind.as_str()).collect();
        let mut args: Vec<&dyn rusqlite::ToSql> = Vec::with_capacity(chunk.len() * 7);
        for (i, e) in chunk.iter().enumerate() {
            args.push(&sid);
            args.push(&idxs[i]);
            args.push(&kinds[i]);
            args.push(if e.turn_id.is_empty() { &rusqlite::types::Null } else { &e.turn_id });
            args.push(if !e.text.is_empty() {
                &e.text
            } else if !e.error.is_empty() {
                &e.error
            } else {
                &rusqlite::types::Null
            });
            args.push(match &tools[i] {
                Some(t) => t,
                None => &rusqlite::types::Null,
            });
            args.push(if e.at == 0 { &rusqlite::types::Null } else { &e.at });
        }
        tx.prepare_cached(&sql)?.execute(args.as_slice())?;
    }
    Ok(())
}

const TITLE_WORDS: usize = 5;
const TITLE_MAX_CHARS: usize = 60;
const REPLY_MAX_CHARS: usize = 240;

fn collapse_space(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

fn title_for(meta: &Meta, events: &[Event]) -> String {
    let t = collapse_space(&meta.title);
    if !t.is_empty() {
        return t;
    }
    events
        .iter()
        .filter(|e| e.kind == Kind::UserMessage)
        .map(|e| opening_words(&e.text))
        .find(|t| !t.is_empty())
        .unwrap_or_default()
}

fn opening_words(text: &str) -> String {
    let words: Vec<&str> = text.split_whitespace().collect();
    if words.is_empty() {
        return String::new();
    }
    let mut truncated = words.len() > TITLE_WORDS;
    let mut out = words[..words.len().min(TITLE_WORDS)].join(" ");
    if out.chars().count() > TITLE_MAX_CHARS {
        out = out.chars().take(TITLE_MAX_CHARS).collect::<String>().trim_end_matches(' ').to_string();
        truncated = true;
    }
    if truncated {
        out.push('…');
    }
    out
}

fn last_reply(events: &[Event]) -> String {
    events
        .iter()
        .rev()
        .filter(|e| e.kind == Kind::Text)
        .map(|e| preview_of(&e.text))
        .find(|t| !t.is_empty())
        .unwrap_or_default()
}

fn preview_of(text: &str) -> String {
    // One pass like strings.Replacer: a "**" removed must not expose a "__".
    let mut stripped = String::with_capacity(text.len());
    let mut rest = text;
    while !rest.is_empty() {
        if let Some(r) = rest.strip_prefix("**").or_else(|| rest.strip_prefix("__")).or_else(|| rest.strip_prefix('`')) {
            rest = r;
            continue;
        }
        let c = rest.chars().next().unwrap();
        stripped.push(c);
        rest = &rest[c.len_utf8()..];
    }
    let text = stripped;
    let lines: Vec<&str> = text
        .split('\n')
        .map(|l| l.trim_start_matches(|c| " \t#>-*•".contains(c)))
        .filter(|l| !l.is_empty())
        .collect();
    let out = collapse_space(&lines.join(" "));
    if out.chars().count() > REPLY_MAX_CHARS {
        return out.chars().take(REPLY_MAX_CHARS).collect::<String>().trim_end_matches(' ').to_string() + "…";
    }
    out
}

fn created_at(meta: &Meta, events: &[Event]) -> i64 {
    events.iter().find(|e| e.at > 0).map(|e| e.at).unwrap_or(meta.updated_at_ms)
}

fn transcript_status(events: &[Event]) -> Option<(&'static str, i64)> {
    let mut status = "";
    let mut at = 0;
    for e in events {
        status = match e.kind {
            Kind::TurnStarted => "working",
            Kind::TurnFinished => "idle",
            Kind::TurnFailed if e.error == "aborted" => "interrupted",
            Kind::TurnFailed => "failed",
            _ => continue,
        };
        at = e.at;
    }
    if status.is_empty() || at == 0 {
        return None;
    }
    Some((status, at))
}

struct Turn {
    key: String,
    started_at: Option<i64>,
    finished_at: Option<i64>,
}

const TRANSCRIPT_PREFIX: &str = "transcript:";

fn transcript_turn(events: &[Event]) -> Turn {
    let empty = Turn { key: String::new(), started_at: None, finished_at: None };
    let mut start = events.iter().rposition(|e| e.kind == Kind::TurnStarted);
    let lifecycle = start.is_some();
    if start.is_none() {
        start = events.iter().rposition(|e| e.kind == Kind::UserMessage);
    }
    let Some(start) = start else { return empty };
    if events[start].at == 0 {
        return empty;
    }
    let mut finished = 0;
    for e in &events[start + 1..] {
        if e.at == 0 {
            continue;
        }
        match e.kind {
            Kind::TurnFinished | Kind::TurnFailed => finished = e.at,
            Kind::Text | Kind::Reasoning | Kind::ToolCall | Kind::ToolResult if !lifecycle => finished = e.at,
            _ => {}
        }
    }
    Turn {
        key: format!("{TRANSCRIPT_PREFIX}{}", events[start].at),
        started_at: Some(events[start].at),
        finished_at: if finished > 0 { Some(finished) } else { None },
    }
}

// Only the from-file branch of the Go mergeTurn: a live observation on the
// row wins over the transcript, otherwise the transcript's reading replaces
// the last one whole.
fn merge_turn(key: String, started: Option<i64>, finished: Option<i64>, turn: Turn) -> (String, Option<i64>, Option<i64>) {
    if turn.key.is_empty() || (turn.started_at.is_none() && turn.finished_at.is_none()) {
        return (key, started, finished);
    }
    if !key.is_empty() && !key.starts_with(TRANSCRIPT_PREFIX) {
        return (key, started, finished);
    }
    (turn.key, turn.started_at, turn.finished_at)
}
