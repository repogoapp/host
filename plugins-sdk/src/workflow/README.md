# Workflow core

The durable workflow implementation: journal, replay, attempts, lifecycle
and service ownership. Experimental: its contract may still change. Plugin authors use the public
`workflow` helper documented in the SDK README; they do not append events.

```ts
import { RunJournal } from "./journal";
import { stepPolicy } from "./policy";

const journal = await RunJournal.open(runDirectory);
try {
  await journal.append({
    type: "run.started", workflow: "report", input: null,
    timeoutMs: 30 * 86_400_000,
  });
  await journal.append({ type: "step.started", stepId: "fetch", attempt: 1, policy: stepPolicy() });
  await journal.append({
    type: "step.completed", stepId: "fetch", attempt: 1, result: { title: "Report" },
  });
  console.log(journal.state);
} finally {
  await journal.close();
}
```

`events.jsonl` is authoritative. Records include a format version, contiguous
sequence number and UTC timestamp. Appends are serialized, validated against
the previous state, and flushed before resolving. The initial limit is 1 MiB
per record and 64 MiB per journal; active history is never trimmed to make room.
A new attempt reserves three maximum-size records for its result, a cancellation
request and a terminal event.
Validated JSON is normalized before use so initial execution and replay agree.

`state.json` is a readable projection, replaced with a flushed temporary file,
atomic rename and directory flush. Recovery replays the entire journal and
rebuilds this file; a stale, missing or malformed snapshot cannot change a run.
A snapshot failure is exposed as `snapshotError`, while a committed append
still succeeds. Close retries that snapshot and releases ownership even if
the retry fails.

`owner.db` holds an exclusive SQLite transaction solely for OS-managed
ownership, using the SDK's existing Bun SQL support. It stores no workflow
state. The OS releases the lock when its owner dies; it must not be unlinked
to take over an active run. The storage directory must be on a local filesystem
with SQLite locking, atomic rename and directory-flush support.

Recovery removes only a final record without a newline. Invalid UTF-8, JSON,
versions, sequence numbers or transitions in committed records refuse recovery.
An append/flush failure poisons the instance because the disk outcome is
uncertain: close and recover before writing again. A valid record found during
recovery may have reached disk even if its original caller saw an I/O error.

The reducer preserves interrupted running attempts; it does not decide whether
to retry them or classify external effects. Those decisions belong to a runner.
Retries must be scheduled explicitly, increment the attempt by one and cannot
start before their recorded due time. Terminal runs refuse subsequent events.
Inputs and results must be JSON; getters return isolated copies of state.

Tests beside the journal use temporary directories, injected storage failures,
an injected clock and a subprocess killed after flushing its history. Run them
with `bun test src/workflow` from the SDK folder.

## Sequential runner

`runWorkflow` is an internal executor over an already owned journal. It does not
close that journal or load code: its caller supplies the handler, which is the
plugin's current code. The service owns executor processes.

```ts
import { runWorkflow } from "./runner";

const result = await runWorkflow(
  journal,
  { workflow: "report", input: { customer: "one" } },
  async (input, { step }) => {
    const customer = await step.run("customer", async () => ({ name: "Ada" }));
    return await step.run("report", () => ({ title: customer.name }));
  },
);
```

Await every step, use unique stable names and keep side effects inside callbacks.
The handler replays from its beginning; ordinary code outside steps executes again.
The reducer records operation order explicitly, including numeric step names
and automatically identified sleeps. Replay must
visit completed steps in that order before adding new ones. A reordered or
omitted step means the code changed since the run started: the run records
`run.attention` and waits, not terminal and not relaunched, until `run.restarted`
clears its progress or it is cancelled. Duplicate, nested or overlapping steps
fail the run. A caught step error still
stops further dispatch. Automatic retries require an explicitly declared query.

A reopened run between steps reuses saved JSON results without calling those
callbacks. A completed run returns its saved final result without calling its
handler. Identity must match the recorded workflow and input. The generic
return types rely on the caller supplying that same code and result contract.

An interrupted mutation makes the run `unknown` before any handler executes;
its external effects may already have happened. An interrupted query consumes
its recorded attempt and can schedule another within its persisted budget.
Invalid mutation output makes the run unknown; invalid query output fails it. A thrown callback either schedules
a safe query retry or fails its step. Failed and unknown
runs reject with `RunnerError` code `FAILED` or `UNKNOWN` and the stored
`failure`, identically on replay. Waiting runs require a later scheduler.
Append/flush errors propagate without attempting further events or callbacks;
close and recover the journal before deciding its outcome. Snapshot-only errors
retain the journal's committed-success semantics.

A step not yet dispatched when the run stops is recorded failed without calling
its callback. Unawaited-step detection is best effort; authors must await steps.
The executor waits for an outstanding callback before reporting an unawaited-step
failure or releasing its in-process ownership. It cannot forcibly stop a callback;
a callback that never settles keeps that executor occupied. Timeouts and
cancellation are cooperative. The service owns host recovery and discovery.
Callers must not append events to, or close, a journal
while its executor is running.


## Retries, deadlines and cancellation

```ts
const result = await runWorkflow(journal, identity, async (input, { step }) => {
  return await step.run("read", async ({ attempt, attempts, signal, stepId }) => {
    const response = await fetch("https://example.com/report", { signal });
    if (response.status === 404) throw new NonRetryable("Report not found");
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    return await response.json();
  }, {
    kind: "query", attempts: 3, timeout: "30s",
    backoff: { initial: "2s", factor: 2, max: "1m" },
  });
}, { timeout: "30d", signal: cancellation.signal });
```

Import `NonRetryable` from `./runner` alongside `runWorkflow` in core code.
Plugin authors import `NonRetryable` from `@repogo/plugins`.

Attempts are total invocations, starting at one; the default is one. Mutations
are the default kind and cannot request more than one attempt. Query callbacks
run inside the SDK's read-only tracking scope, which refuses SDK data writes;
arbitrary JavaScript and external APIs remain the author's responsibility.
Options are validated before dispatch: 1–100 attempts, positive finite durations
up to 365 days, factor 1–1000 and initial backoff no greater than its cap.
Fractional milliseconds round up. Unknown step options are refused.

Backoff uses full jitter over a capped exponential delay. The sample is taken
once, and `step.retryScheduled` stores its integer `delayMs`. The event timestamp
plus that delay determines `nextAttemptAt`; recovery never samples again.
`RunWaiting` (`code: "WAITING"`) returns control after this durable suspension.
It identifies the waiting step, consumed attempt and due time; the journal stays
`waiting`. This control signal is not a business failure: do not notify failure
or run side effects from a catch outside steps. Catching it cannot complete the
run or dispatch later steps. A caller resumes the same run by invoking
`runWorkflow` again at or after its due time, with the same handler.
Before that time, the handler is not invoked and no event is appended. This
layer holds no worker or timer during backoff; service discovery and its timer
resume the run when due.

The first durable `step.started` records its resolved policy; subsequent attempts
must match it and cannot exceed its budget. Completed-prefix results replay in
order, while the waiting step executes the next attempt. `NonRetryable` stops
immediately. Invalid or oversized results never retry: queries fail with `INVALID_RESULT`,
while mutations persist uncertainty rather than repeat a callback whose result
could not be saved.

Each attempt defaults to a five-minute timeout. It receives a fresh AbortSignal,
and timeout aborts it cooperatively. Ownership and the in-flight gate remain held
until the invocation settles, even if it ignores abort; a callback that never
settles continues to hold that executor. Values returned after timeout are
ignored. A timed-out query can schedule a retry; a timed-out mutation becomes
unknown. Per-attempt timing starts just before invoking the callback, after its durable
start has flushed. A callback that never dispatches is recorded as a known failure.

The run defaults to a 30-day wall-clock lifetime. `run.started.timeoutMs` and its
event timestamp establish `deadlineAt` once, including waits and offline time;
options on resume cannot extend it. On expiry no further step dispatch occurs.
A supplied AbortSignal records `run.cancelRequested` before aborting callbacks;
recovery honors that request before any handler or retry. Cancellation/expiry
wait for active callbacks to settle before terminalizing and replay stable
`CANCELLED`/`EXPIRED` errors. These states never
promise rollback: an unfinished mutation may already have produced effects,
and its running step remains visible. Failed cancellation persistence aborts
cooperatively but propagates the storage error without claiming cancellation.
Listeners and timers are removed before terminal writes. Terminal callbacks
cannot overwrite a cancelled, expired or unknown run.

The journal exposes its `clock`, shared by timestamps, deadlines and timers.
`RunJournal.open(directory, clock)` and `recover(storage, clock)` accept an injected
`WorkflowClock`; runner options accept entropy as `random`. System timers re-arm
in bounded chunks for waits beyond the platform's timer limit. Co-located tests
use a virtual clock and manually settled callbacks; they contain no real sleeps.

## Service and executable ownership

`service.ts` is owned by the plugin server rather than checkout workers. Each
run executes in its own subprocess, `executor.ts` (shipped beside the plugin
runtime as `workflow-executor.js`), which loads the run's plugin folder from
source: a run always executes the current code, and the journal's replay checks
it against what the run recorded. Process exit releases SQLite locks reliably.

The catalog indexes bounded summaries and service availability timestamps.
`run.started.location` supplies identity and policy so missing index entries can
be reconstructed from journals. The service checks offline limits before
refreshing availability; an active executor separately detects long clock gaps.
An unavailable journal blocks the run and does not create a replacement.
Retention is anchored to the terminal event. Cleanup removes the journal before
its catalog reference, so failed deletion remains retryable. Discovery
runs under the same serial ownership as starts.

Cancellation is flushed before an active owner acknowledges it. Shutdown sends
suspension, waits briefly, then terminates subprocesses that ignore abort; it
does not record cancellation. Query recovery consumes an interrupted attempt;
mutation recovery records uncertainty. Manual retry durably extends only a
safely failed query's budget without changing its original policy. Cancel all
cancels each unfinished run of a workflow in the project, optionally one status.
If a callback returns a valid result during cooperative suspension before its
attempt timeout, that result is committed before pausing. Recovery reuses it.
Cancellation and expiry still prevent further dispatch and completion of the run.

`availability.ts` supplies a passive, injectable network signal: no nonlocal
assigned address means known disconnection; unknown observations preserve a
previous outage. It does not probe third parties or prove internet reachability.
A single catalog heartbeat retains network-outage start across restarts. Checks
use the old availability record before replacing it, so reconnect cannot erase
an expired outage. Healthy waiting and retained terminal runs do not reread and
fsync their full journals on every maintenance tick. The gateway isolates an
unavailable catalog from ordinary plugins and retries ownership acquisition.

## Durable sleep

`ctx.sleep(duration)` shares the runner's operation cursor and in-flight guard
with named step callbacks. The reserved `@sleep:<position>` ID comes from that
cursor, not random values or source locations. Step names cannot use that prefix.
The state keeps callback policies/attempts in `steps`, durable waits in `sleeps`,
and their shared replay order in `operationOrder`. `sleep.scheduled` derives
`wakeAt` from the committed event time and duration; `sleep.completed` refuses
early completion. The original wake time wins on replay and each new sleep
reserves journal room for completion, cancellation and terminal events.

Scheduling yields WAITING immediately with no worker or local sleep timer. The
service reuses the existing waiting/nextAttemptAt scheduler and original run
deadline. A sleep can extend past that deadline, but cannot extend the run.
Cancellation, expiry and host suspension use the same control path as retries.
Terminal summaries and detail rows clear pending wake times. Attempt scope
prevents detached or nested step callbacks from claiming workflow positions.

Snapshot version and executable ABI are 2. Old incompatible executables block
explicitly; they never fall back to current plugin code. The JSONL envelope
remains version 1 and snapshots remain rebuildable from the event history.
