import { watch, type FSWatcher } from "node:fs";
import { mkdir, readdir, readFile, rename, rm, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { z } from "zod";
import { MethodError, toCallError } from "./errors";
import { safeName, Store, tableName, type StoreOptions } from "./store";
import { assertWritable, recordRead, recordWrite } from "./tracking";

/**
 * How a file's meta reads: `ok` matches the bucket's schema, `empty` has none,
 * `invalid` fails the schema, and `malformed` isn't JSON or its folder doesn't
 * hold exactly one file. `problem` says why for the last two.
 */
export type FileStatus = "ok" | "empty" | "invalid" | "malformed";

/** One file in a bucket. `path` is the file on disk; `meta` is its `meta.json` without `folder`. */
export type BucketFile<T> = {
  id: string;
  name: string;
  folder: string;
  path: string;
  size: number;
  type: string;
  meta: Partial<T>;
  status: FileStatus;
  problem: string | null;
  revision: number;
  createdAt: string;
  updatedAt: string;
};

/** What `put` writes: the file's name, its bytes, and optionally where it sits and its meta. */
export type PutFile<T> = { name: string; body: string | Uint8Array | ArrayBuffer | Blob; folder?: string; meta?: T };

type Row = {
  id: string;
  name: string;
  folder: string;
  size: number;
  type: string;
  meta: string;
  status: FileStatus;
  problem: string | null;
  revision: number;
  /** Of the file and of meta.json, so a scan tells a changed file from changed meta. */
  file_print: string;
  meta_print: string;
  created_at: string;
  updated_at: string;
};

/** What a scan read from one file's folder. */
type OnDisk = Omit<Row, "revision" | "created_at" | "updated_at"> & { createdAt: string; updatedAt: string };

const metaFile = "meta.json";
/** Where `put` assembles a file's folder before renaming it into place. */
const tmpFolder = ".tmp";
/** A burst of file events is one scan, this long after the last. */
const watchSettleMs = 200;
/** The watcher is a hint; a full scan this often catches what it missed. */
const scanEveryMs = 60_000;

// Files and their meta, one folder per file named by its id:
// `<namespace>/buckets/<name>/<id>/{<file>, meta.json}`. The folders are the truth, so
// an agent adds or edits a file with its own tools; the table and its search
// index are rebuilt from them by `scan`, on open, when the folder changes and
// once a minute. Changes emit `created`, `changed` (the file), `metaChanged`, `deleted`.
export class Bucket<S extends z.ZodObject> extends Store {
  private table: string;
  private searchTable: string;
  private folder: string;
  private watcher: FSWatcher | null = null;
  private settling: Timer | null = null;
  private scanning: Promise<void> = Promise.resolve();
  private lastScan = 0;

  constructor(options: StoreOptions, name: string, private schema: S) {
    super(options, "buckets", name);
    if ("folder" in schema.shape) throw new Error(`bucket ${name}: "folder" is the file's place, not a meta field`);
    this.table = tableName("buckets", name);
    this.searchTable = `${this.table}_search`;
    this.folder = join(options.dir, "buckets", name);
  }

  /** The bucket's folder, for agents to add and read files. */
  get path() {
    return this.folder;
  }

  async setup() {
    await mkdir(join(this.folder, tmpFolder), { recursive: true });
    await this.sql.unsafe(
      `create table if not exists "${this.table}" (
        id text primary key,
        name text not null,
        folder text not null,
        size integer not null,
        type text not null,
        meta text not null,
        status text not null,
        problem text,
        revision integer not null,
        file_print text not null,
        meta_print text not null,
        created_at text not null,
        updated_at text not null
      )`,
    );
    await this.sql.unsafe(`create virtual table if not exists "${this.searchTable}" using fts5(id unindexed, name, folder, text)`);
    await this.scan();
    this.watcher = watch(this.folder, { recursive: true }, (_, file) => {
      if (String(file ?? "").startsWith(tmpFolder)) return;
      if (this.settling) clearTimeout(this.settling);
      this.settling = setTimeout(() => void this.scan(), watchSettleMs);
    });
  }

  async put(file: PutFile<z.input<S>>): Promise<BucketFile<z.infer<S>>> {
    assertWritable(this.tag);
    checkName(file.name);
    const meta = this.schema.parse(file.meta ?? {});
    const id = Bun.randomUUIDv7();
    const staging = join(this.folder, tmpFolder, id);
    await mkdir(staging, { recursive: true });
    await Bun.write(join(staging, file.name), file.body);
    await writeMeta(staging, { ...meta, ...(file.folder ? { folder: file.folder } : {}) });
    await rename(staging, join(this.folder, id));
    const { after } = await this.reindex(id);
    return after!;
  }

  async get(id: string): Promise<BucketFile<z.infer<S>> | null> {
    recordRead(this.tag);
    const rows = await this.sql.unsafe(`select * from "${this.table}" where id = $1`, [id]);
    return rows[0] ? this.toFile(rows[0]) : null;
  }

  /** The file's bytes, to read as text, JSON, a stream or an ArrayBuffer. */
  async file(id: string) {
    const found = await this.get(id);
    if (!found) throw new MethodError("NOT_FOUND", `${this.name} has no file ${id}.`);
    return Bun.file(found.path);
  }

  /** Newest first; `folder` keeps one folder's files, `status` one status's. */
  async list(options: { folder?: string; status?: FileStatus; limit?: number } = {}): Promise<BucketFile<z.infer<S>>[]> {
    recordRead(this.tag);
    const rows = await this.sql.unsafe(
      `select * from "${this.table}"
       where ($1 is null or folder = $1) and ($2 is null or status = $2)
       order by created_at desc, id desc limit $3`,
      [options.folder ?? null, options.status ?? null, options.limit ?? 100],
    );
    return rows.map((row: Row) => this.toFile(row));
  }

  /** Files whose name, folder or meta text has every word of `query`, each as a prefix, best first. */
  async search(query: string, options: { limit?: number } = {}): Promise<BucketFile<z.infer<S>>[]> {
    recordRead(this.tag);
    const words = query.match(/[\p{L}\p{N}]+/gu) ?? [];
    if (words.length === 0) return [];
    const rows = await this.sql.unsafe(
      `select t.* from "${this.searchTable}" s join "${this.table}" t on t.id = s.id
       where "${this.searchTable}" match $1 order by s.rank limit $2`,
      [words.map((word) => `"${word}"*`).join(" "), options.limit ?? 50],
    );
    return rows.map((row: Row) => this.toFile(row));
  }

  /**
   * Merges `meta` into the file's meta.json (`null` removes a field), checked
   * against the schema. With `revision`, fails if the file changed since it was read.
   */
  async update(id: string, meta: Partial<{ [K in keyof z.input<S>]: z.input<S>[K] | null }>, options: { revision?: number } = {}) {
    assertWritable(this.tag);
    return this.rewrite(id, options.revision, async (folder, current) => {
      const merged = Object.fromEntries(Object.entries({ ...current, ...meta }).filter(([, value]) => value !== null));
      const { folder: place, ...fields } = merged;
      return { ...this.schema.parse(fields), ...(place ? { folder: place } : {}) };
    });
  }

  /** Renames the file, or moves it to another folder; its id stays. */
  async move(id: string, to: { name?: string; folder?: string }, options: { revision?: number } = {}) {
    assertWritable(this.tag);
    if (to.name !== undefined) checkName(to.name);
    return this.rewrite(id, options.revision, async (folder, current, name) => {
      if (to.name !== undefined && to.name !== name) await rename(join(folder, name), join(folder, to.name));
      const { folder: _, ...fields } = current;
      return { ...fields, ...((to.folder ?? current.folder) ? { folder: to.folder ?? current.folder } : {}) };
    });
  }

  async delete(id: string): Promise<void> {
    assertWritable(this.tag);
    checkID(id);
    await rm(join(this.folder, id), { recursive: true, force: true });
    await this.reindex(id);
  }

  async tick(now: number) {
    if (now - this.lastScan >= scanEveryMs) await this.scan();
  }

  close() {
    this.watcher?.close();
    if (this.settling) clearTimeout(this.settling);
  }

  /**
   * Brings the table in line with the folders, one scan at a time. A folder
   * whose prints match its row is skipped; rows go only once the listing worked.
   */
  scan(): Promise<void> {
    this.scanning = this.scanning.then(() => this.scanNow()).catch((error) => {
      console.error(`[buckets] ${this.name}: scan failed:`, toCallError(error).message);
    });
    return this.scanning;
  }

  private async scanNow() {
    this.lastScan = Date.now();
    const entries = await readdir(this.folder, { withFileTypes: true });
    const ids = entries.filter((entry) => entry.isDirectory() && safeName.test(entry.name)).map((entry) => entry.name);
    const rows: Pick<Row, "id" | "file_print" | "meta_print">[] = await this.sql.unsafe(
      `select id, file_print, meta_print from "${this.table}"`,
    );
    const known = new Map(rows.map((row) => [row.id, row]));
    const present = new Set(ids);
    const changed: string[] = [];
    for (const id of ids) {
      const row = known.get(id);
      const prints = await fingerprints(join(this.folder, id));
      if (!row || row.file_print !== prints.file || row.meta_print !== prints.meta) changed.push(id);
    }
    const gone = rows.map((row) => row.id).filter((id) => !present.has(id));
    for (const id of [...changed, ...gone]) await this.reindex(id, { announce: true });
  }

  // Reads one file's folder and writes its row, under the lock; a folder that's
  // gone deletes the row. Emits what changed. A tracked call records the write;
  // a scan, outside any call, announces it.
  private async reindex(id: string, options: { announce?: boolean } = {}) {
    const result = await this.options.database.transaction(async () => {
      const before: Row | undefined = (await this.sql.unsafe(`select * from "${this.table}" where id = $1`, [id]))[0];
      const disk = await this.read(id);
      if (!disk) {
        if (!before) return { before: null, after: null };
        await this.sql.unsafe(`delete from "${this.table}" where id = $1`, [id]);
        await this.sql.unsafe(`delete from "${this.searchTable}" where id = $1`, [id]);
        recordWrite(this.tag);
        return { before, after: null };
      }
      if (before && before.file_print === disk.file_print && before.meta_print === disk.meta_print) {
        return { before, after: before };
      }
      const row: Row = {
        ...disk,
        revision: (before?.revision ?? 0) + 1,
        created_at: before?.created_at ?? disk.createdAt,
        updated_at: disk.updatedAt,
      };
      await this.sql.unsafe(
        `insert or replace into "${this.table}"
         (id, name, folder, size, type, meta, status, problem, revision, file_print, meta_print, created_at, updated_at)
         values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
        [row.id, row.name, row.folder, row.size, row.type, row.meta, row.status, row.problem, row.revision,
          row.file_print, row.meta_print, row.created_at, row.updated_at],
      );
      await this.sql.unsafe(`delete from "${this.searchTable}" where id = $1`, [id]);
      await this.sql.unsafe(`insert into "${this.searchTable}" (id, name, folder, text) values ($1, $2, $3, $4)`, [
        id, row.name, row.folder, searchText(JSON.parse(row.meta)),
      ]);
      recordWrite(this.tag);
      return { before, after: row };
    });
    const { before, after } = result;
    if (before === after) return { before: before && this.toFile(before), after: after && this.toFile(after) };
    if (options.announce) this.options.announce([this.tag]);
    const file = after && this.toFile(after);
    if (!before) await this.emit("created", file);
    else if (!after) await this.emit("deleted", this.toFile(before));
    else {
      if (before.file_print !== after.file_print) await this.emit("changed", file);
      if (before.meta_print !== after.meta_print) await this.emit("metaChanged", file);
    }
    return { before: before && this.toFile(before), after: file };
  }

  /** One file's folder as a row would hold it, or null when it's gone. */
  private async read(id: string): Promise<OnDisk | null> {
    const folder = join(this.folder, id);
    const names = await readdir(folder).catch(() => null);
    if (!names) return null;
    const files = names.filter((name) => name !== metaFile && !name.startsWith("."));
    const prints = await fingerprints(folder);
    const info = await stat(folder);
    const file = files.length === 1 ? await stat(join(folder, files[0]!)) : null;
    const disk: OnDisk = {
      id, name: files[0] ?? "", folder: "", size: file?.size ?? 0,
      type: files.length === 1 ? Bun.file(join(folder, files[0]!)).type : "",
      meta: "{}", status: "empty", problem: null, file_print: prints.file, meta_print: prints.meta,
      createdAt: info.birthtime.toISOString(),
      updatedAt: new Date(Math.max(info.mtimeMs, file?.mtimeMs ?? 0, prints.metaMtimeMs)).toISOString(),
    };
    const raw = await readFile(join(folder, metaFile), "utf8").catch(() => null);
    let meta: Record<string, unknown> = {};
    if (raw !== null) {
      try {
        const parsed = JSON.parse(raw);
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) throw new Error("meta.json is not an object");
        meta = parsed;
      } catch (error) {
        return { ...disk, status: "malformed", problem: `meta.json: ${(error as Error).message}` };
      }
    }
    const { folder: place, ...fields } = meta;
    disk.folder = typeof place === "string" ? place : "";
    disk.meta = JSON.stringify(fields);
    if (files.length !== 1) {
      return { ...disk, status: "malformed", problem: `A file's folder holds one file and meta.json; found ${files.length}.` };
    }
    if (Object.keys(fields).length === 0) return disk;
    const checked = this.schema.safeParse(fields);
    if (!checked.success) return { ...disk, status: "invalid", problem: toCallError(checked.error).message };
    return { ...disk, status: "ok" };
  }

  // The read-change-write of a file's meta.json, under the lock so two SDK
  // writes can't interleave; an agent's own edit is still last-writer-wins.
  private async rewrite(
    id: string,
    revision: number | undefined,
    change: (folder: string, meta: Record<string, unknown>, name: string) => Promise<Record<string, unknown>>,
  ): Promise<BucketFile<z.infer<S>>> {
    checkID(id);
    const folder = join(this.folder, id);
    await this.options.database.transaction(async () => {
      const row: Row | undefined = (await this.sql.unsafe(`select * from "${this.table}" where id = $1`, [id]))[0];
      if (!row) throw new MethodError("NOT_FOUND", `${this.name} has no file ${id}.`);
      if (revision !== undefined && row.revision !== revision) {
        throw new MethodError("VALIDATION", `${this.name} ${id} changed since it was read (revision ${row.revision}, not ${revision}).`);
      }
      const raw = await readFile(join(folder, metaFile), "utf8").catch(() => "{}");
      await writeMeta(folder, await change(folder, JSON.parse(raw), row.name));
    });
    const { after } = await this.reindex(id);
    return after!;
  }

  private toFile(row: Row): BucketFile<z.infer<S>> {
    return {
      id: row.id,
      name: row.name,
      folder: row.folder,
      path: join(this.folder, row.id, row.name),
      size: row.size,
      type: row.type,
      meta: JSON.parse(row.meta),
      status: row.status,
      problem: row.problem,
      revision: row.revision,
      createdAt: row.created_at,
      updatedAt: row.updated_at,
    };
  }
}

/** Writes meta.json beside a temp copy and renames it over, so a reader never sees half of it. */
async function writeMeta(folder: string, meta: Record<string, unknown>) {
  const temp = join(folder, `.${metaFile}.${crypto.randomUUID()}`);
  await writeFile(temp, JSON.stringify(meta, null, 2) + "\n");
  await rename(temp, join(folder, metaFile));
}

/** Sizes and nanosecond mtimes of a folder's file(s) and of its meta.json, apart. */
async function fingerprints(folder: string) {
  const names = (await readdir(folder).catch(() => [] as string[])).filter((name) => !name.startsWith(".")).sort();
  const file: string[] = [];
  let meta = "";
  let metaMtimeMs = 0;
  for (const name of names) {
    const info = await stat(join(folder, name), { bigint: true }).catch(() => null);
    if (!info) continue;
    const print = `${name}:${info.size}:${info.mtimeNs}`;
    if (name === metaFile) {
      meta = print;
      metaMtimeMs = Number(info.mtimeMs);
    } else file.push(print);
  }
  return { file: file.join("|"), meta, metaMtimeMs };
}

/** Every string and number in a file's meta, for the search index. */
function searchText(value: unknown): string {
  if (typeof value === "string" || typeof value === "number") return String(value);
  if (Array.isArray(value)) return value.map(searchText).join(" ");
  if (value && typeof value === "object") return Object.values(value).map(searchText).join(" ");
  return "";
}

function checkID(id: string) {
  if (!safeName.test(id)) throw new MethodError("VALIDATION", `Not a file id: ${id}`);
}

/** A file's own name: anything a folder can hold except a slash, a leading dot, or meta.json. */
function checkName(name: string) {
  if (!/^[^/\\\0.][^/\\\0]{0,254}$/.test(name) || name === metaFile) {
    throw new MethodError("VALIDATION", `A file name has no slashes, doesn't start with a dot and isn't meta.json: ${name}`);
  }
}
