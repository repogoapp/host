import { AsyncLocalStorage } from "node:async_hooks";
import { MethodError } from "./errors";

// Which data a call read and wrote, as tags: `<app>.<collection>`. A query's
// reads are what it depends on; a write's tags invalidate every query that
// read them. Tracked per call, including calls into other apps and the event
// handlers a write sets off, so custom methods need no declarations.

export type Access = {
  reads: Set<string>;
  writes: Set<string>;
  /** Inside a query: nothing may write. */
  readOnly: boolean;
  /** The call returned; later writes (detached work) are announced on their own. */
  done: { value: boolean };
  /** The runtime this call belongs to, which announces its late writes. */
  announce: (tags: string[]) => void;
};

const current = new AsyncLocalStorage<Access>();

export function currentAccess(): Access | undefined {
  return current.getStore();
}

export type Tracked<T> = { ok: true; value: T; access: Access; outermost: boolean } | { ok: false; error: unknown; access: Access; outermost: boolean };

/**
 * Runs `work` tracking its reads and writes. A call inside another adds to the
 * outer one's sets; a query inside a writable call gets a read-only scope over
 * those same sets. The access comes back even when `work` throws, so writes
 * that committed before the error are still announced.
 */
export async function tracked<T>(readOnly: boolean, announce: (tags: string[]) => void, work: () => Promise<T>): Promise<Tracked<T>> {
  const outer = current.getStore();
  const outermost = !outer;
  const access: Access =
    outer && (outer.readOnly || !readOnly)
      ? outer
      : outer
        ? { ...outer, readOnly: true }
        : { reads: new Set(), writes: new Set(), readOnly, done: { value: false }, announce };
  try {
    const value = access === outer ? await work() : await current.run(access, work);
    return { ok: true, value, access, outermost };
  } catch (error) {
    return { ok: false, error, access, outermost };
  } finally {
    if (outermost) access.done.value = true;
  }
}

export function recordRead(tag: string) {
  current.getStore()?.reads.add(tag);
}

/** Called before a write runs: a query, and anything it calls, may not write. */
export function assertWritable(tag: string) {
  if (current.getStore()?.readOnly) {
    throw new MethodError("FORBIDDEN", `A query can't change data (it tried to write ${tag}).`);
  }
}

export function recordWrite(tag: string) {
  const access = current.getStore();
  if (!access) return;
  if (access.done.value) access.announce([tag]);
  else access.writes.add(tag);
}
