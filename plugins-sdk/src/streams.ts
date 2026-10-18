import { appendFile, mkdir, readdir, readFile, rm, stat, truncate } from "node:fs/promises";
import { join } from "node:path";
import type { z } from "zod";
import { parseDuration, type Duration } from "./duration";
import { MethodError } from "./errors";
import { safeName, Store, tableName, type StoreOptions } from "./store";
import { assertWritable, recordRead, recordWrite } from "./tracking";

/** One line of a stream. `id` is `<key>:<offset>`, for cards; on disk a line is `{ offset, at, data }`. */
export type StreamEntry<T> = { id: string; key: string; offset: number; at: string; data: T };

/** One key of a stream: a run, a job, a day. */
export type StreamKey = { key: string; lines: number; bytes: number; endedAt: string | null; updatedAt: string };

/**
 * How long a stream keeps what it was sent: a key untouched for `age` goes
 * whole, and past `bytes` its oldest segments go. `"forever"` keeps everything.
 */
export type Retain = { age?: Duration; bytes?: number } | "forever";

export const defaultRetain = { age: "7d", bytes: 50_000_000 } as const;

/** A key's lines roll to a new file past this, so trimming deletes files instead of rewriting one. */
const segmentBytes = 1_000_000;

/** Retention runs this often; it lists folders, so not on every tick. */
const retainEveryMs = 60_000;

/** How often a follower looks for lines another Worker appended. */
const followPollMs = 1_000;

type Row = {
  key: string;
  /** The oldest offset still on disk, and the next one to write. */
  first_offset: number;
  next_offset: number;
  /** The current segment's first offset, which names its file, and its committed length. */
  segment: number;
  segment_bytes: number;
  bytes: number;
  ended_at: string | null;
  updated_at: string;
};

// Append-only JSONL per key, `<namespace>/streams/<name>/<key>/<first offset>.jsonl`,
// with a row per key in the namespace's `data.db`. An append writes the line and its row in
// one transaction; a line past the row's length (a crash between the two) is
// cut off before the next append, so offsets are never reused or skipped.
export class Stream<S extends z.ZodType> extends Store {
  private table: string;
  private folder: string;
  private ageMs: number | null;
  private maxBytes: number | null;
  private lastRetain = 0;
  /** Followers in this Worker waiting on a key's next line. */
  private waiting = new Map<string, Set<() => void>>();

  constructor(options: StoreOptions, name: string, private schema: S, retain: Retain = defaultRetain) {
    super(options, "streams", name);
    this.table = tableName("streams", name);
    this.folder = join(options.dir, "streams", name);
    this.ageMs = retain === "forever" || retain.age === undefined ? null : parseDuration(retain.age);
    this.maxBytes = retain === "forever" ? null : (retain.bytes ?? null);
  }

  /** The stream's folder, for agents to `tail`. */
  get path() {
    return this.folder;
  }

  async setup() {
    await mkdir(this.folder, { recursive: true });
    await this.sql.unsafe(
      `create table if not exists "${this.table}" (
        key text primary key,
        first_offset integer not null,
        next_offset integer not null,
        segment integer not null,
        segment_bytes integer not null,
        bytes integer not null,
        ended_at text,
        updated_at text not null
      )`,
    );
  }

  async append(key: string, data: z.input<S>): Promise<StreamEntry<z.infer<S>>> {
    assertWritable(this.tag);
    checkKey(key);
    const parsed = this.schema.parse(data);
    const entry = await this.options.database.transaction(async () => {
      const row = (await this.row(key)) ?? (await this.start(key));
      if (row.ended_at) throw new MethodError("VALIDATION", `${this.name} ${key} has ended; append to a new key.`);
      await this.cutUncommitted(row);
      const at = new Date().toISOString();
      const line = JSON.stringify({ offset: row.next_offset, at, data: parsed }) + "\n";
      const size = Buffer.byteLength(line);
      const segment = row.segment_bytes > 0 && row.segment_bytes + size > segmentBytes ? row.next_offset : row.segment;
      const written = segment === row.segment ? row.segment_bytes : 0;
      await appendFile(this.segmentPath(key, segment), line);
      await this.sql.unsafe(
        `update "${this.table}" set next_offset = $1, segment = $2, segment_bytes = $3, bytes = bytes + $4, updated_at = $5 where key = $6`,
        [row.next_offset + 1, segment, written + size, size, at, key],
      );
      recordWrite(this.tag);
      return { id: `${key}:${row.next_offset}`, key, offset: row.next_offset, at, data: parsed };
    });
    this.wake(key);
    return entry;
  }

  /** Lines from `from` on (the oldest kept when it's older), at most `limit`. */
  async read(key: string, options: { from?: number; limit?: number } = {}): Promise<StreamEntry<z.infer<S>>[]> {
    recordRead(this.tag);
    const row = await this.row(key);
    return row ? this.lines(row, options.from ?? 0, options.limit ?? 100) : [];
  }

  /**
   * Replays from `from`, then yields each line as it's appended, until the key
   * ends or `signal` aborts. Defaults to the last `last` lines (100).
   */
  async *follow(key: string, options: { from?: number; last?: number; signal: AbortSignal }) {
    recordRead(this.tag);
    let next = options.from ?? Math.max(0, ((await this.row(key))?.next_offset ?? 0) - (options.last ?? 100));
    while (!options.signal.aborted) {
      // Waiting starts before the read, so a line appended between the two still wakes it.
      const appended = this.nextAppend(key, options.signal);
      const row = await this.row(key);
      const lines = row ? await this.lines(row, next, 500) : [];
      for (const line of lines) yield line;
      next = lines.at(-1) ? lines.at(-1)!.offset + 1 : next;
      if (lines.length === 500) continue;
      if (row?.ended_at) return;
      await appended;
    }
  }

  /** Marks a key finished: followers stop, appends fail, and it goes once it's older than the retention age. */
  async end(key: string): Promise<void> {
    assertWritable(this.tag);
    await this.options.database.transaction(async () => {
      if (!(await this.row(key))) throw new MethodError("NOT_FOUND", `${this.name} has no key ${key}.`);
      const now = new Date().toISOString();
      await this.sql.unsafe(`update "${this.table}" set ended_at = $1, updated_at = $1 where key = $2`, [now, key]);
      recordWrite(this.tag);
    });
    this.wake(key);
  }

  async keys(options: { limit?: number } = {}): Promise<StreamKey[]> {
    recordRead(this.tag);
    const rows: Row[] = await this.sql.unsafe(`select * from "${this.table}" order by updated_at desc limit $1`, [
      options.limit ?? 100,
    ]);
    return rows.map(toKey);
  }

  async delete(key: string): Promise<void> {
    assertWritable(this.tag);
    await this.options.database.transaction(async () => {
      await rm(join(this.folder, key), { recursive: true, force: true });
      await this.sql.unsafe(`delete from "${this.table}" where key = $1`, [key]);
      recordWrite(this.tag);
    });
    this.wake(key);
  }

  async tick(now: number) {
    if (now - this.lastRetain < retainEveryMs || (this.ageMs === null && this.maxBytes === null)) return;
    this.lastRetain = now;
    const changed = await this.options.database.transaction(async () => {
      let changed = false;
      const rows: Row[] = await this.sql.unsafe(`select * from "${this.table}"`);
      for (const row of rows) changed = (await this.retain(row, now)) || changed;
      return changed;
    });
    if (changed) this.options.announce([this.tag]);
  }

  close() {
    for (const wakers of this.waiting.values()) for (const wake of wakers) wake();
    this.waiting.clear();
  }

  // A key untouched for the age goes whole; otherwise old segments go while the
  // key is over its bytes or they're older than the age. The current one stays.
  private async retain(row: Row, now: number): Promise<boolean> {
    if (this.ageMs !== null && Date.parse(row.updated_at) <= now - this.ageMs) {
      await rm(join(this.folder, row.key), { recursive: true, force: true });
      await this.sql.unsafe(`delete from "${this.table}" where key = $1`, [row.key]);
      return true;
    }
    const segments = (await this.segments(row.key)).filter((start) => start < row.segment);
    let { bytes, first_offset: first } = row;
    for (const [index, start] of segments.entries()) {
      const path = this.segmentPath(row.key, start);
      const info = await stat(path);
      const old = this.ageMs !== null && info.mtimeMs <= now - this.ageMs;
      const over = this.maxBytes !== null && bytes > this.maxBytes;
      if (!old && !over) break;
      await rm(path, { force: true });
      bytes -= info.size;
      first = segments[index + 1] ?? row.segment;
    }
    if (first === row.first_offset) return false;
    await this.sql.unsafe(`update "${this.table}" set first_offset = $1, bytes = $2 where key = $3`, [first, bytes, row.key]);
    return true;
  }

  private async lines(row: Row, from: number, limit: number): Promise<StreamEntry<z.infer<S>>[]> {
    const start = Math.max(from, row.first_offset);
    const segments = await this.segments(row.key);
    const firstSegment = segments.findLastIndex((segment) => segment <= start);
    const lines: StreamEntry<z.infer<S>>[] = [];
    for (const segment of segments.slice(Math.max(0, firstSegment))) {
      const text = await readFile(this.segmentPath(row.key, segment), "utf8").catch(() => "");
      for (const raw of text.split("\n")) {
        if (!raw) continue;
        const line = JSON.parse(raw) as { offset: number; at: string; data: z.infer<S> };
        // Past next_offset is a line whose append never committed.
        if (line.offset < start || line.offset >= row.next_offset) continue;
        lines.push({ id: `${row.key}:${line.offset}`, key: row.key, ...line });
        if (lines.length >= limit) return lines;
      }
    }
    return lines;
  }

  private async cutUncommitted(row: Row) {
    const path = this.segmentPath(row.key, row.segment);
    const size = await stat(path).then((info) => info.size, () => 0);
    if (size > row.segment_bytes) await truncate(path, row.segment_bytes);
  }

  private async start(key: string): Promise<Row> {
    await mkdir(join(this.folder, key), { recursive: true });
    const row: Row = {
      key, first_offset: 0, next_offset: 0, segment: 0, segment_bytes: 0, bytes: 0, ended_at: null,
      updated_at: new Date().toISOString(),
    };
    await this.sql.unsafe(
      `insert into "${this.table}" (key, first_offset, next_offset, segment, segment_bytes, bytes, updated_at)
       values ($1, 0, 0, 0, 0, 0, $2)`,
      [key, row.updated_at],
    );
    return row;
  }

  private async row(key: string): Promise<Row | null> {
    const rows = await this.sql.unsafe(`select * from "${this.table}" where key = $1`, [key]);
    return rows[0] ?? null;
  }

  /** A key's segment files by first offset, oldest first. */
  private async segments(key: string): Promise<number[]> {
    const names = await readdir(join(this.folder, key)).catch(() => [] as string[]);
    return names.filter((name) => /^\d+\.jsonl$/.test(name)).map((name) => Number.parseInt(name)).sort((a, b) => a - b);
  }

  private segmentPath(key: string, segment: number) {
    return join(this.folder, key, `${String(segment).padStart(12, "0")}.jsonl`);
  }

  /** Settles on this Worker's next append to `key`, after a poll interval for other Workers', or on abort. */
  private nextAppend(key: string, signal: AbortSignal): Promise<void> {
    return new Promise((resolve) => {
      const wakers = this.waiting.get(key) ?? new Set();
      this.waiting.set(key, wakers);
      const done = () => {
        clearTimeout(timer);
        signal.removeEventListener("abort", done);
        wakers.delete(done);
        resolve();
      };
      const timer = setTimeout(done, followPollMs);
      signal.addEventListener("abort", done);
      wakers.add(done);
    });
  }

  private wake(key: string) {
    for (const wake of [...(this.waiting.get(key) ?? [])]) wake();
  }
}

function checkKey(key: string) {
  if (!safeName.test(key)) {
    throw new MethodError("VALIDATION", `A stream key is letters, digits, dots, dashes and underscores, up to 128: ${key}`);
  }
}

function toKey(row: Row): StreamKey {
  return {
    key: row.key,
    lines: row.next_offset - row.first_offset,
    bytes: row.bytes,
    endedAt: row.ended_at,
    updatedAt: row.updated_at,
  };
}
