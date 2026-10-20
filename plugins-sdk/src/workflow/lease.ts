import { SQL } from "bun";
import { JournalError } from "./events";

/** An OS-managed SQLite lock is released on process death without stale-file deletion. */
export async function acquireOwner(path: string): Promise<() => Promise<void>> {
  const owner = new SQL({ adapter: "sqlite", filename: path });
  try {
    await owner.unsafe("pragma busy_timeout = 0");
    await owner.unsafe("begin exclusive");
  } catch (error) {
    await owner.close();
    if (error instanceof SQL.SQLiteError && [5, 6].includes(error.errno & 0xff)) throw new JournalError("LOCKED", "Another executor owns this storage.");
    throw error;
  }
  let closed = false;
  return async () => {
    if (closed) return;
    closed = true;
    try { await owner.unsafe("rollback"); } finally { await owner.close(); }
  };
}
