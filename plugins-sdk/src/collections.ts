import type { SQL } from "bun";
import type { z } from "zod";
import type { EventBus } from "./events";
import { assertWritable, recordRead, recordWrite } from "./tracking";

export type Record_<T> = T & { id: string; createdAt: string; updatedAt: string };

export type ListOptions = {
  where?: Record<string, string | number | boolean | null>;
  orderBy?: "createdAt" | "updatedAt";
  order?: "asc" | "desc";
  limit?: number;
};

// A collection is one table of JSON rows. Every write is checked against the
// schema and emits `<plugin>.<collection>.<created|updated|deleted>`. Reads and
// writes are tracked under the tag `<db>.<collection>`, the table's identity
// across every plugin and Worker that opens the file, so cards refresh.
export class Collection<S extends z.ZodObject> {
  private table: string;
  readonly tag: string;
  /** This Worker's writes run one at a time; the transaction holds off other Workers. */
  private writes: Promise<unknown> = Promise.resolve();

  constructor(
    private sql: SQL,
    private events: EventBus,
    private plugin: string,
    db: string,
    private name: string,
    private schema: S,
  ) {
    this.table = name.replace(/[^a-zA-Z0-9_]/g, "_");
    this.tag = `${db}.${name}`;
  }

  async setup() {
    await this.sql.unsafe(
      `create table if not exists "${this.table}" (
        id text primary key,
        data text not null,
        created_at text not null,
        updated_at text not null
      )`,
    );
  }

  // Each write's read-merge-write runs alone, in a transaction that takes the
  // file's write lock before the read, so two Workers can't overwrite each
  // other's change. Its event fires after, so a handler that writes to the same
  // collection doesn't wait on itself.
  private serial<T>(work: () => Promise<T>): Promise<T> {
    const result = this.writes.then(() => this.transaction(work), () => this.transaction(work));
    this.writes = result.catch(() => {});
    return result;
  }

  private async transaction<T>(work: () => Promise<T>): Promise<T> {
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

  async create(input: z.input<S>): Promise<Record_<z.infer<S>>> {
    assertWritable(this.tag);
    const record = await this.serial(async () => {
      const data = this.schema.parse(input);
      const now = new Date().toISOString();
      const record = { ...data, id: crypto.randomUUID(), createdAt: now, updatedAt: now };
      await this.sql.unsafe(`insert into "${this.table}" (id, data, created_at, updated_at) values ($1, $2, $3, $4)`, [
        record.id,
        JSON.stringify(data),
        now,
        now,
      ]);
      recordWrite(this.tag);
      return record;
    });
    await this.events.emit(this.eventName("created"), record);
    return record;
  }

  async get(id: string): Promise<Record_<z.infer<S>> | null> {
    recordRead(this.tag);
    const rows = await this.sql.unsafe(`select * from "${this.table}" where id = $1`, [id]);
    return rows[0] ? this.toRecord(rows[0]) : null;
  }

  async update(id: string, patch: Partial<z.input<S>>): Promise<Record_<z.infer<S>>> {
    assertWritable(this.tag);
    const { before, after } = await this.serial(async () => {
      const before = await this.get(id);
      if (!before) throw new Error(`${this.name} ${id} not found`);
      const { id: _, createdAt, updatedAt: __, ...fields } = before;
      const data = this.schema.parse({ ...fields, ...patch });
      const now = new Date().toISOString();
      await this.sql.unsafe(`update "${this.table}" set data = $1, updated_at = $2 where id = $3`, [JSON.stringify(data), now, id]);
      recordWrite(this.tag);
      return { before, after: { ...data, id, createdAt, updatedAt: now } };
    });
    await this.events.emit(this.eventName("updated"), { before, after });
    return after;
  }

  async delete(id: string): Promise<void> {
    assertWritable(this.tag);
    const before = await this.serial(async () => {
      const before = await this.get(id);
      if (!before) return null;
      await this.sql.unsafe(`delete from "${this.table}" where id = $1`, [id]);
      recordWrite(this.tag);
      return before;
    });
    if (before) await this.events.emit(this.eventName("deleted"), before);
  }

  async list(options: ListOptions = {}): Promise<Record_<z.infer<S>>[]> {
    recordRead(this.tag);
    const params: unknown[] = [];
    const clauses = Object.entries(options.where ?? {}).map(([field, value]) => {
      if (!/^[a-zA-Z0-9_]+$/.test(field)) throw new Error(`bad field name: ${field}`);
      if (value === null) return `json_extract(data, '$.${field}') is null`;
      params.push(typeof value === "boolean" ? Number(value) : value);
      return `json_extract(data, '$.${field}') = $${params.length}`;
    });
    const where = clauses.length ? `where ${clauses.join(" and ")}` : "";
    const column = options.orderBy === "updatedAt" ? "updated_at" : "created_at";
    const order = options.order === "asc" ? "asc" : "desc";
    params.push(options.limit ?? 100);
    const rows = await this.sql.unsafe(
      `select * from "${this.table}" ${where} order by ${column} ${order}, id ${order} limit $${params.length}`,
      params,
    );
    return rows.map((row: any) => this.toRecord(row));
  }

  private eventName(kind: "created" | "updated" | "deleted") {
    return `${this.plugin}.${this.name}.${kind}`;
  }

  private toRecord(row: any): Record_<z.infer<S>> {
    return { ...JSON.parse(row.data), id: row.id, createdAt: row.created_at, updatedAt: row.updated_at };
  }
}
