import { SQL } from "bun";
import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import { acquireOwner } from "./lease";
import type { Availability } from "./availability";
import type { RunMetadata, RunSummary } from "./contracts";

export type CatalogRun = { run: RunMetadata; summary: RunSummary; seen: number; terminal: number | null; error: string | null };
export class WorkflowCatalog {
  private constructor(private sql: SQL, private release: () => Promise<void>) {}
  static async open(directory: string): Promise<WorkflowCatalog> {
    await mkdir(directory, { recursive: true, mode: 0o700 });
    const release = await acquireOwner(join(directory, "owner.db"));
    let sql: SQL | null = null;
    try {
      sql = new SQL({ adapter: "sqlite", filename: join(directory, "catalog.db") });
      await sql.unsafe("pragma journal_mode = wal");
      await sql.unsafe("pragma synchronous = full");
      await sql.unsafe("create table if not exists runs (id text primary key, metadata text not null, summary text not null, seen real not null, terminal real, error text)");
      await sql.unsafe("create table if not exists availability (id integer primary key check(id = 1), seen real not null, offline_since real)");
      return new WorkflowCatalog(sql, release);
    } catch (error) { await sql?.close(); await release(); throw error; }
  }
  async all(): Promise<CatalogRun[]> {
    const rows = await this.sql.unsafe("select metadata, summary, seen, terminal, error from runs order by id");
    return rows.map((row: any) => ({ run: JSON.parse(row.metadata), summary: JSON.parse(row.summary), seen: row.seen, terminal: row.terminal, error: row.error }));
  }
  async put(value: CatalogRun): Promise<void> {
    await this.sql.unsafe("insert into runs (id,metadata,summary,seen,terminal,error) values (?,?,?,?,?,?) on conflict(id) do update set metadata=excluded.metadata,summary=excluded.summary,seen=excluded.seen,terminal=excluded.terminal,error=excluded.error", [
      value.run.id, JSON.stringify(value.run), JSON.stringify(value.summary), value.seen, value.terminal, value.error,
    ]);
  }
  async availability(): Promise<Availability | null> {
    const [row] = await this.sql.unsafe("select seen, offline_since from availability where id = 1");
    return row ? { seen: row.seen, offlineSince: row.offline_since } : null;
  }
  async heartbeat(value: Availability) {
    await this.sql.unsafe("insert into availability (id,seen,offline_since) values (1,?,?) on conflict(id) do update set seen=excluded.seen,offline_since=excluded.offline_since", [value.seen, value.offlineSince]);
  }
  async remove(id: string) { await this.sql.unsafe("delete from runs where id = ?", [id]); }
  async close() { try { await this.sql.close(); } finally { await this.release(); } }
}
