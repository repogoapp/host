// Standalone twin of bench/files: discover, parse, and write
// <out>/<session id>/session.json and events.json, all in Rust, so the file
// cache can be timed without Go on the path.
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Instant;

use clap::Parser;
use rayon::prelude::*;
use serde::Serialize;

use repogo_import::event::{Event, Meta, ToolCall};
use repogo_import::reader::{self, Provider};
use repogo_import::{claude, codex, exclude};

#[derive(Parser)]
struct Args {
    /// Directory holding one folder per session; defaults to ~/.repogo/bench-sessions-rust.
    #[arg(long)]
    out: Option<PathBuf>,
    /// claude, codex, or all
    #[arg(long, default_value = "claude")]
    provider: String,
    /// Parse+write worker threads; defaults to the core count.
    #[arg(long)]
    threads: Option<usize>,
    /// Keep the output directory instead of deleting it first.
    #[arg(long)]
    keep: bool,
}

// Field names follow the Go session.Meta and agent.Event JSON.
#[derive(Serialize)]
struct MetaJSON<'a> {
    id: &'a str,
    agent: &'a str,
    cwd: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    title: &'a str,
    path: &'a str,
    #[serde(skip_serializing_if = "<[String]>::is_empty")]
    paths: &'a [String],
    updated_at_ms: i64,
    size_bytes: i64,
    #[serde(skip_serializing_if = "is_zero")]
    context_used: i64,
    #[serde(skip_serializing_if = "is_zero")]
    context_size: i64,
}

#[derive(Serialize)]
struct EventJSON<'a> {
    turn_id: &'a str,
    seq: u64,
    kind: &'a str,
    #[serde(skip_serializing_if = "is_zero")]
    at: i64,
    #[serde(skip_serializing_if = "str::is_empty")]
    text: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    tool: Option<&'a ToolCall>,
    #[serde(skip_serializing_if = "str::is_empty")]
    error: &'a str,
}

fn is_zero(n: &i64) -> bool {
    *n == 0
}

fn meta_json(m: &Meta) -> MetaJSON<'_> {
    MetaJSON {
        id: &m.id,
        agent: m.agent.map(|a| a.as_str()).unwrap_or(""),
        cwd: &m.cwd,
        title: &m.title,
        path: &m.path,
        paths: &m.paths,
        updated_at_ms: m.updated_at_ms,
        size_bytes: m.size_bytes,
        context_used: m.context_used,
        context_size: m.context_size,
    }
}

fn event_json(e: &Event) -> EventJSON<'_> {
    EventJSON {
        turn_id: &e.turn_id,
        seq: 0,
        kind: e.kind.as_str(),
        at: e.at,
        text: &e.text,
        tool: e.tool.as_ref(),
        error: &e.error,
    }
}

#[derive(Default)]
struct Totals {
    parse_ns: AtomicU64,
    marshal_ns: AtomicU64,
    write_ns: AtomicU64,
    written: AtomicU64,
    events: AtomicU64,
    out_bytes: AtomicU64,
    errors: AtomicU64,
}

fn write(out: &Path, m: &Meta, events: &[Event], t: &Totals) -> std::io::Result<()> {
    let start = Instant::now();
    let meta = serde_json::to_vec(&meta_json(m))?;
    let evs = serde_json::to_vec(&events.iter().map(event_json).collect::<Vec<_>>())?;
    t.marshal_ns.fetch_add(start.elapsed().as_nanos() as u64, Ordering::Relaxed);

    let start = Instant::now();
    let dir = out.join(&m.id);
    std::fs::create_dir_all(&dir)?;
    std::fs::write(dir.join("session.json"), &meta)?;
    std::fs::write(dir.join("events.json"), &evs)?;
    t.write_ns.fetch_add(start.elapsed().as_nanos() as u64, Ordering::Relaxed);
    t.out_bytes.fetch_add((meta.len() + evs.len()) as u64, Ordering::Relaxed);
    Ok(())
}

fn main() {
    let args = Args::parse();
    if let Some(n) = args.threads {
        rayon::ThreadPoolBuilder::new().num_threads(n).build_global().unwrap();
    }
    let out = args.out.unwrap_or_else(|| {
        PathBuf::from(std::env::var("HOME").expect("HOME")).join(".repogo/bench-sessions-rust")
    });
    if !args.keep {
        let _ = std::fs::remove_dir_all(&out);
    }
    std::fs::create_dir_all(&out).expect("create output directory");

    let providers: Vec<(&str, fn() -> std::io::Result<Vec<Meta>>, &dyn Provider)> = match args.provider.as_str() {
        "claude" => vec![("claude", claude::list, &claude::Claude)],
        "codex" => vec![("codex", codex::list, &codex::Codex)],
        _ => vec![("claude", claude::list, &claude::Claude), ("codex", codex::list, &codex::Codex)],
    };

    // --- discover -----------------------------------------------------------
    let started = Instant::now();
    let mut work: Vec<(Meta, &dyn Provider)> = Vec::new();
    for (name, list, p) in &providers {
        let metas = list().unwrap_or_else(|e| panic!("{name}: discovery failed: {e}"));
        work.extend(metas.into_iter().filter(|m| !exclude::is_excluded(&m.cwd)).map(|m| (m, *p)));
    }
    work.sort_by(|a, b| b.0.updated_at_ns.cmp(&a.0.updated_at_ns));
    let discovery = started.elapsed();
    let transcript: i64 = work.iter().map(|(m, _)| m.size_bytes).sum();

    println!("out        {}", out.display());
    println!("discover   {:>6} sessions  {:>8}  in {:.3}s", work.len(), mib(transcript as u64), discovery.as_secs_f64());
    println!("threads    {:>6} parse+write\n", rayon::current_num_threads());

    // --- parse + write (parallel) -------------------------------------------
    let t = Totals::default();
    let started = Instant::now();
    work.par_iter().for_each(|(m, p)| {
        let start = Instant::now();
        let events = reader::read_all(m, *p);
        t.parse_ns.fetch_add(start.elapsed().as_nanos() as u64, Ordering::Relaxed);
        let Ok(events) = events else {
            t.errors.fetch_add(1, Ordering::Relaxed);
            return;
        };
        if let Err(e) = write(&out, m, &events, &t) {
            eprintln!("write: {} {e}", m.id);
            t.errors.fetch_add(1, Ordering::Relaxed);
            return;
        }
        t.written.fetch_add(1, Ordering::Relaxed);
        t.events.fetch_add(events.len() as u64, Ordering::Relaxed);
    });
    let wall = started.elapsed();

    // --- report -------------------------------------------------------------
    let secs = |n: &AtomicU64| n.load(Ordering::Relaxed) as f64 / 1e9;
    println!("parsed     {:>6} sessions  {:>8} events", t.written.load(Ordering::Relaxed), t.events.load(Ordering::Relaxed));
    if t.errors.load(Ordering::Relaxed) > 0 {
        println!("errors     {:>6}", t.errors.load(Ordering::Relaxed));
    }
    println!("out size   {:>8}\n", mib(t.out_bytes.load(Ordering::Relaxed)));
    println!(
        "WALL       {:.3}s   (discover + this = {:.3}s)",
        wall.as_secs_f64(),
        (discovery + wall).as_secs_f64()
    );
    println!(
        "  cpu across {} threads: parse {:.3}s   marshal {:.3}s   mkdir+write {:.3}s",
        rayon::current_num_threads(),
        secs(&t.parse_ns),
        secs(&t.marshal_ns),
        secs(&t.write_ns)
    );
}

fn mib(n: u64) -> String {
    format!("{:.1} MB", n as f64 / (1u64 << 20) as f64)
}
