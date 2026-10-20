import { afterEach, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import "../self";
import { Runtime } from "../runtime";
import { WorkflowService } from "./service";
import { WorkflowGateway } from "./gateway";
import { RunJournal } from "./journal";
import { stepPolicy } from "./policy";
import { testWorkflowRuntime } from "./testing";
import type { RunSummary, WorkflowCallbacks } from "./contracts";

const roots: string[] = [];
const closing: (() => Promise<void>)[] = [];
afterEach(async () => { for (const close of closing.splice(0).reverse()) await close(); for (const root of roots.splice(0)) await rm(root, { recursive: true, force: true }); });
const callbacks: WorkflowCallbacks = { host: async () => { throw new Error("No live host in this fixture"); }, invalidate: () => {}, notify: () => {}, logs: () => {} };
async function fixture(version = "A") {
  const root = await mkdtemp(join(tmpdir(), "repogo-workflow-integration-")); roots.push(root);
  const dir = join(root, ".repogo", "plugins");
  await mkdir(join(dir, "reports"), { recursive: true });
  await mkdir(join(dir, "sibling"), { recursive: true });
  await writeFile(join(dir, "reports", "version.ts"), `export const version = ${JSON.stringify(version)};`);
  await writeFile(join(dir, "reports", "app.ts"), `import { definePlugin, workflow, z } from "@repogo/plugins";
import { version } from "./version";
export default definePlugin({ namespace: "fixture.reports", workflows: {
  report: workflow(z.object({ name: z.string() }), async ({ name }, ctx) => {
    const old = await ctx.step.run("saved", () => version);
    const value = await ctx.step.run("read", async ({ attempt }) => {
      if (attempt === 1) throw new Error("retry once");
      return await ctx.call("sibling.version");
    }, { kind: "query", attempts: 2, backoff: { initial: "0.05s", max: "0.05s" } });
    return { name, version: old, sibling: value };
  }),
} });`);
  await writeFile(join(dir, "sibling", "app.ts"), `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({ methods: { version: method(z.object({}), () => ${JSON.stringify(version)}, { kind: "query" }) } });`);
  return { root, dir, dataDir: join(root, "data") };
}
async function open(f: Awaited<ReturnType<typeof fixture>>) {
  const service = await WorkflowService.open(f.dataDir, callbacks);
  const runtime = await Runtime.open({ dataDir: f.dataDir, root: f.root, workflows: request => service.request(request) });
  await runtime.loadDir(f.dir);
  closing.push(async () => { runtime.close(); await service.close(); });
  return { service, runtime };
}
function until(service: WorkflowService, id: string, predicate: (summary: RunSummary) => boolean) {
  return new Promise<RunSummary>((resolve, reject) => {
    const timer = setTimeout(() => { remove(); reject(new Error(`Workflow ${id} did not reach expected state.`)); }, 5000);
    const remove = service.observe(summary => { if (summary.id === id && predicate(summary)) { remove(); clearTimeout(timer); resolve(summary); } });
  });
}

test("public workflow methods pin imports and sibling calls while old and new runs use different code", async () => {
  const f = await fixture();
  const { service, runtime } = await open(f);
  const start = (await runtime.call("reports.report.start", { input: { name: "first" }, key: "first" })).result as RunSummary;
  const waiting = until(service, start.id, summary => summary.status === "waiting");
  await service.tick();
  await waiting;
  await writeFile(join(f.dir, "reports", "version.ts"), 'export const version = "B";');
  await writeFile(join(f.dir, "sibling", "app.ts"), 'import { definePlugin, method, z } from "@repogo/plugins"; export default definePlugin({ methods: { version: method(z.object({}), () => "B", { kind: "query" }) } });');
  const completed = until(service, start.id, summary => summary.status === "completed");
  service.start();
  await completed;
  const old = (await runtime.call("reports.report.get", { id: start.id })).result as any;
  expect(old.result).toEqual({ name: "first", version: "A", sibling: "A" });
  expect((await runtime.call("reports.report.start", { input: { name: "first" }, key: "first" })).result).toMatchObject({ id: start.id });
  await expect(runtime.call("reports.report.start", { input: { name: "different" }, key: "first" })).rejects.toThrow("different input");
  const next = (await runtime.call("reports.report.start", { input: { name: "second" } })).result as RunSummary;
  const finished = until(service, next.id, summary => summary.status === "completed");
  await finished;
  expect(next.buildId).not.toBe(start.buildId);
  expect(((await runtime.call("reports.report.get", { id: next.id })).result as any).result).toEqual({ name: "second", version: "B", sibling: "B" });
}, 15000);

test("a service restart resumes a pinned waiting run even when the mutable plugin source is gone", async () => {
  const f = await fixture();
  let service = await WorkflowService.open(f.dataDir, callbacks);
  const runtime = await Runtime.open({ dataDir: f.dataDir, root: f.root, workflows: request => service.request(request) });
  await runtime.loadDir(f.dir);
  const start = (await runtime.call("reports.report.start", { input: { name: "restart" } })).result as RunSummary;
  const waiting = until(service, start.id, summary => summary.status === "waiting");
  await service.tick(); await waiting;
  await service.close();
  await rm(f.dir, { recursive: true, force: true });
  service = await WorkflowService.open(f.dataDir, callbacks);
  closing.push(async () => { runtime.close(); await service.close(); });
  const completed = until(service, start.id, summary => summary.status === "completed");
  service.start(); await completed;
  const result = await service.request({ operation: "get", dir: f.dir, root: f.root, plugin: "reports", namespace: "fixture.reports", workflow: "report", input: { id: start.id } }) as any;
  expect(result.result).toEqual({ name: "restart", version: "A", sibling: "A" });
}, 15000);

const target = (f: Awaited<ReturnType<typeof fixture>>, id: string) => ({ dir: f.dir, root: f.root, plugin: "reports", namespace: "fixture.reports", workflow: "report", input: { id } });

test("catalog loss recovers completed runs from their journals", async () => {
  const f = await fixture();
  const original = await open(f);
  const start = (await original.runtime.call("reports.report.start", { input: { name: "recovered" } })).result as RunSummary;
  const complete = until(original.service, start.id, s => s.status === "completed");
  original.service.start(); await complete; await original.service.close();
  for (const suffix of ["", "-wal", "-shm"]) await rm(join(f.dataDir, ".workflows", `catalog.db${suffix}`), { force: true });
  const service = await WorkflowService.open(f.dataDir, callbacks); closing.push(() => service.close());
  const result = await service.request({ ...target(f, start.id), operation: "get" }) as any;
  expect(result.status).toBe("completed");
  expect(result.result).toEqual({ name: "recovered", version: "A", sibling: "A" });
  await service.tick();
  expect((await service.request({ ...target(f, start.id), operation: "get" }) as any).status).toBe("completed");
}, 15000);

test("missing journals block execution without recreating history or blocking healthy runs", async () => {
  const f = await fixture(); const { runtime, service } = await open(f);
  const start = (await runtime.call("reports.report.start", { input: { name: "missing" } })).result as RunSummary;
  const path = join(f.dataDir, "fixture.reports", "workflows", start.id, "events.jsonl");
  await rm(path); await service.tick();
  expect((await runtime.call("reports.report.get", { id: start.id })).result).toMatchObject({ status: "blocked" });
  await expect(readFile(path)).rejects.toThrow();
  const next = (await runtime.call("reports.report.start", { input: { name: "healthy" } })).result as RunSummary;
  const complete = until(service, next.id, s => s.status === "completed"); service.start(); await complete;
}, 15000);

test("offline expiry precedes dispatch and retention removes journals and unused builds", async () => {
  const f = await fixture(); let now = Date.now();
  const service = await WorkflowService.open(f.dataDir, callbacks, { clock: { now: () => now, schedule: () => () => {} } });
  const runtime = await Runtime.open({ dataDir: f.dataDir, root: f.root, workflows: request => service.request(request) });
  await runtime.loadDir(f.dir); closing.push(async () => { runtime.close(); await service.close(); });
  const start = (await runtime.call("reports.report.start", { input: { name: "offline" } })).result as RunSummary;
  now += 14 * 86400_000; await service.tick();
  expect((await runtime.call("reports.report.get", { id: start.id })).result).toMatchObject({ status: "expired", steps: [] });
  now += 30 * 86400_000; await service.tick();
  expect((await runtime.call("reports.report.list")).result).toEqual({ items: [], cursor: null });
  await expect(readFile(join(f.dataDir, ".workflows", "builds", start.buildId, "worker.js"))).rejects.toThrow();
});

test("active cancellation persists intent and prevents manual retry", async () => {
  const f = await fixture();
  await writeFile(join(f.dir, "reports", "app.ts"), `import { definePlugin, workflow, z } from "@repogo/plugins";
export default definePlugin({ namespace: "fixture.reports", workflows: {
  report: workflow(z.object({ name: z.string() }), async (_, ctx) => ctx.step.run("wait", async ({ signal }) => {
    await new Promise(resolve => signal.addEventListener("abort", resolve, { once: true })); return "late";
  }, { kind: "query" })),
} });`);
  const { runtime, service } = await open(f);
  const start = (await runtime.call("reports.report.start", { input: { name: "cancel" } })).result as RunSummary;
  const running = until(service, start.id, s => s.currentStep === "wait"); service.start(); await running;
  const cancelled = until(service, start.id, s => s.status === "cancelled");
  await runtime.call("reports.report.cancel", { id: start.id }); await cancelled;
  expect((await runtime.call("reports.report.get", { id: start.id })).result).toMatchObject({ status: "cancelled", cancelRequested: true });
  await expect(runtime.call("reports.report.retry", { id: start.id })).rejects.toThrow("safely failed query");
}, 15000);


test("the file-backed developer helper runs public lifecycle methods", async () => {
  const f = await fixture();
  const app = await testWorkflowRuntime(f.dir, { root: f.root });
  closing.push(() => app.close());
  const run = await app.call("reports.report.start", { input: { name: "helper" } }) as RunSummary;
  expect(await app.wait("reports.report", run.id)).toMatchObject({ status: "completed", result: { name: "helper", version: "A", sibling: "A" } });
});

test("input transforms are applied to raw input once by the pinned handler", async () => {
  const f = await fixture();
  await writeFile(join(f.dir, "reports", "app.ts"), `import { definePlugin, workflow, z } from "@repogo/plugins";
export default definePlugin({ namespace: "fixture.reports", workflows: {
 report: workflow(z.object({name:z.string()}).transform(value => ({name:value.name + "!"})), async input => input),
}});`);
  const app = await testWorkflowRuntime(f.dir); closing.push(() => app.close());
  const run = await app.call("reports.report.start", { input: { name: "once" } }) as RunSummary;
  expect(await app.wait("reports.report", run.id)).toMatchObject({ result: { name: "once!" } });
});

test.each(["confirm", "host"])("unattended workflows refuse unsafe capability paths: %s", async mode => {
  const f = await fixture();
  await writeFile(join(f.dir, "reports", "app.ts"), `import { definePlugin, workflow, method, z } from "@repogo/plugins";
export default definePlugin({ namespace: "fixture.reports", methods: {
 danger: method(z.object({}), () => { throw new Error("should never execute"); }, {confirm:"Delete everything?"}),
}, workflows: {
 report: workflow(z.object({mode:z.string()}), async ({mode}, ctx) => ctx.step.run("work", () => mode === "confirm" ? ctx.call("danger") : ctx.host("actions.run", {}), {kind:${JSON.stringify(mode === "host" ? "query" : "mutation")}})),
}});`);
  let calls = 0;
  const app = await testWorkflowRuntime(f.dir, { host: () => { calls++; return null; } }); closing.push(() => app.close());
  const run = await app.call("reports.report.start", { input: { mode } }) as RunSummary;
  const result = await app.wait("reports.report", run.id) as any;
  expect(result.status).toBe("failed");
  expect(result.error.message).toContain(mode === "confirm" ? "Delete everything?" : "query can't change data");
  expect(calls).toBe(0);
});


test("an executor refuses a journal deleted after admission without recreating history", async () => {
  const f = await fixture(); const { runtime } = await open(f);
  const run = await runtime.call("reports.report.start", { input: { name: "deleted" } });
  const summary = run.result as RunSummary;
  const directory = join(f.dataDir, "fixture.reports", "workflows", summary.id);
  const journal = await RunJournal.openExisting(directory);
  const metadata = journal.state.metadata;
  await journal.close(); await rm(join(directory, "events.jsonl"));
  const errors: string[] = [];
  const worker = Bun.spawn([process.execPath, "--no-env-file", join(f.dataDir, ".workflows", "builds", summary.buildId, "worker.js")], {
    stdin: "ignore", stdout: "ignore", stderr: "ignore", ipc: message => { if (message.type === "error") errors.push(message.message); },
  });
  worker.send({ type: "start", run: metadata, directory, dataDir: f.dataDir });
  expect(await worker.exited).toBe(1);
  expect(errors.length).toBeGreaterThan(0);
  await expect(readFile(join(directory, "events.jsonl"))).rejects.toThrow();
});

test("cancelling an interrupted mutation preserves its unknown effect", async () => {
  const f = await fixture(); const { runtime } = await open(f);
  const run = (await runtime.call("reports.report.start", { input: { name: "interrupted" } })).result as RunSummary;
  const journal = await RunJournal.openExisting(join(f.dataDir, "fixture.reports", "workflows", run.id));
  await journal.append({ type: "step.started", stepId: "write", attempt: 1, policy: stepPolicy() });
  await journal.close(); await runtime.call("reports.report.cancel", { id: run.id });
  expect((await runtime.call("reports.report.get", { id: run.id })).result).toMatchObject({ status: "cancelled", steps: [{ id: "write", status: "unknown" }] });
});

test("an unavailable catalog is isolated and reconnects after its owner releases it", async () => {
  const f = await fixture(); const { service } = await open(f);
  const gateway = await WorkflowGateway.open(f.dataDir, callbacks); closing.push(() => gateway.close());
  await expect(gateway.request({ ...target(f, ""), operation: "list" })).rejects.toThrow("unavailable");
  await service.close();
  expect(await gateway.request({ ...target(f, ""), operation: "list" })).toEqual({ items: [], cursor: null });
});

test("a known network outage survives restart and expires before a healthy heartbeat can erase it", async () => {
  const f = await fixture(); let now = Date.now(); let online = false;
  const options = { clock: { now: () => now, schedule: () => () => {} }, network: () => online };
  let service = await WorkflowService.open(f.dataDir, callbacks, options);
  const runtime = await Runtime.open({ dataDir: f.dataDir, root: f.root, workflows: request => service.request(request) });
  await runtime.loadDir(f.dir); closing.push(async () => { runtime.close(); await service.close(); });
  const run = (await runtime.call("reports.report.start", { input: { name: "network" } })).result as RunSummary;
  const journal = await RunJournal.openExisting(join(f.dataDir, "fixture.reports", "workflows", run.id), options.clock);
  await journal.append({ type: "step.started", stepId: "offline", attempt: 1, policy: stepPolicy({ kind: "query", attempts: 2 }) });
  await journal.append({ type: "step.retryScheduled", stepId: "offline", attempt: 1, error: { message: "waiting" }, delayMs: 20 * 86400_000 });
  await journal.close(); await service.close();
  now += 13 * 86400_000; service = await WorkflowService.open(f.dataDir, callbacks, options); await service.tick();
  expect((await runtime.call("reports.report.get", { id: run.id })).result).toMatchObject({ status: "waiting" });
  now += 86400_000; online = true; await service.tick();
  expect((await runtime.call("reports.report.get", { id: run.id })).result).toMatchObject({ status: "expired", expiryReason: "Environment unavailable beyond the workflow offline limit." });
});

async function sleepingFixture(duration = "0.5s", timeout = "30d") {
  const f = await fixture();
  await writeFile(join(f.dir, "reports", "app.ts"), `import { definePlugin, workflow, z } from "@repogo/plugins";
import { version } from "./version";
export default definePlugin({ namespace: "fixture.reports", kv: { counts: z.number() }, workflows: {
 report: workflow(z.object({ name: z.string(), pause: z.boolean().default(true) }), async ({name, pause}, ctx) => {
  const saved = await ctx.step.run("first", async () => {
    await ctx.kv.counts.set(ctx.runId, (await ctx.kv.counts.get(ctx.runId) ?? 0) + 1);
    return version;
  });
  if (pause) await ctx.sleep(${JSON.stringify(duration)});
  return ctx.step.run("second", async () => ({ name, version: saved, count: await ctx.kv.counts.get(ctx.runId), sibling: await ctx.call("sibling.version") }));
 }, { timeout: ${JSON.stringify(timeout)} }),
}});`);
  return f;
}

test("public ctx.sleep survives service restart and source removal without repeating step one", async () => {
  const f = await sleepingFixture();
  let service = await WorkflowService.open(f.dataDir, callbacks);
  const runtime = await Runtime.open({ dataDir: f.dataDir, root: f.root, workflows: request => service.request(request) });
  await runtime.loadDir(f.dir);
  closing.push(async () => { runtime.close(); await service.close(); });
  const start = (await runtime.call("reports.report.start", { input: { name: "sleeping" } })).result as RunSummary;
  const sleeping = until(service, start.id, s => s.status === "waiting" && s.currentStep === "@sleep:2");
  await service.tick();
  const waiting = await sleeping;
  expect(waiting.attempt).toBe(0);
  expect(waiting.nextAttemptAt).not.toBeNull();
  await service.close();
  const state = await RunJournal.openExisting(join(f.dataDir, "fixture.reports", "workflows", start.id));
  expect(state.state.sleeps["@sleep:2"].wakeAt).toBe(waiting.nextAttemptAt!);
  await state.close();
  await rm(f.dir, { recursive: true, force: true });
  service = await WorkflowService.open(f.dataDir, callbacks);
  const completed = until(service, start.id, s => s.status === "completed");
  service.start(); await completed;
  const result = await service.request({ ...target(f, start.id), operation: "get" }) as any;
  expect(result.result).toEqual({ name: "sleeping", version: "A", count: 1, sibling: "A" });
  expect(result.steps).toMatchObject([{ id: "first", kind: "step" }, { id: "@sleep:2", kind: "sleep", status: "completed" }, { id: "second", kind: "step" }]);
  const lines = (await readFile(join(f.dataDir, "fixture.reports", "workflows", start.id, "events.jsonl"), "utf8")).trim().split("\n").map(line => JSON.parse(line));
  expect(lines.filter(event => event.type === "sleep.scheduled")).toHaveLength(1);
  expect(lines.filter(event => event.type === "sleep.completed")).toHaveLength(1);
}, 15000);

test("sleeping workflows do not exhaust executor slots and another run can finish", async () => {
  const f = await sleepingFixture("1h");
  const service = await WorkflowService.open(f.dataDir, callbacks, { storageLimit: 2 * 1024 ** 3 });
  const runtime = await Runtime.open({ dataDir: f.dataDir, root: f.root, workflows: request => service.request(request) });
  await runtime.loadDir(f.dir); closing.push(async () => { runtime.close(); await service.close(); });
  const sleeping: RunSummary[] = [];
  for (let i = 0; i < 4; i++) sleeping.push((await runtime.call("reports.report.start", { input: { name: `sleep-${i}` } })).result as RunSummary);
  const last = (await runtime.call("reports.report.start", { input: { name: "awake", pause: false } })).result as RunSummary;
  const waits = sleeping.map(run => until(service, run.id, s => s.status === "waiting"));
  const done = until(service, last.id, s => s.status === "completed");
  service.start(); await Promise.all([...waits, done]);
  expect((await runtime.call("reports.report.get", { id: last.id })).result).toMatchObject({ status: "completed", result: { name: "awake", count: 1 } });
  for (const run of sleeping) {
    expect((await runtime.call("reports.report.get", { id: run.id })).result).toMatchObject({ status: "waiting" });
  }
}, 15000);

test("public cancellation stops a sleeping run without dispatching its next step", async () => {
  const f = await sleepingFixture("1h");
  const { runtime, service } = await open(f);
  const start = (await runtime.call("reports.report.start", { input: { name: "cancel-sleep" } })).result as RunSummary;
  const waiting = until(service, start.id, s => s.status === "waiting");
  await service.tick(); await waiting;
  const cancelled = until(service, start.id, s => s.status === "cancelled");
  await runtime.call("reports.report.cancel", { id: start.id }); await cancelled;
  await service.tick();
  expect((await runtime.call("reports.report.get", { id: start.id })).result).toMatchObject({ status: "cancelled" });
  const path = join(f.dataDir, "fixture.reports", "workflows", start.id, "events.jsonl");
  const lines = (await readFile(path, "utf8")).trim().split("\n").map(line => JSON.parse(line));
  expect(lines.some(event => event.type === "step.started" && event.stepId === "second")).toBe(false);
}, 15000);


test("the scheduler expires a sleeping run at its earlier run deadline", async () => {
  const f = await sleepingFixture("5m", "1m"); let now = Date.now();
  const service = await WorkflowService.open(f.dataDir, callbacks, { clock: { now: () => now, schedule: () => () => {} } });
  const runtime = await Runtime.open({ dataDir: f.dataDir, root: f.root, workflows: request => service.request(request) });
  await runtime.loadDir(f.dir); closing.push(async () => { runtime.close(); await service.close(); });
  const run = (await runtime.call("reports.report.start", { input: { name: "deadline" } })).result as RunSummary;
  const waiting = until(service, run.id, s => s.status === "waiting");
  await service.tick(); const asleep = await waiting;
  expect(Date.parse(asleep.nextAttemptAt!)).toBeGreaterThan(Date.parse(asleep.deadlineAt!));
  now += 60_000; await service.tick();
  const result = (await runtime.call("reports.report.get", { id: run.id })).result as any;
  expect(result.status).toBe("expired"); expect(result.nextAttemptAt).toBeNull();
  expect(result.steps.find((step: any) => step.kind === "sleep").nextAttemptAt).toBeNull();
  expect(result.steps.some((step: any) => step.id === "second")).toBe(false);
});
