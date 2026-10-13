import type { Collection } from "./collections";

// What every method and handler receives. Kept small on purpose: it is the
// whole surface an agent has to learn.
export type Context = {
  app: string;
  db: Record<string, Collection<any>>;
  call: (method: string, input?: unknown) => Promise<unknown>;
  emit: (name: string, payload: unknown) => Promise<void>;
  log: (...args: unknown[]) => void;
};
