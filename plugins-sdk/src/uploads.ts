import { AsyncLocalStorage } from "node:async_hooks";
import { z } from "zod";

/** A file the phone uploaded for this call, staged on the environment until a day after its last use. */
export type UploadedFile = { id: string; name: string; type: string; size: number; path: string };

/** The files the host attached to the running call; a method it calls sees them too. */
export const callFiles = new AsyncLocalStorage<UploadedFile[]>();

/**
 * One file from an `<Upload>`: `files: z.array(upload())`. It accepts only a
 * file the host attached to this call, so input written by hand can't point a
 * method at another path. Copy what you keep: `put({ body: Bun.file(file.path) })`.
 */
export function upload() {
  return z
    .object({ id: z.string(), name: z.string(), type: z.string(), size: z.number(), path: z.string() })
    .refine((file) => (callFiles.getStore() ?? []).some((held) => held.id === file.id && held.path === file.path), {
      message: "Not a file uploaded for this call.",
    });
}
