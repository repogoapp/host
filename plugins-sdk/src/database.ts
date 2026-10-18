import { SQL } from "bun";

// A namespace's SQLite file, `<namespace>/data.db`, and the one connection every store in it shares:
// collections, kv and the indexes of streams and buckets. Its writes run one at
// a time through `transaction`, so two stores can't open overlapping
// transactions on the connection, and `begin immediate` holds off other Workers.
export class Database {
  private writes: Promise<unknown> = Promise.resolve();

  private constructor(readonly sql: SQL) {}

  static async open(path: string) {
    const sql = new SQL(`sqlite://${path}`);
    // Other Workers open the same file: WAL lets them read while one writes,
    // and a writer waits for the other's lock.
    await sql.unsafe("pragma journal_mode = wal");
    await sql.unsafe("pragma busy_timeout = 5000");
    return new Database(sql);
  }

  /** Runs `work` alone in a transaction; it commits when `work` returns and rolls back when it throws. */
  transaction<T>(work: () => Promise<T>): Promise<T> {
    const result = this.writes.then(() => this.run(work), () => this.run(work));
    this.writes = result.catch(() => {});
    return result;
  }

  close() {
    this.sql.close();
  }

  private async run<T>(work: () => Promise<T>): Promise<T> {
    await this.sql.unsafe("begin immediate");
    try {
      const value = await work();
      await this.sql.unsafe("commit");
      return value;
    } catch (error) {
      await this.sql.unsafe("rollback").catch(() => {});
      throw error;
    }
  }
}
