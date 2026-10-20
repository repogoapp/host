import type { WorkflowClock } from "../clock";

export class TestClock implements WorkflowClock {
  private time = Date.parse("2026-01-01T00:00:00.000Z");
  private nextId = 0;
  readonly timers = new Map<number, { at: number; callback: () => void }>();

  now() { return this.time; }
  schedule(at: number, callback: () => void) {
    const id = ++this.nextId;
    this.timers.set(id, { at, callback });
    return () => { this.timers.delete(id); };
  }
  advance(ms: number) {
    this.time += ms;
    while (true) {
      const due = [...this.timers.entries()].filter(([, timer]) => timer.at <= this.time).sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) return;
      this.timers.delete(due[0]);
      due[1].callback();
    }
  }
}
