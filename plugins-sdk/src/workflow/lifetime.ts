import type { WorkflowClock } from "./clock";
import type { RunJournal } from "./journal";

/** Cancellation is durable before notifying callbacks; expiry already has a recorded deadline. */
export class RunLifetime {
  readonly controller = new AbortController();
  reason: "cancelled" | "expired" | "paused" | null = null;
  private pending: Promise<void> = Promise.resolve();
  private closed = false;
  private failure: { error: unknown } | null = null;
  expiryReason = "Workflow deadline reached.";
  private lastSeen: number;
  private removeHeartbeat: () => void = () => {};
  private removeTimer: () => void;
  private pause = () => {
    if (!this.reason) { this.reason = "paused"; this.controller.abort(new Error("Workflow executor suspended.")); }
  };
  private cancel = () => {
    if (this.reason) return;
    this.reason = "cancelled";
    this.pending = this.journal.append({ type: "run.cancelRequested" }).then(() => {}).catch(error => {
      this.failure = { error };
    }).finally(() => this.controller.abort(new Error("Workflow cancelled.")));
  };

  constructor(private journal: RunJournal, private clock: WorkflowClock, readonly deadlineAt: string | null, private signal?: AbortSignal, private suspend?: AbortSignal, private maxOfflineMs?: number) {
    this.lastSeen = clock.now();
    this.heartbeat();
    this.removeTimer = deadlineAt ? clock.schedule(Date.parse(deadlineAt), () => this.check()) : () => {};
    suspend?.addEventListener("abort", this.pause, { once: true });
    if (suspend?.aborted) this.pause();
    signal?.addEventListener("abort", this.cancel, { once: true });
    if (signal?.aborted) this.cancel();
    this.check();
  }

  private heartbeat() {
    if (this.closed || !this.maxOfflineMs) return;
    this.removeHeartbeat = this.clock.schedule(this.clock.now() + Math.min(5000, this.maxOfflineMs / 2), () => {
      this.check();
      if (!this.reason) this.heartbeat();
    });
  }

  check(): void {
    const now = this.clock.now();
    if (!this.closed && !this.reason && this.maxOfflineMs && now - this.lastSeen >= this.maxOfflineMs) {
      this.reason = "expired";
      this.expiryReason = "Environment unavailable beyond the workflow offline limit.";
      this.controller.abort(new Error(this.expiryReason));
    }
    this.lastSeen = now;
    if (!this.closed && !this.reason && this.deadlineAt && this.clock.now() >= Date.parse(this.deadlineAt)) {
      this.reason = "expired";
      this.controller.abort(new Error("Workflow deadline reached."));
    }
  }

  async flush(): Promise<void> {
    await this.pending;
    if (this.failure) throw this.failure.error;
  }

  async close(): Promise<void> {
    if (!this.closed) {
      this.check();
      this.closed = true;
      this.removeTimer();
      this.removeHeartbeat();
      this.signal?.removeEventListener("abort", this.cancel);
      this.suspend?.removeEventListener("abort", this.pause);
    }
    await this.flush();
  }
}
