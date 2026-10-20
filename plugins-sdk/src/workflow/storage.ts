import { constants } from "node:fs";
import { mkdir, open, rename, type FileHandle } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { acquireOwner } from "./lease";
import { JournalError } from "./events";

export const maxRecordBytes = 1024 * 1024;
export const maxJournalBytes = 64 * 1024 * 1024;

/** Storage methods resolve only after their writes have been flushed. */
export interface JournalStorage {
  readEvents(): Promise<Uint8Array>;
  append(record: Uint8Array): Promise<void>;
  truncateEvents(bytes: number): Promise<void>;
  replaceSnapshot(snapshot: string): Promise<void>;
  close(): Promise<void>;
}

export class FileJournalStorage implements JournalStorage {
  private constructor(private directory: string, private events: FileHandle, private release: () => Promise<void>) {}

  static async open(directory: string, create = true): Promise<FileJournalStorage> {
    directory = resolve(directory);
    const created = create ? await mkdir(directory, { recursive: true, mode: 0o700 }) : undefined;
    const ancestor = created ? dirname(resolve(created)) : dirname(directory);
    for (let parent = dirname(directory); ; parent = dirname(parent)) {
      await syncDirectory(parent);
      if (parent === ancestor) break;
    }
    const release = await acquireOwner(join(directory, "owner.db"));
    let events: FileHandle | undefined;
    try {
      events = await open(join(directory, "events.jsonl"), constants.O_RDWR | constants.O_APPEND | (create ? constants.O_CREAT : 0), 0o600);
      await events.sync();
      await syncDirectory(directory);
      return new FileJournalStorage(directory, events, release);
    } catch (error) {
      try {
        await events?.close();
      } finally { await release(); }
      throw error;
    }
  }

  async readEvents(): Promise<Uint8Array> {
    if ((await this.events.stat()).size > maxJournalBytes) throw new JournalError("LIMIT", "The run journal exceeds its size limit.");
    return this.events.readFile();
  }

  async append(record: Uint8Array): Promise<void> {
    await this.events.writeFile(record);
    await this.events.sync();
  }

  async truncateEvents(bytes: number): Promise<void> {
    await this.events.truncate(bytes);
    await this.events.sync();
  }

  async replaceSnapshot(snapshot: string): Promise<void> {
    const temporary = join(this.directory, "state.json.tmp");
    const file = await open(temporary, "w", 0o600);
    try {
      await file.writeFile(snapshot);
      await file.sync();
    } finally {
      await file.close();
    }
    await rename(temporary, join(this.directory, "state.json"));
    await syncDirectory(this.directory);
  }

  async close(): Promise<void> {
    try {
      await this.events.close();
    } finally {
      await this.release();
    }
  }
}

async function syncDirectory(path: string): Promise<void> {
  const directory = await open(path, "r");
  try { await directory.sync(); } finally { await directory.close(); }
}
