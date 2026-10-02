use std::sync::OnceLock;

fn roots() -> &'static Vec<String> {
    static ROOTS: OnceLock<Vec<String>> = OnceLock::new();
    ROOTS.get_or_init(|| {
        let tmp = std::env::var("TMPDIR").unwrap_or_else(|_| "/tmp".into());
        let mut out: Vec<String> = Vec::new();
        let mut add = |dir: String| {
            let dir = clean(&dir);
            if dir.is_empty() || dir == "/" || dir == "." || out.contains(&dir) {
                return;
            }
            out.push(dir);
        };
        for dir in [tmp.as_str(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"] {
            add(dir.to_string());
            if let Ok(resolved) = std::fs::canonicalize(dir) {
                add(resolved.to_string_lossy().into_owned());
            }
        }
        out
    })
}

// Enough of filepath.Clean for the paths a CLI records: absolute, no dot
// segments, at most a trailing slash.
fn clean(p: &str) -> String {
    let mut out = String::with_capacity(p.len());
    let mut prev_slash = false;
    for c in p.chars() {
        if c == '/' {
            if prev_slash {
                continue;
            }
            prev_slash = true;
        } else {
            prev_slash = false;
        }
        out.push(c);
    }
    while out.len() > 1 && out.ends_with('/') {
        out.pop();
    }
    if out.is_empty() {
        ".".into()
    } else {
        out
    }
}

pub fn is_excluded(cwd: &str) -> bool {
    if cwd.is_empty() {
        return false;
    }
    let cwd = clean(cwd);
    roots()
        .iter()
        .any(|root| cwd == *root || cwd.starts_with(&format!("{root}/")))
}
