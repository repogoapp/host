import { z } from "zod";
import { method, type Effect, type Method, type PluginDefinition } from "./define";
import { MethodError, type ErrorCode } from "./errors";

/** What a host answers for GET /<plugin> at its device address. */
type Listing = {
  description: string | null;
  methods: { name: string; effect: Effect; description: string | null; input: unknown; output: unknown }[];
};

const codes: Record<number, ErrorCode> = { 400: "VALIDATION", 403: "FORBIDDEN", 404: "NOT_FOUND" };

/**
 * A plugin whose methods run on another host: each method `name` opens to
 * `device` at `url` (such as http://192.168.64.1:47444), with its schemas, as
 * a request there. That host keeps the plugin's code, keys and data.
 */
export async function remotePlugin(url: string, name: string): Promise<PluginDefinition> {
  const base = `${url.replace(/\/$/, "")}/${name}`;
  const response = await fetch(base);
  if (!response.ok) throw new Error(`remotePlugin: ${base} answered ${response.status}`);
  const listing = (await response.json()) as Listing;
  const methods: Record<string, Method> = {};
  for (const m of listing.methods) {
    methods[m.name] = method(z.fromJSONSchema(m.input as z.core.JSONSchema.JSONSchema), (input) => call(`${base}/${m.name}`, input), {
      // A method with an approve is never served, so none here is destructive.
      effect: m.effect as "read" | "write",
      description: m.description ?? undefined,
      output: m.output ? z.fromJSONSchema(m.output as z.core.JSONSchema.JSONSchema) : undefined,
    });
  }
  return { description: listing.description ?? undefined, methods };
}

async function call(url: string, input: unknown): Promise<unknown> {
  const response = await fetch(url, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(input) });
  const answer = await response.json().catch(() => null);
  if (response.ok) return answer;
  throw new MethodError(codes[response.status] ?? "INTERNAL", answer?.error ?? `${url} answered ${response.status}`, answer?.fields ?? undefined);
}
