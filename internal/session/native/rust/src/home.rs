use std::path::PathBuf;

pub fn claude_home() -> PathBuf {
    provider_home("CLAUDE_CONFIG_DIR", ".claude")
}

pub fn codex_home() -> PathBuf {
    provider_home("CODEX_HOME", ".codex")
}

fn provider_home(env: &str, dir: &str) -> PathBuf {
    if let Ok(v) = std::env::var(env) {
        let v = v.trim();
        if !v.is_empty() {
            return PathBuf::from(v);
        }
    }
    PathBuf::from(std::env::var("HOME").unwrap_or_default()).join(dir)
}
