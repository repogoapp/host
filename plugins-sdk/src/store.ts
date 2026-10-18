import type { Database } from "./database";
import type { EventBus } from "./events";

/** What every kv, stream and bucket is built with: its namespace's file and folder, and the runtime's bus. */
export type StoreOptions = {
  database: Database;
  events: EventBus;
  namespace: string;
  /** `<dataDir>/<namespace>`, the folder for what isn't rows. */
  dir: string;
  /** Tells cards a tag changed, for writes no call is tracking (the sweep, a scan). */
  announce: (tags: string[]) => void;
};

export type StoreKind = "kv" | "streams" | "buckets";

// One named kv, stream or bucket in a namespace. Plugins naming the same one share it,
// so each change emits `<plugin>.<name>.<event>` for every plugin that declared it.
// Its tag is `<namespace>.<kind>.<name>`, apart from collections' `<namespace>.<name>`.
export abstract class Store {
  readonly tag: string;
  readonly plugins = new Set<string>();

  constructor(
    protected options: StoreOptions,
    readonly kind: StoreKind,
    readonly name: string,
  ) {
    this.tag = `${options.namespace}.${kind}.${name}`;
  }

  /** Brings the store's files and table up; called once, when the first plugin declares it. */
  abstract setup(): Promise<void>;

  /** Called every few seconds by the Worker: expiry, retention, reconciling with the disk. */
  abstract tick(now: number): Promise<void>;

  close() {}

  protected get sql() {
    return this.options.database.sql;
  }

  protected async emit(event: string, payload: unknown) {
    for (const plugin of this.plugins) await this.options.events.emit(`${plugin}.${this.name}.${event}`, payload);
  }
}

/** A table name for a store: its kind and name, letters, digits and underscores only. */
export function tableName(kind: StoreKind, name: string) {
  return `${kind}_${name.replace(/[^a-zA-Z0-9_]/g, "_")}`;
}

/** A key or id that is also a file name: no slashes, no leading dot, at most 128 characters. */
export const safeName = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;
