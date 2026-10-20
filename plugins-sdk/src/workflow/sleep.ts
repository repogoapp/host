import type { Duration } from "../duration";
import type { WorkflowClock } from "./clock";
import { deadlineFrom } from "./clock";
import { JournalError } from "./events";
import { RunnerError, RunWaiting } from "./errors";
import type { RunJournal } from "./journal";
import { durationMs } from "./policy";
import type { SleepState } from "./state";

/** The scheduled event anchors the wait; replay never calculates a fresh deadline. */
export async function executeSleep(
  id: string, duration: Duration, saved: SleepState | undefined,
  append: RunJournal["append"], clock: WorkflowClock, check: () => void,
): Promise<void> {
  if (saved?.status === "completed") return;
  if (!saved) {
    let milliseconds: number;
    try { milliseconds = durationMs(duration); }
    catch { throw new RunnerError("USAGE", "Sleep duration must be positive and no greater than 365 days."); }
    const event = await append({ type: "sleep.scheduled", sleepId: id, durationMs: milliseconds });
    check();
    throw new RunWaiting(id, 0, deadlineFrom(event.at, milliseconds));
  }
  check();
  if (clock.now() < Date.parse(saved.wakeAt)) throw new RunWaiting(id, 0, saved.wakeAt);
  try { await append({ type: "sleep.completed", sleepId: id }); }
  catch (error) {
    if (error instanceof JournalError && error.code === "NOT_DUE") throw new RunWaiting(id, 0, saved.wakeAt);
    throw error;
  }
  check();
}
