const WINDOWS: &[(&str, i64)] = &[
    ("claude-opus-5", 1_000_000),
    ("claude-sonnet-5", 1_000_000),
    ("claude-fable-5", 1_000_000),
    ("claude-fable-5-1", 1_000_000),
    ("claude-haiku-5", 1_000_000),
    ("claude-sonnet-4-5", 200_000),
    ("claude-opus-4-1", 200_000),
    ("claude-opus-4", 200_000),
    ("claude-sonnet-4", 200_000),
    ("claude-haiku-4-5", 200_000),
    ("claude-3-7-sonnet", 200_000),
    ("claude-3-5-sonnet", 200_000),
    ("claude-3-5-haiku", 200_000),
];

pub fn context_window_for(model: &str) -> i64 {
    let mut name = model.trim().to_lowercase();
    if name.is_empty() {
        return 0;
    }
    if let Some(i) = name.rfind('.') {
        name = name[i + 1..].to_string();
    }
    if let Some(i) = name.rfind('/') {
        name = name[i + 1..].to_string();
    }
    let mut best = 0;
    let mut best_len = 0;
    for (key, window) in WINDOWS {
        if name.starts_with(key) && key.len() > best_len {
            best = *window;
            best_len = key.len();
        }
    }
    best
}
