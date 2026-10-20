import { mkdir, open, rename, unlink } from "node:fs/promises";
import { dirname } from "node:path";

export async function atomicFile(path: string, body: string | Uint8Array): Promise<void> {
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const temporary = `${path}.${crypto.randomUUID()}.tmp`;
  const file = await open(temporary, "wx", 0o600);
  try {
    await file.writeFile(body);
    await file.sync();
  } catch (error) {
    await unlink(temporary).catch(() => {});
    throw error;
  } finally { await file.close(); }
  await rename(temporary, path);
  const directory = await open(dirname(path), "r");
  try { await directory.sync(); } finally { await directory.close(); }
}
