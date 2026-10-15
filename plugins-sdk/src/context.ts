import type { Collection } from "./collections";

// What every method and handler receives. Kept small on purpose: it is the
// whole surface an agent has to learn.
export type Context = {
  plugin: string;
  /** The project's checkout, for a plugin that reads the project's files. */
  root: string;
  db: Record<string, Collection<any>>;
  call: (method: string, input?: unknown) => Promise<unknown>;
  emit: (name: string, payload: unknown) => Promise<void>;
  log: (...args: unknown[]) => void;
};
