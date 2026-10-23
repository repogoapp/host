import type { Database } from "./database";
import { MethodError } from "./errors";
import { record } from "./logs";
import { isolated, recordRead } from "./tracking";

/** How a `cached` method keeps its answers: fresh for this long, shared by every project or not. */
export type CacheOptions = { fresh: number; shared: boolean; timeout: number };

/** An answer bigger than this is returned but not saved. */
const maxBytes = 1_000_000;
/** An answer nobody has asked for in this long is deleted. */
const keepMs = 30 * 86_400_000;

type Row = { value: string; checked_at: number };

// The saved answers of a namespace's `cached` methods, a row per method, input
// and project. Each row is its own tag, so saving one redraws only the cards
// showing it, and saving the same answer again redraws none.
export class AnswerCache {
  /** One fetch per key at a time; a card and a background check share it. */
  private inflight = new Map<string, Promise<unknown>>();

  constructor(
    private database: Database,
    private namespace: string,
    private announce: (tags: string[]) => void,
  ) {}

  async setup() {
    await this.database.sql.unsafe(
      `create table if not exists cached_answers (
        key text primary key,
        value text not null,
        checked_at integer not null
      )`,
    );
  }

  /**
   * The saved answer at once, checked again in the background when it's
   * older than `fresh`. With none saved, the caller waits for the fetch.
   */
  async read(key: string, options: CacheOptions, fetch: () => unknown): Promise<unknown> {
    const tag = `${this.namespace}.cached.${Bun.hash(key).toString(36)}`;
    recordRead(tag);
    const [row] = (await this.database.sql.unsafe(`select value, checked_at from cached_answers where key = $1`, [key])) as Row[];
    if (!row) return this.refresh(key, tag, options, fetch);
    if (Date.now() - row.checked_at >= options.fresh) {
      // The card keeps the saved answer; the failure goes to the plugin's logs.
      this.refresh(key, tag, options, fetch).catch((error) => record("error", [error]));
    }
    return JSON.parse(row.value);
  }

  /** Settles once no fetch is out. */
  async idle() {
    while (this.inflight.size) await Promise.allSettled([...this.inflight.values()]);
  }

  private refresh(key: string, tag: string, options: CacheOptions, fetch: () => unknown): Promise<unknown> {
    const running = this.inflight.get(key);
    if (running) return running;
    const started = Date.now();
    const work = isolated(async () => withTimeout(Promise.resolve(fetch()), options.timeout))
      .then(async (value) => {
        await this.save(key, tag, value ?? null, started);
        return value ?? null;
      }, async (error) => {
        // Waits a full `fresh` before trying again, rather than on every read.
        await this.database.transaction(() => this.database.sql.unsafe(`update cached_answers set checked_at = $1 where key = $2`, [Date.now(), key]));
        throw error;
      })
      .finally(() => this.inflight.delete(key));
    this.inflight.set(key, work);
    return work;
  }

  private async save(key: string, tag: string, value: unknown, started: number) {
    const text = JSON.stringify(value);
    if (text.length > maxBytes) return;
    const changed = await this.database.transaction(async () => {
      const [before] = (await this.database.sql.unsafe(`select value, checked_at from cached_answers where key = $1`, [key])) as Row[];
      // Another Worker saved an answer fetched after this one started.
      if (before && before.checked_at > started) return false;
      const now = Date.now();
      await this.database.sql.unsafe(
        `insert into cached_answers (key, value, checked_at) values ($1, $2, $3)
         on conflict (key) do update set value = excluded.value, checked_at = excluded.checked_at`,
        [key, text, now],
      );
      await this.database.sql.unsafe(`delete from cached_answers where checked_at < $1`, [now - keepMs]);
      return before?.value !== text;
    });
    if (changed) this.announce([tag]);
  }
}

/** A key for one answer: the method, the project unless it's shared, and the input with its keys sorted. */
export function cacheKey(method: string, root: string | null, input: unknown) {
  return JSON.stringify([method, root, sorted(input)]);
}

function sorted(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sorted);
  if (value === null || typeof value !== "object") return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, sorted((value as Record<string, unknown>)[key])]));
}

function withTimeout<T>(work: Promise<T>, ms: number): Promise<T> {
  let timer: Timer;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new MethodError("INTERNAL", `The answer took longer than ${Math.round(ms / 1000)} s.`)), ms);
  });
  return Promise.race([work, timeout]).finally(() => clearTimeout(timer));
}
