import type { z } from "zod";
import { parseDuration, type Duration } from "./duration";
import { Store, tableName, type StoreOptions } from "./store";
import { assertWritable, recordRead, recordWrite } from "./tracking";

/** One key and its value, as `get` sees it and the generated methods send it. */
export type KvEntry<T> = { key: string; value: T; expiresAt: string | null; createdAt: string; updatedAt: string };

/** `ttl`: how long the key lives; setting a key without one keeps it until deleted. */
export type SetOptions = { ttl?: Duration };

type Row = { key: string; value: string; expires_at: number | null; created_at: string; updated_at: string };

// Keys to JSON values, one table in the namespace's `data.db`. A key past its `ttl` reads as
// missing at once, and the sweep deletes it and tells cards. Each write emits
// `<plugin>.<kv>.<created|updated|deleted>` with the entry.
export class Kv<S extends z.ZodType> extends Store {
  private table: string;

  constructor(options: StoreOptions, name: string, private schema: S) {
    super(options, "kv", name);
    this.table = tableName("kv", name);
  }

  async setup() {
    await this.sql.unsafe(
      `create table if not exists "${this.table}" (
        key text primary key,
        value text not null,
        expires_at integer,
        created_at text not null,
        updated_at text not null
      )`,
    );
  }

  async get(key: string): Promise<z.infer<S> | null> {
    return (await this.entry(key))?.value ?? null;
  }

  async entry(key: string): Promise<KvEntry<z.infer<S>> | null> {
    recordRead(this.tag);
    const row = await this.live(key);
    return row ? toEntry(row) : null;
  }

  async set(key: string, value: z.input<S>, options: SetOptions = {}): Promise<KvEntry<z.infer<S>>> {
    assertWritable(this.tag);
    const parsed = this.schema.parse(value);
    const expiresAt = options.ttl === undefined ? null : Date.now() + parseDuration(options.ttl);
    const { entry, created } = await this.options.database.transaction(async () => {
      const before = await this.live(key);
      const now = new Date().toISOString();
      const row: Row = {
        key,
        value: JSON.stringify(parsed),
        expires_at: expiresAt,
        created_at: before?.created_at ?? now,
        updated_at: now,
      };
      await this.sql.unsafe(
        `insert into "${this.table}" (key, value, expires_at, created_at, updated_at) values ($1, $2, $3, $4, $5)
         on conflict (key) do update set value = excluded.value, expires_at = excluded.expires_at,
           created_at = excluded.created_at, updated_at = excluded.updated_at`,
        [row.key, row.value, row.expires_at, row.created_at, row.updated_at],
      );
      recordWrite(this.tag);
      return { entry: toEntry(row), created: !before };
    });
    await this.emit(created ? "created" : "updated", entry);
    return entry;
  }

  async delete(key: string): Promise<void> {
    assertWritable(this.tag);
    const before = await this.options.database.transaction(async () => {
      const before = await this.live(key);
      await this.sql.unsafe(`delete from "${this.table}" where key = $1`, [key]);
      if (before) recordWrite(this.tag);
      return before;
    });
    if (before) await this.emit("deleted", toEntry(before));
  }

  /** Live keys in key order, those starting with `prefix` when it's given. */
  async list(options: { prefix?: string; limit?: number } = {}): Promise<KvEntry<z.infer<S>>[]> {
    recordRead(this.tag);
    const prefix = options.prefix ?? "";
    const rows = await this.sql.unsafe(
      `select * from "${this.table}"
       where substr(key, 1, length($1)) = $1 and (expires_at is null or expires_at > $2)
       order by key limit $3`,
      [prefix, Date.now(), options.limit ?? 100],
    );
    return rows.map(toEntry);
  }

  // Deletes what expired by `now`, checked under the lock, so a key set again meanwhile stays.
  async tick(now: number) {
    const expired: Row[] = await this.options.database.transaction(async () => {
      const rows = await this.sql.unsafe(`select * from "${this.table}" where expires_at <= $1`, [now]);
      if (rows.length > 0) await this.sql.unsafe(`delete from "${this.table}" where expires_at <= $1`, [now]);
      return rows;
    });
    if (expired.length === 0) return;
    this.options.announce([this.tag]);
    for (const row of expired) await this.emit("deleted", toEntry(row));
  }

  private async live(key: string): Promise<Row | null> {
    const rows = await this.sql.unsafe(
      `select * from "${this.table}" where key = $1 and (expires_at is null or expires_at > $2)`,
      [key, Date.now()],
    );
    return rows[0] ?? null;
  }
}

function toEntry(row: Row): KvEntry<any> {
  return {
    key: row.key,
    value: JSON.parse(row.value),
    expiresAt: row.expires_at === null ? null : new Date(row.expires_at).toISOString(),
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}
