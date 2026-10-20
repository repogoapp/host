import type { JournalStorage } from "../storage";

export class MemoryStorage implements JournalStorage {
  events = Buffer.alloc(0);
  snapshot = "";
  closed = false;
  failSnapshot = false;
  partialAppend = false;
  truncated: number | null = null;

  async readEvents() { return this.events; }
  async append(record: Uint8Array) {
    this.events = Buffer.concat([this.events, Buffer.from(this.partialAppend ? record.slice(0, 8) : record)]);
    if (this.partialAppend) throw new Error("append flush failed");
  }
  async truncateEvents(bytes: number) { this.truncated = bytes; this.events = this.events.subarray(0, bytes); }
  async replaceSnapshot(snapshot: string) {
    if (this.failSnapshot) throw new Error("snapshot flush failed");
    this.snapshot = snapshot;
  }
  async close() { this.closed = true; }
}
