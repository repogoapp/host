use std::fs::File;
use std::io::{Read, Seek, SeekFrom};

pub const PEEK_HEAD: u64 = 64 << 10;
pub const PEEK_TAIL: u64 = 256 << 10;
pub const LINE_LIMIT: usize = 16 << 20;

#[derive(Default, Clone, Debug)]
pub struct PeekResult {
    pub cwd: String,
    pub title: String,
    pub id: String,
    pub context_used: i64,
    pub context_size: i64,
}

// Runs fn over the lines of a buffer the way bufio.Scanner does: the final
// unterminated fragment counts, a trailing \r is dropped, and a line past the
// limit ends the scan.
fn scan_lines(buf: &[u8], mut f: impl FnMut(&[u8]) -> bool) {
    let mut rest = buf;
    while !rest.is_empty() {
        let (line, next) = match memchr::memchr(b'\n', rest) {
            Some(i) => (&rest[..i], &rest[i + 1..]),
            None => (rest, &rest[rest.len()..]),
        };
        if line.len() > LINE_LIMIT {
            return;
        }
        let line = line.strip_suffix(b"\r").unwrap_or(line);
        if !f(line) {
            return;
        }
        rest = next;
    }
}

pub fn scan_head(path: &str, f: impl FnMut(&[u8]) -> bool) {
    let Ok(file) = File::open(path) else { return };
    let mut buf = Vec::new();
    if file.take(PEEK_HEAD).read_to_end(&mut buf).is_err() {
        return;
    }
    scan_lines(&buf, f);
}

pub fn scan_tail(path: &str, size: u64, f: impl FnMut(&[u8]) -> bool) {
    let Ok(mut file) = File::open(path) else { return };
    let mut buf = Vec::new();
    if size <= PEEK_TAIL {
        if file.read_to_end(&mut buf).is_err() {
            return;
        }
        scan_lines(&buf, f);
        return;
    }
    if file.seek(SeekFrom::Start(size - PEEK_TAIL)).is_err() || file.read_to_end(&mut buf).is_err() {
        return;
    }
    // The first line is almost certainly a fragment.
    let Some(i) = memchr::memchr(b'\n', &buf) else { return };
    scan_lines(&buf[i + 1..], f);
}
