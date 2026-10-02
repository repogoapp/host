// The C ABI the Go host links. One call parses one transcript segment into
// the same events the Go parser produces, returned as one flat buffer so the
// boundary is crossed once per file rather than once per event. Go stamps
// Seq/SessionID itself.
use std::ffi::CStr;
use std::os::raw::c_char;

use crate::event::{Event, Kind};
use crate::reader::{read_file, Provider};

pub const REPOGO_AGENT_CLAUDE: u32 = 0;
pub const REPOGO_AGENT_CODEX: u32 = 1;

// A JSON boundary measured no faster than parsing the transcript itself: the
// event text is most of the bytes, and encoding it then decoding it again
// costs what the original parse did. So the layout is flat and little-endian,
// and the Go side only slices:
//
//   u32 count
//   per event: u8 kind, i64 at, u8 has_tool, u8 is_error,
//              then strings turn_id, text, error, call_id, name, input, output
//   string:    u32 len, bytes   (len 0xFFFFFFFF = absent, used for tool.input)
//
// Kind is its position in agent.EventKind's list; see native.go.
const NONE: u32 = u32::MAX;

fn kind_code(k: Kind) -> u8 {
    match k {
        Kind::TurnStarted => 0,
        Kind::UserMessage => 1,
        Kind::Text => 2,
        Kind::Reasoning => 3,
        Kind::ToolCall => 4,
        Kind::ToolResult => 5,
        Kind::TurnFinished => 6,
        Kind::TurnFailed => 7,
    }
}

fn put_str(out: &mut Vec<u8>, s: &str) {
    out.extend_from_slice(&(s.len() as u32).to_le_bytes());
    out.extend_from_slice(s.as_bytes());
}

fn encode(events: &[Event]) -> Vec<u8> {
    let size: usize = events.iter().map(|e| 40 + e.turn_id.len() + e.text.len() + e.error.len()
        + e.tool.as_ref().map(|t| t.call_id.len() + t.name.len() + t.output.len() + t.input.as_deref().map(|r| r.get().len()).unwrap_or(0)).unwrap_or(0)).sum();
    let mut out = Vec::with_capacity(4 + size);
    out.extend_from_slice(&(events.len() as u32).to_le_bytes());
    for e in events {
        out.push(kind_code(e.kind));
        out.extend_from_slice(&e.at.to_le_bytes());
        out.push(e.tool.is_some() as u8);
        out.push(e.tool.as_ref().map(|t| t.is_error).unwrap_or(false) as u8);
        put_str(&mut out, &e.turn_id);
        put_str(&mut out, &e.text);
        put_str(&mut out, &e.error);
        match &e.tool {
            Some(t) => {
                put_str(&mut out, &t.call_id);
                put_str(&mut out, &t.name);
                match &t.input {
                    Some(raw) => put_str(&mut out, raw.get()),
                    None => out.extend_from_slice(&NONE.to_le_bytes()),
                }
                put_str(&mut out, &t.output);
            }
            None => {
                put_str(&mut out, "");
                put_str(&mut out, "");
                out.extend_from_slice(&NONE.to_le_bytes());
                put_str(&mut out, "");
            }
        }
    }
    out
}

/// Parses every complete line of `path` for the given agent. Returns a buffer
/// in the layout above that the caller releases with `repogo_free`, writing its
/// length to `out_len`. Returns null when the file cannot be read; the caller
/// then falls back to its own parser. Never panics across the boundary.
///
/// # Safety
/// `path` and `session_id` must be valid NUL-terminated strings and `out_len`
/// a valid pointer.
#[no_mangle]
pub unsafe extern "C" fn repogo_parse_file(agent: u32, path: *const c_char, session_id: *const c_char, out_len: *mut usize) -> *mut u8 {
    let result = std::panic::catch_unwind(|| {
        let path = CStr::from_ptr(path).to_str().ok()?;
        let session_id = CStr::from_ptr(session_id).to_str().ok()?;
        let provider: &dyn Provider = match agent {
            REPOGO_AGENT_CLAUDE => &crate::claude::Claude,
            REPOGO_AGENT_CODEX => &crate::codex::Codex,
            _ => return None,
        };
        let mut events = Vec::new();
        read_file(path, provider, session_id, &mut events).ok()?;
        Some(encode(&events))
    });
    match result {
        Ok(Some(buf)) => {
            let boxed = buf.into_boxed_slice();
            *out_len = boxed.len();
            Box::into_raw(boxed) as *mut u8
        }
        _ => {
            *out_len = 0;
            std::ptr::null_mut()
        }
    }
}

/// Releases a buffer returned by `repogo_parse_file` or `repogo_scan_usage`.
///
/// # Safety
/// `ptr`/`len` must be exactly what `repogo_parse_file` or `repogo_scan_usage`
/// returned, released once.
#[no_mangle]
pub unsafe extern "C" fn repogo_free(ptr: *mut u8, len: usize) {
    if !ptr.is_null() {
        drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len)));
    }
}

// Usage records, flat and little-endian like the events:
//
//   u32 count
//   per record: strings key, session, model; i64 at, uncached, cached,
//               creation, creation_1h, output, reasoning; u8 fast,
//               u8 has_reported, f64 reported_usd
fn encode_usage(records: &[crate::usage::Record]) -> Vec<u8> {
    let mut out = Vec::with_capacity(4 + records.len() * 120);
    out.extend_from_slice(&(records.len() as u32).to_le_bytes());
    for r in records {
        put_str(&mut out, &r.key);
        put_str(&mut out, &r.session);
        put_str(&mut out, &r.model);
        for n in [r.at, r.uncached, r.cached, r.creation, r.creation_1h, r.output, r.reasoning] {
            out.extend_from_slice(&n.to_le_bytes());
        }
        out.push(r.fast as u8);
        out.push(r.reported_usd.is_some() as u8);
        out.extend_from_slice(&r.reported_usd.unwrap_or(0.0).to_le_bytes());
    }
    out
}

/// Reads every complete line of `path` for its token usage per request.
/// Returns a buffer in the layout above, released with `repogo_free`, or null
/// when the file cannot be read. Never panics across the boundary.
///
/// # Safety
/// `path` must be a valid NUL-terminated string and `out_len` a valid pointer.
#[no_mangle]
pub unsafe extern "C" fn repogo_scan_usage(agent: u32, path: *const c_char, out_len: *mut usize) -> *mut u8 {
    let result = std::panic::catch_unwind(|| {
        let path = CStr::from_ptr(path).to_str().ok()?;
        let agent = match agent {
            REPOGO_AGENT_CLAUDE => crate::event::Agent::Claude,
            REPOGO_AGENT_CODEX => crate::event::Agent::Codex,
            _ => return None,
        };
        let records = crate::usage::scan(agent, path).ok()?;
        Some(encode_usage(&records))
    });
    match result {
        Ok(Some(buf)) => {
            let boxed = buf.into_boxed_slice();
            *out_len = boxed.len();
            Box::into_raw(boxed) as *mut u8
        }
        _ => {
            *out_len = 0;
            std::ptr::null_mut()
        }
    }
}
