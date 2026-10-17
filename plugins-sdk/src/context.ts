import type { Collection } from "./collections";

/** An alert for the user's phones: what it says, and the page a tap opens. */
export type Notice = {
  title: string;
  body: string;
  /** A page target, `.repogo/tabs/vercel.md?project=a`; a tap opens it. */
  page?: string;
  /** Alerts sharing a collapse id replace one another; defaults to the plugin and title. */
  collapse?: string;
};

// What every method and handler receives. Kept small on purpose: it is the
// whole surface an agent has to learn.
export type Context = {
  plugin: string;
  /** The project's checkout, for a plugin that reads the project's files. */
  root: string;
  db: Record<string, Collection<any>>;
  call: (method: string, input?: unknown) => Promise<unknown>;
  emit: (name: string, payload: unknown) => Promise<void>;
  /** Sends an alert to every phone paired with this environment. */
  notify: (notice: Notice) => void;
  log: (...args: unknown[]) => void;
};
