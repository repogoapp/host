import { systemClock, type WorkflowClock } from "./clock";
import { eventInput, JournalError, storedEvent, type EventInput, type JournalEvent } from "./events";
import { applyEvent, initialState, type RunState } from "./state";
import { FileJournalStorage, maxJournalBytes, maxRecordBytes, type JournalStorage } from "./storage";

/** Internal durable history for one run; the snapshot is only a readable projection. */
export class RunJournal {
  private value = initialState();
  private listeners = new Set<() => void>();
  private bytes = 0;
  private queue: Promise<unknown> = Promise.resolve();
  private closing: Promise<void> | null = null;
  private poisoned = false;
  private snapshotFailure: unknown = null;

  private constructor(private storage: JournalStorage, readonly clock: WorkflowClock) {}

  static async open(directory: string, clock: WorkflowClock = systemClock): Promise<RunJournal> {
    return RunJournal.recover(await FileJournalStorage.open(directory), clock);
  }

  static async openExisting(directory: string, clock: WorkflowClock = systemClock): Promise<RunJournal> {
    return RunJournal.recover(await FileJournalStorage.open(directory, false), clock);
  }

  /** The injected storage owns its lock; recovery closes it on failure. */
  static async recover(storage: JournalStorage, clock: WorkflowClock): Promise<RunJournal> {
    const journal = new RunJournal(storage, clock);
    try {
      const bytes = Buffer.from(await storage.readEvents());
      if (bytes.length > maxJournalBytes) throw new JournalError("LIMIT", "The run journal exceeds its size limit.");
      const complete = bytes.lastIndexOf(10) + 1;
      let offset = 0;
      while (offset < complete) {
        const end = bytes.indexOf(10, offset);
        if (end - offset + 1 > maxRecordBytes) throw new JournalError("CORRUPT_JOURNAL", "A committed record exceeds its size limit.");
        try {
          const line = new TextDecoder("utf-8", { fatal: true }).decode(bytes.subarray(offset, end));
          journal.value = applyEvent(journal.value, storedEvent(JSON.parse(line)));
        } catch (error) {
          throw new JournalError("CORRUPT_JOURNAL", `Invalid committed event at byte ${offset}: ${error instanceof Error ? error.message : String(error)}`);
        }
        offset = end + 1;
      }
      if (complete !== bytes.length) await storage.truncateEvents(complete);
      journal.bytes = complete;
      await journal.checkpoint();
      return journal;
    } catch (error) {
      await storage.close();
      throw error;
    }
  }

  onChange(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  }

  get state(): RunState { return structuredClone(this.value); }

  /** A failed snapshot does not invalidate an event already committed to the journal. */
  get snapshotError(): unknown { return this.snapshotFailure; }

  append(input: EventInput): Promise<JournalEvent> {
    if (this.closing) return Promise.reject(new JournalError("CLOSED", "The journal is closing or closed."));
    let captured: EventInput;
    try { captured = structuredClone(eventInput(input)); } catch (error) { return Promise.reject(error); }
    const writing = this.queue.then(async () => {
      if (this.poisoned) throw new JournalError("POISONED", "A journal write failed. Close and recover before continuing.");
      const event: JournalEvent = { ...captured, version: 1, seq: this.value.lastSeq + 1, at: new Date(this.clock.now()).toISOString() };
      const next = applyEvent(this.value, event);
      const record = Buffer.from(JSON.stringify(event) + "\n");
      // A dispatched step needs room for a result, cancellation request and terminal event.
      const reserve = (event.type === "step.started" || event.type === "sleep.scheduled") ? 3 * maxRecordBytes : 0;
      if (record.length > maxRecordBytes || this.bytes + record.length + reserve > maxJournalBytes) {
        throw new JournalError("LIMIT", "The event or run journal exceeds its size limit.");
      }
      try {
        await this.storage.append(record);
      } catch (error) {
        this.poisoned = true;
        throw error;
      }
      this.value = next;
      this.bytes += record.length;
      await this.checkpoint();
      for (const listener of this.listeners) {
        try { listener(); } catch (error) { console.error("Workflow observer failed:", error); }
      }
      return structuredClone(event);
    });
    this.queue = writing.catch(() => {});
    return writing;
  }

  close(): Promise<void> {
    this.closing ??= this.queue.then(async () => {
      try {
        if (!this.poisoned && this.snapshotFailure) {
          await this.checkpoint();
          if (this.snapshotFailure) throw this.snapshotFailure;
        }
      } finally {
        await this.storage.close();
      }
    });
    return this.closing;
  }

  private async checkpoint(): Promise<void> {
    try {
      await this.storage.replaceSnapshot(JSON.stringify(this.value) + "\n");
      this.snapshotFailure = null;
    } catch (error) {
      this.snapshotFailure = error;
    }
  }
}
