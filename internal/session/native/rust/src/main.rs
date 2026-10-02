mod store;

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc::sync_channel;
use std::time::{Duration, Instant};

use clap::Parser;
use rayon::prelude::*;

use repogo_import::event::{Entry, Meta};
use repogo_import::reader::{self, Provider};
use repogo_import::{claude, codex, exclude};

#[derive(Parser)]
struct Args {
    /// Database to create. Refuses to overwrite.
    #[arg(long)]
    db: String,
    /// claude, codex, or all
    #[arg(long, default_value = "all")]
    provider: String,
    /// Parse worker threads; defaults to the core count.
    #[arg(long)]
    threads: Option<usize>,
    /// Sessions per transaction.
    #[arg(long, default_value_t = 64)]
    batch: usize,
    /// Extra PRAGMA statements applied after the production ones.
    #[arg(long)]
    pragma: Vec<String>,
}

struct Stats {
    sessions: usize,
    events: usize,
    discovery: Duration,
    parse_cpu: Duration,
    write: Duration,
    total: Duration,
}

fn run(db: &mut rusqlite::Connection, name: &str, list: fn() -> std::io::Result<Vec<Meta>>, p: &dyn Provider, batch: usize) -> Stats {
    let started = Instant::now();
    let mut metas = list().unwrap_or_else(|e| panic!("{name}: discovery failed: {e}"));
    metas.retain(|m| !exclude::is_excluded(&m.cwd));
    metas.sort_by(|a, b| b.updated_at_ns.cmp(&a.updated_at_ns));
    let discovery = started.elapsed();

    let parse_ns = AtomicU64::new(0);
    let events = AtomicU64::new(0);
    let mut write = Duration::ZERO;
    let (tx, rx) = sync_channel::<Vec<Entry>>(2);
    std::thread::scope(|s| {
        s.spawn(|| {
            metas.par_chunks(batch).for_each(|chunk| {
                let t = Instant::now();
                let entries: Vec<Entry> = chunk
                    .iter()
                    .filter_map(|m| reader::read_all(m, p).ok().map(|events| Entry { meta: m.clone(), events }))
                    .collect();
                events.fetch_add(entries.iter().map(|e| e.events.len() as u64).sum(), Ordering::Relaxed);
                parse_ns.fetch_add(t.elapsed().as_nanos() as u64, Ordering::Relaxed);
                tx.send(entries).unwrap();
            });
            drop(tx);
        });
        for entries in rx {
            let t = Instant::now();
            store::sync_batch(db, &entries).unwrap_or_else(|e| panic!("{name}: batch failed: {e}"));
            write += t.elapsed();
        }
    });

    Stats {
        sessions: metas.len(),
        events: events.load(Ordering::Relaxed) as usize,
        discovery,
        parse_cpu: Duration::from_nanos(parse_ns.load(Ordering::Relaxed)),
        write,
        total: started.elapsed(),
    }
}

fn main() {
    let args = Args::parse();
    if std::path::Path::new(&args.db).exists() {
        eprintln!("{} already exists; pick a fresh path", args.db);
        std::process::exit(1);
    }
    if let Some(n) = args.threads {
        rayon::ThreadPoolBuilder::new().num_threads(n).build_global().unwrap();
    }
    let mut db = store::open(&args.db).expect("open database");
    for p in &args.pragma {
        db.execute_batch(&format!("PRAGMA {p};")).expect("pragma");
    }

    let providers: Vec<(&str, fn() -> std::io::Result<Vec<Meta>>, &dyn Provider)> = match args.provider.as_str() {
        "claude" => vec![("claude", claude::list, &claude::Claude)],
        "codex" => vec![("codex", codex::list, &codex::Codex)],
        _ => vec![("claude", claude::list, &claude::Claude), ("codex", codex::list, &codex::Codex)],
    };

    let mut all = Stats { sessions: 0, events: 0, discovery: Duration::ZERO, parse_cpu: Duration::ZERO, write: Duration::ZERO, total: Duration::ZERO };
    for (name, list, p) in providers {
        let s = run(&mut db, name, list, p, args.batch);
        report(name, &s);
        all.sessions += s.sessions;
        all.events += s.events;
        all.discovery += s.discovery;
        all.parse_cpu += s.parse_cpu;
        all.write += s.write;
        all.total += s.total;
    }
    report("all", &all);
}

fn report(name: &str, s: &Stats) {
    println!(
        "{name:<7} sessions {:>5}  events {:>7}  discovery {:>6.2}s  parse-cpu {:>6.2}s  write {:>6.2}s  total {:>6.2}s",
        s.sessions,
        s.events,
        s.discovery.as_secs_f64(),
        s.parse_cpu.as_secs_f64(),
        s.write.as_secs_f64(),
        s.total.as_secs_f64()
    );
}
