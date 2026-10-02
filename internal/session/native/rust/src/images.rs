// Images a user sent, as attachment links in the prompt's text. Mirrors
// internal/session/images.go: a URL links as is; inline base64 is written once
// to <REPOGO_HOME>/attachments/transcripts/<fnv64 of the base64>.<ext>, the
// same name the Go side writes, and links as that file.

use std::fs;
use std::path::PathBuf;

const MAX_UPLOAD_BYTES: usize = 4 << 20;

pub fn image_link(n: usize, url: &str, media_type: &str, data: &str) -> String {
    let target = if url.starts_with("https://") {
        url.to_string()
    } else {
        let (media_type, data) = if url.starts_with("data:") { split_data_url(url) } else { (media_type, data) };
        match inline_image_path(media_type, data) {
            Some(path) => format!("file://{path}"),
            None => return String::new(),
        }
    };
    format!("[@Image {n}]({target})")
}

fn split_data_url(url: &str) -> (&str, &str) {
    let rest = &url["data:".len()..];
    match rest.split_once(',') {
        Some((head, data)) if head.ends_with(";base64") => (&head[..head.len() - ";base64".len()], data),
        _ => ("", ""),
    }
}

fn inline_image_path(media_type: &str, data: &str) -> Option<String> {
    if data.is_empty() {
        return None;
    }
    let path = attachments_dir()?.join("transcripts").join(format!("{:016x}.{}", fnv64(data), image_ext(media_type)));
    let path_str = path.to_string_lossy().into_owned();
    if path.exists() {
        return Some(path_str);
    }
    let raw = base64_decode(data)?;
    if raw.len() > MAX_UPLOAD_BYTES {
        return None;
    }
    fs::create_dir_all(path.parent()?).ok()?;
    let tmp = PathBuf::from(format!("{path_str}.tmp"));
    if fs::write(&tmp, &raw).is_err() || fs::rename(&tmp, &path).is_err() {
        let _ = fs::remove_file(&tmp);
        return None;
    }
    Some(path_str)
}

// apphome.Dir: $REPOGO_HOME, or ~/.repogo; then agent.AttachmentsDir.
fn attachments_dir() -> Option<PathBuf> {
    if let Ok(v) = std::env::var("REPOGO_HOME") {
        let v = v.trim();
        if !v.is_empty() {
            return Some(PathBuf::from(v).join("attachments"));
        }
    }
    let home = std::env::var("HOME").ok()?;
    Some(PathBuf::from(home).join(".repogo").join("attachments"))
}

fn image_ext(media_type: &str) -> &'static str {
    match media_type {
        "image/png" => "png",
        "image/jpeg" | "image/jpg" => "jpg",
        "image/gif" => "gif",
        "image/webp" => "webp",
        "image/heic" => "heic",
        _ => "png",
    }
}

pub fn fnv64(s: &str) -> u64 {
    let mut h: u64 = 14695981039346656037;
    for b in s.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(1099511628211);
    }
    h
}

// Standard base64 with padding, as Go's StdEncoding: None on anything else.
fn base64_decode(s: &str) -> Option<Vec<u8>> {
    let bytes = s.as_bytes();
    if bytes.len() % 4 != 0 {
        return None;
    }
    let val = |c: u8| -> Option<u32> {
        match c {
            b'A'..=b'Z' => Some((c - b'A') as u32),
            b'a'..=b'z' => Some((c - b'a' + 26) as u32),
            b'0'..=b'9' => Some((c - b'0' + 52) as u32),
            b'+' => Some(62),
            b'/' => Some(63),
            _ => None,
        }
    };
    let mut out = Vec::with_capacity(bytes.len() / 4 * 3);
    for (i, chunk) in bytes.chunks(4).enumerate() {
        let last = i == bytes.len() / 4 - 1;
        let pad = chunk.iter().rev().take_while(|&&c| c == b'=').count();
        if pad > 2 || (pad > 0 && !last) {
            return None;
        }
        let mut n: u32 = 0;
        for &c in &chunk[..4 - pad] {
            n = (n << 6) | val(c)?;
        }
        n <<= 6 * pad as u32;
        let b = [(n >> 16) as u8, (n >> 8) as u8, n as u8];
        out.extend_from_slice(&b[..3 - pad]);
    }
    Some(out)
}

pub fn with_links(text: &str, links: &[String]) -> String {
    if links.is_empty() {
        return text.to_string();
    }
    let all = links.join("\n");
    if text.is_empty() {
        all
    } else {
        format!("{text}\n{all}")
    }
}
