export interface WorkflowClock {
  now(): number;
  schedule(at: number, callback: () => void): () => void;
}

export const systemClock: WorkflowClock = {
  now: () => Date.now(),
  schedule(at, callback) {
    let timer: ReturnType<typeof setTimeout> | undefined;
    let cancelled = false;
    const tick = () => {
      if (cancelled) return;
      const remaining = at - Date.now();
      if (remaining <= 0) callback();
      else timer = setTimeout(tick, Math.min(remaining, 2_147_483_647));
    };
    timer = setTimeout(tick, Math.min(Math.max(0, at - Date.now()), 2_147_483_647));
    return () => { cancelled = true; if (timer !== undefined) clearTimeout(timer); };
  },
};

export function deadlineFrom(at: string, milliseconds: number): string {
  return new Date(Date.parse(at) + milliseconds).toISOString();
}
