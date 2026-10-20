import type { RunJournal } from "./journal";

/** Terminal control preserves uncertainty about an attempt's external effects. */
export async function settleInterrupted(journal: RunJournal, code: "CANCELLED" | "EXPIRED") {
  for (const [stepId, step] of Object.entries(journal.state.steps)) {
    if (step.status !== "running") continue;
    await journal.append({
      type: step.policy.kind === "mutation" ? "step.unknown" : "step.failed",
      stepId, attempt: step.attempt,
      error: { code, message: "The workflow stopped before this attempt's outcome was recorded." },
    });
  }
}
