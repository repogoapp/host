// Transcript discovery and normalization for Claude and Codex, shared by the
// benchmark binary (main.rs) and the C ABI the Go host links (ffi.rs).
pub mod claude;
pub mod codex;
pub mod contextwindow;
pub mod event;
pub mod exclude;
pub mod ffi;
pub mod gojson;
pub mod home;
pub mod images;
pub mod peek;
pub mod reader;
pub mod usage;

use std::time::UNIX_EPOCH;

pub fn mtime(info: &std::fs::Metadata) -> (i64, i128) {
    let d = info.modified().ok().and_then(|t| t.duration_since(UNIX_EPOCH).ok()).unwrap_or_default();
    (d.as_millis() as i64, d.as_nanos() as i128)
}
