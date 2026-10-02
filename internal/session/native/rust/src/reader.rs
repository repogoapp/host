use crate::event::{Event, Meta};
use crate::peek::LINE_LIMIT;

pub trait Provider: Sync {
    fn parse(&self, line: &[u8]) -> Vec<Event>;
}

// One file, whole: the largest transcript is tens of megabytes and reading it
// in one call beats a buffered reader once parsing is spread across cores.
pub fn read_file(path: &str, p: &dyn Provider, _session_id: &str, events: &mut Vec<Event>) -> std::io::Result<()> {
    let buf = std::fs::read(path)?;
    let mut rest = buf.as_slice();
    // A trailing fragment without its newline is a write in progress.
    while let Some(i) = memchr::memchr(b'\n', rest) {
        let line = &rest[..=i];
        rest = &rest[i + 1..];
        if line.len() > LINE_LIMIT {
            continue;
        }
        events.extend(p.parse(line));
    }
    Ok(())
}

pub fn read_all(m: &Meta, p: &dyn Provider) -> std::io::Result<Vec<Event>> {
    let mut events = Vec::new();
    let files = m.files();
    if files.len() == 1 {
        read_file(files[0], p, &m.id, &mut events)?;
        return Ok(events);
    }
    for f in files {
        // A segment that cannot be read is skipped, not fatal.
        let _ = read_file(f, p, &m.id, &mut events);
    }
    Ok(events)
}
