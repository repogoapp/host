import { isAbsolute } from "node:path";
import { z } from "zod";
import type { Bucket } from "./buckets";
import type { MethodKind } from "./define";
import { MethodError } from "./errors";
import type { Kv } from "./kv";
import type { Stream } from "./streams";

// The methods every kv, stream and bucket gets, so a plugin that only declares
// data is read and changed by agents and cards with no code: `<plugin>.<name>.<method>`.

export type Generated = {
  kind: MethodKind;
  description: string;
  input: z.ZodType;
  output?: z.ZodType;
  confirm?: string;
  run: (input: any, signal: AbortSignal) => unknown;
};

const limit = z.number().int().positive().max(500).optional();
const ttl = z.union([z.number().positive(), z.string().regex(/^\d+(\.\d+)?[smhd]$/)]).optional();

export function kvMethods(name: string, kv: Kv<any>, schema: z.ZodType): Record<string, Generated> {
  const key = z.string().min(1);
  const entry = z.object({ key: z.string(), value: schema, expiresAt: z.string().nullable(), createdAt: z.string(), updatedAt: z.string() });
  return {
    get: {
      kind: "query", description: `Get one ${name} entry by key, or null`, input: z.object({ key }), output: entry.nullable(),
      run: ({ key }) => kv.entry(key),
    },
    list: {
      kind: "query", description: `List ${name} entries in key order, optionally by prefix`,
      input: z.object({ prefix: z.string().optional(), limit }), output: z.array(entry),
      run: (input) => kv.list(input),
    },
    set: {
      kind: "mutation", description: `Set a ${name} key; ttl ("5m", "1h", "7d" or ms) makes it expire`,
      input: z.object({ key, value: schema, ttl }), output: entry,
      run: ({ key, value, ttl }) => kv.set(key, value, { ttl }),
    },
    delete: {
      kind: "mutation", description: `Delete a ${name} key`, input: z.object({ key }), output: z.null(),
      confirm: "Delete this key?", run: async ({ key }) => (await kv.delete(key), null),
    },
  };
}

export function streamMethods(name: string, stream: Stream<any>, schema: z.ZodType): Record<string, Generated> {
  const key = z.string().min(1);
  const line = z.object({ id: z.string(), key: z.string(), offset: z.number(), at: z.string(), data: schema });
  return {
    tail: {
      kind: "stream", description: `Follow a ${name} key live: its last lines (100 unless last says), then each new one, until it ends`,
      input: z.object({ key, from: z.number().int().min(0).optional(), last: z.number().int().positive().max(500).optional() }),
      run: ({ key, from, last }, signal) => stream.follow(key, { from, last, signal }),
    },
    read: {
      kind: "query", description: `Read a ${name} key's lines from an offset`,
      input: z.object({ key, from: z.number().int().min(0).optional(), limit }), output: z.array(line),
      run: ({ key, ...options }) => stream.read(key, options),
    },
    keys: {
      kind: "query", description: `List ${name} keys, most recently written first`, input: z.object({ limit }),
      output: z.array(z.object({ key: z.string(), lines: z.number(), bytes: z.number(), endedAt: z.string().nullable(), updatedAt: z.string() })),
      run: (input) => stream.keys(input),
    },
    append: {
      kind: "mutation", description: `Append a line to a ${name} key`, input: z.object({ key, data: schema }), output: line,
      run: ({ key, data }) => stream.append(key, data),
    },
    end: {
      kind: "mutation", description: `End a ${name} key: followers stop and appends fail`, input: z.object({ key }), output: z.null(),
      run: async ({ key }) => (await stream.end(key), null),
    },
    delete: {
      kind: "mutation", description: `Delete a ${name} key and its lines`, input: z.object({ key }), output: z.null(),
      confirm: "Delete this stream and its lines?", run: async ({ key }) => (await stream.delete(key), null),
    },
  };
}

export function bucketMethods(name: string, bucket: Bucket<any>, schema: z.ZodObject): Record<string, Generated> {
  const id = z.string().min(1);
  const status = z.enum(["ok", "empty", "invalid", "malformed"]);
  const file = z.object({
    id: z.string(), name: z.string(), folder: z.string(), path: z.string(), size: z.number(), type: z.string(),
    meta: schema.partial(), status, problem: z.string().nullable(), revision: z.number(),
    createdAt: z.string(), updatedAt: z.string(),
  });
  const revision = z.number().int().optional();
  return {
    list: {
      kind: "query", description: `List ${name} files, newest first, optionally in one folder or with one status`,
      input: z.object({ folder: z.string().optional(), status: status.optional(), limit }), output: z.array(file),
      run: (input) => bucket.list(input),
    },
    search: {
      kind: "query", description: `Search ${name} files by name, folder and meta`,
      input: z.object({ query: z.string(), limit }), output: z.array(file),
      run: ({ query, limit }) => bucket.search(query, { limit }),
    },
    get: {
      kind: "query", description: `Get one ${name} file by id, or null`, input: z.object({ id }), output: file.nullable(),
      run: ({ id }) => bucket.get(id),
    },
    // Bytes never ride a call: text for a small file, or a path on the environment to copy.
    put: {
      kind: "mutation", description: `Add a file to ${name}: its name, and text or an absolute path to copy`,
      input: z.object({
        name: z.string(), folder: z.string().optional(), text: z.string().optional(),
        from: z.string().refine(isAbsolute, "from is an absolute path").optional(), meta: schema.optional(),
      }),
      output: file,
      run: async ({ name, folder, text, from, meta }) => {
        if ((text === undefined) === (from === undefined)) throw new MethodError("VALIDATION", "Send text or from, one of them.");
        return bucket.put({ name, folder, meta, body: text ?? Bun.file(from!) });
      },
    },
    update: {
      kind: "mutation", description: `Change fields of a ${name} file's meta; null removes one`,
      input: z.object({ id, meta: z.record(z.string(), z.unknown()), revision }), output: file,
      run: ({ id, meta, revision }) => bucket.update(id, meta, { revision }),
    },
    move: {
      kind: "mutation", description: `Rename a ${name} file or move it to another folder`,
      input: z.object({ id, name: z.string().optional(), folder: z.string().optional(), revision }), output: file,
      run: ({ id, name, folder, revision }) => bucket.move(id, { name, folder }, { revision }),
    },
    delete: {
      kind: "mutation", description: `Delete a ${name} file and its meta`, input: z.object({ id }), output: z.null(),
      confirm: "Delete this file?", run: async ({ id }) => (await bucket.delete(id), null),
    },
  };
}
