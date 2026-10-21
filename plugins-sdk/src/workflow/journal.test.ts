import { afterEach, expect, test } from "bun:test";
import { appendFile, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { MemoryStorage } from "./fixtures/memory-storage";
import { stepPolicy } from "./policy";
import { RunJournal } from "./journal";
import { JournalError, type EventInput, type JournalEvent } from "./events";
import { maxRecordBytes, type JournalStorage } from "./storage";

const started: EventInput = { type: "run.started", workflow: "report", input: { url: "https://example.com" }, timeoutMs: 30 * 86_400_000 };
const journalPolicy = stepPolicy({ kind: "query", attempts: 100 });
const epoch = "2026-01-01T00:00:00.000Z";
const clock = () => new Date(epoch);
const journals: RunJournal[] = [];
const directories: string[] = [];
const children: ReturnType<typeof Bun.spawn>[] = [];

afterEach(async () => {
  for (const child of children.splice(0)) {
    child.kill();
    await child.exited;
  }
  for (const journal of journals.splice(0)) await journal.close().catch(() => {});
  for (const path of directories.splice(0)) await rm(path, { recursive: true, force: true });
});

async function directory() {
  const path = await mkdtemp(join(tmpdir(), "repogo-workflow-"));
  directories.push(path);
  return path;
}

async function recover(storage: JournalStorage, now = clock) {
  const journal = await RunJournal.recover(storage, { now: () => now().getTime(), schedule: () => () => {} });
  journals.push(journal);
  return journal;
}

async function open(path: string) {
  const journal = await RunJournal.open(path);
  journals.push(journal);
  return journal;
}



test("reopening reconstructs results and the run's identity without trusting a snapshot", async () => {
  const path = await directory();
  const journal = await open(path);
  await journal.append(started);
  await journal.append({ type: "step.started", stepId: "fetch", attempt: 1, policy: journalPolicy });
  await journal.append({ type: "step.completed", stepId: "fetch", attempt: 1, result: { title: "Report" } });
  const expected = journal.state;
  await journal.close();

  await writeFile(join(path, "state.json"), "broken snapshot");
  const reopened = await open(path);
  expect(reopened.state).toEqual(expected);
  expect(JSON.parse(await readFile(join(path, "state.json"), "utf8"))).toEqual(expected);
  await reopened.close();

  await rm(join(path, "state.json"));
  expect((await open(path)).state).toEqual(expected);
});

test("snapshots and returned events cannot mutate the journal's state", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  const event = await journal.append(started);
  const snapshot = journal.state;
  snapshot.steps.fetch = { status: "failed", attempt: 99, limit: 100, policy: journalPolicy, result: null, error: null, nextAttemptAt: null };
  snapshot.workflow = "other";
  if (event.type === "run.started") event.workflow = "changed";
  expect(journal.state.workflow).toBe("report");
  expect(journal.state.steps).toEqual({});
});

test("append order is serialized and invalid transitions do not consume sequence numbers", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  const results = await Promise.all([
    journal.append(started),
    journal.append({ type: "step.started", stepId: "fetch", attempt: 1, policy: journalPolicy }),
    journal.append({ type: "step.completed", stepId: "fetch", attempt: 1, result: null }),
  ]);
  expect(results.map(event => event.seq)).toEqual([1, 2, 3]);
  await expect(journal.append({ type: "step.started", stepId: "fetch", attempt: 2, policy: journalPolicy })).rejects.toThrow("scheduled retry");
  expect((await journal.append({ type: "run.completed", result: null })).seq).toBe(4);
});

test("a retry retains its due time and cannot start early after recovery", async () => {
  let now = new Date(epoch);
  const storage = new MemoryStorage();
  const journal = await recover(storage, () => now);
  await journal.append(started);
  await journal.append({ type: "step.started", stepId: "fetch", attempt: 1, policy: journalPolicy });
  await journal.append({ type: "step.retryScheduled", stepId: "fetch", attempt: 1, error: { message: "503" }, delayMs: 60_000 });
  await journal.close();
  const reopened = await recover(storage, () => now);
  expect(reopened.state.steps.fetch.attempt).toBe(1);
  expect(reopened.state.steps.fetch.nextAttemptAt).toBe("2026-01-01T00:01:00.000Z");
  await expect(reopened.append({ type: "step.started", stepId: "fetch", attempt: 2, policy: journalPolicy })).rejects.toThrow("not due");
  now = new Date("2026-01-01T00:01:00.000Z");
  await reopened.append({ type: "step.started", stepId: "fetch", attempt: 2, policy: journalPolicy });
  await expect(reopened.append({ type: "step.completed", stepId: "fetch", attempt: 1, result: null })).rejects.toThrow("running attempt");
});

test("a second unfinished step and completion of a running workflow are refused", async () => {
  const journal = await recover(new MemoryStorage());
  await journal.append(started);
  await journal.append({ type: "step.started", stepId: "first", attempt: 1, policy: journalPolicy });
  await expect(journal.append({ type: "step.started", stepId: "second", attempt: 1, policy: journalPolicy })).rejects.toThrow("unfinished");
  await expect(journal.append({ type: "run.completed", result: null })).rejects.toThrow("unfinished");
});

test("terminal state refuses a late completion", async () => {
  const journal = await recover(new MemoryStorage());
  await journal.append(started);
  await journal.append({ type: "step.started", stepId: "fetch", attempt: 1, policy: journalPolicy });
  await journal.append({ type: "run.cancelled" });
  await expect(journal.append({ type: "step.completed", stepId: "fetch", attempt: 1, result: "late" })).rejects.toThrow("not active");
  expect(journal.state.lastSeq).toBe(3);
});

test("invalid input and non-JSON results never reach storage", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  await expect(journal.append({ ...started, extra: true } as EventInput)).rejects.toThrow();
  await journal.append(started);
  await journal.append({ type: "step.started", stepId: "fetch", attempt: 1, policy: journalPolicy });
  await expect(journal.append({ type: "step.completed", stepId: "fetch", attempt: 1, result: BigInt(1) } as unknown as EventInput)).rejects.toThrow();
  expect(journal.state.lastSeq).toBe(2);
});

test("an oversized event is refused without poisoning the next valid append", async () => {
  const journal = await recover(new MemoryStorage());
  await journal.append(started);
  await expect(journal.append({ type: "run.completed", result: "x".repeat(maxRecordBytes) })).rejects.toThrow("size limit");
  expect((await journal.append({ type: "run.completed", result: "ok" })).seq).toBe(2);
});

test("a committed append stays successful if only the snapshot fails", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  storage.failSnapshot = true;
  expect((await journal.append(started)).seq).toBe(1);
  expect(journal.snapshotError).toBeInstanceOf(Error);
  storage.failSnapshot = false;
  await journal.close();
  expect(JSON.parse(storage.snapshot).lastSeq).toBe(1);
  expect((await recover(storage)).state.workflow).toBe("report");
});

test("state and acknowledgement wait until the append has flushed", async () => {
  const entered = Promise.withResolvers<void>();
  const flushed = Promise.withResolvers<void>();
  class DelayedStorage extends MemoryStorage {
    override async append(record: Uint8Array) {
      await super.append(record);
      entered.resolve();
      await flushed.promise;
    }
  }
  const journal = await recover(new DelayedStorage());
  let acknowledged = false;
  const writing = journal.append(started).then(() => { acknowledged = true; });
  await entered.promise;
  try {
    expect(acknowledged).toBe(false);
    expect(journal.state.lastSeq).toBe(0);
  } finally {
    flushed.resolve();
    await writing;
  }
  expect(journal.state.lastSeq).toBe(1);
});

test("a failed append poisons the owner until close and recovery", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  await journal.append(started);
  storage.partialAppend = true;
  await expect(journal.append({ type: "step.started", stepId: "fetch", attempt: 1, policy: journalPolicy })).rejects.toThrow("flush failed");
  await expect(journal.append({ type: "run.cancelled" })).rejects.toThrow("Close and recover");
  expect(journal.state.lastSeq).toBe(1);
  await journal.close();
  storage.partialAppend = false;
  const reopened = await recover(storage);
  expect(reopened.state.lastSeq).toBe(1);
  expect(storage.truncated).not.toBeNull();
  expect((await reopened.append({ type: "run.cancelled" })).seq).toBe(2);
});

test("a complete JSON object without a newline is an incomplete final record", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  await journal.append(started);
  await journal.close();
  const committed = storage.events.length;
  storage.events = Buffer.concat([storage.events, Buffer.from(JSON.stringify({ version: 1, seq: 2, at: epoch, type: "run.cancelled" }))]);
  expect((await recover(storage)).state.lastSeq).toBe(1);
  expect(storage.truncated).toBe(committed);
});

test.each(["invalid JSON\n", '{"version":2}\n', "\n"])("invalid committed content is never truncated: %s", async bad => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  await journal.append(started);
  await journal.close();
  storage.events = Buffer.concat([storage.events, Buffer.from(bad)]);
  await expect(RunJournal.recover(storage, { now: () => clock().getTime(), schedule: () => () => {} })).rejects.toMatchObject({ code: "CORRUPT_JOURNAL" });
  expect(storage.truncated).toBeNull();
  expect(storage.closed).toBe(true);
});

test("a sequence gap or duplicate cannot be mistaken for a valid replay", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  await journal.append(started);
  await journal.close();
  const event: JournalEvent = { version: 1, seq: 3, at: epoch, type: "run.cancelled" };
  storage.events = Buffer.concat([storage.events, Buffer.from(JSON.stringify(event) + "\n")]);
  await expect(RunJournal.recover(storage, { now: () => clock().getTime(), schedule: () => () => {} })).rejects.toMatchObject({ code: "CORRUPT_JOURNAL" });
});

test("invalid UTF-8 in a committed record is corruption, not replacement text", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  await journal.append(started);
  await journal.close();
  storage.events = Buffer.concat([storage.events, Buffer.from([0xc3, 0x0a])]);
  await expect(RunJournal.recover(storage, { now: () => clock().getTime(), schedule: () => () => {} })).rejects.toMatchObject({ code: "CORRUPT_JOURNAL" });
  expect(storage.truncated).toBeNull();
});

test("snapshot failure on close still releases storage ownership", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  storage.failSnapshot = true;
  await journal.append(started);
  await expect(journal.close()).rejects.toThrow("snapshot flush failed");
  expect(storage.closed).toBe(true);
});

test("recovery failure releases the filesystem lock without altering committed bytes", async () => {
  const path = await directory();
  const owner = await open(path);
  await owner.append(started);
  await owner.close();
  const original = await readFile(join(path, "events.jsonl"));
  const damaged = Buffer.concat([original, Buffer.from("broken\n")]);
  await writeFile(join(path, "events.jsonl"), damaged);
  await expect(RunJournal.open(path)).rejects.toMatchObject({ code: "CORRUPT_JOURNAL" });
  expect(await readFile(join(path, "events.jsonl"))).toEqual(damaged);
  await writeFile(join(path, "events.jsonl"), original);
  expect((await open(path)).state.lastSeq).toBe(1);
});

test("close waits for already queued appends and refuses new ones", async () => {
  const storage = new MemoryStorage();
  const journal = await recover(storage);
  const writing = journal.append(started);
  const closing = journal.close();
  await expect(journal.append({ type: "run.cancelled" })).rejects.toMatchObject({ code: "CLOSED" });
  await writing;
  await closing;
  expect(journal.state.lastSeq).toBe(1);
  expect(storage.closed).toBe(true);
  await journal.close();
});

test("the filesystem rejects competing owners and releases ownership on close", async () => {
  const path = await directory();
  const owner = await open(path);
  await owner.append(started);
  await expect(RunJournal.open(path)).rejects.toMatchObject({ code: "LOCKED" });
  await owner.close();
  expect((await open(path)).state.lastSeq).toBe(1);
});

test("a killed owner releases its lock and preserves flushed events", async () => {
  const path = await directory();
  const child = Bun.spawn([process.execPath, fileURLToPath(new URL("fixtures/journal-owner.ts", import.meta.url)), path], {
    stdin: "pipe", stdout: "pipe", stderr: "pipe",
  });
  children.push(child);
  const reader = child.stdout.getReader();
  let ready = "";
  while (!ready.includes("ready\n")) {
    const chunk = await reader.read();
    if (chunk.done) throw new Error(`Journal owner exited before readiness: ${await new Response(child.stderr).text()}`);
    ready += new TextDecoder().decode(chunk.value);
  }
  reader.releaseLock();
  await expect(RunJournal.open(path)).rejects.toBeInstanceOf(JournalError);
  child.kill("SIGKILL");
  await child.exited;
  children.splice(children.indexOf(child), 1);
  const journal = await open(path);
  expect(journal.state.steps.first.result).toEqual({ saved: true });
  expect(journal.state.steps.second.status).toBe("running");
  expect(journal.state.lastSeq).toBe(4);
});

test("filesystem recovery truncates only the interrupted tail", async () => {
  const path = await directory();
  const journal = await open(path);
  await journal.append(started);
  await journal.close();
  const original = await readFile(join(path, "events.jsonl"));
  await appendFile(join(path, "events.jsonl"), Buffer.from([0x7b, 0x22, 0xc3]));
  const reopened = await open(path);
  expect(reopened.state.lastSeq).toBe(1);
  expect(await readFile(join(path, "events.jsonl"))).toEqual(original);
});

test("near the journal cap a new step is refused while a terminal record still fits", async () => {
  const chunks: Buffer[] = [];
  let seq = 0;
  const record = (event: EventInput) => chunks.push(Buffer.from(JSON.stringify({ ...event, version: 1, seq: ++seq, at: epoch }) + "\n"));
  record(started);
  const result = "x".repeat(maxRecordBytes - 512);
  for (let index = 0; index < 63; index += 1) {
    record({ type: "step.started", stepId: `step-${index}`, attempt: 1, policy: journalPolicy });
    record({ type: "step.completed", stepId: `step-${index}`, attempt: 1, result });
  }
  const storage: JournalStorage = {
    async readEvents() { return Buffer.concat(chunks); },
    async append(bytes) { chunks.push(Buffer.from(bytes)); },
    async truncateEvents() { throw new Error("Unexpected truncation"); },
    async replaceSnapshot() {},
    async close() {},
  };
  const journal = await recover(storage);
  await expect(journal.append({ type: "step.started", stepId: "one-more", attempt: 1, policy: journalPolicy })).rejects.toMatchObject({ code: "LIMIT" });
  await journal.append({ type: "run.failed", error: { message: "Journal capacity reached" } });
  expect(JSON.parse(chunks.at(-1)!.toString()).type).toBe("run.failed");
});
