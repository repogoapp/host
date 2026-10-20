import { createHash } from "node:crypto";
import { isBuiltin } from "node:module";
import { readdir, readFile, realpath } from "node:fs/promises";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { atomicFile } from "./files";

const abi = 2;
const maxBundleBytes = 32 * 1024 * 1024;
const forbidden = /^(?:\.env(?:\..*)?|environment\.json|\.npmrc|id_rsa|id_ed25519|secrets(?:\..*)?|credentials(?:\..*)?|.*\.(?:pem|key|p12))$/i;
const builtin = (path: string) => path === "bun" || path.startsWith("bun:") || isBuiltin(path);
export type Artifact = { id: string; bytes: Uint8Array; bun: string; abi: number };
const workerSource = fileURLToPath(new URL(import.meta.url.endsWith(".ts") ? "./snapshot-worker.ts" : "./snapshot-worker.js", import.meta.url));

export async function captureArtifact(directory: string): Promise<Artifact> {
  directory = await realpath(directory);
  const sdkRoot = workerSource.endsWith(".ts") ? dirname(dirname(workerSource)) : dirname(workerSource);
  const roots = [directory, sdkRoot];
  for (const base of [directory, sdkRoot]) {
    for (let parent = base; ; parent = dirname(parent)) {
      roots.push(join(parent, "node_modules"));
      if (parent === dirname(parent)) break;
    }
  }

  const entries = (await readdir(directory, { withFileTypes: true }))
    .filter(entry => entry.isDirectory() && /^[a-z0-9][a-z0-9-]*$/.test(entry.name)).sort((a, b) => a.name.localeCompare(b.name));
  const imports = entries.map((entry, index) => `import p${index} from ${JSON.stringify(join(directory, entry.name, "app.ts"))};`);
  const source = `${imports.join("\n")}\nimport { runSnapshot } from ${JSON.stringify(workerSource)};\nrunSnapshot({${entries.map((entry, index) => `${JSON.stringify(entry.name)}:p${index}`).join(",")}});`;
  const result = await Bun.build({
    entrypoints: ["repogo-workflow:entry"], target: "bun", format: "esm", splitting: false,
    env: "disable", packages: "bundle", metafile: true,
    plugins: [{ name: "workflow-snapshot", setup(build) {
      build.onResolve({ filter: /^repogo-workflow:entry$/ }, () => ({ path: "entry", namespace: "workflow" }));
      build.onLoad({ filter: /.*/, namespace: "workflow" }, () => ({ contents: source, loader: "ts", resolveDir: directory }));
      build.onResolve({ filter: /^@repogo\/plugins$/ }, () => ({ path: workerSource }));
      build.onLoad({ filter: /.*/, namespace: "file" }, async args => {
        const path = await realpath(args.path);
        if (!roots.some(root => path.startsWith(root + "/"))) throw new Error("Workflow imports must belong to the plugins directory, SDK or installed dependencies.");
        if (forbidden.test(basename(args.path))) throw new Error("Secrets and environment files cannot be workflow imports.");
        if (!/\.[cm]?[jt]sx?$/.test(args.path)) return undefined;
        const contents = await readFile(args.path, "utf8");
        const loader = /\.tsx$/.test(args.path) ? "tsx" : /\.ts$/.test(args.path) ? "ts" : "js";
        const scanned = new Bun.Transpiler({ loader, minifyWhitespace: true }).transformSync(contents);
        const sdk = args.path.startsWith(workerSource.endsWith(".ts") ? dirname(dirname(workerSource)) + "/" : dirname(workerSource) + "/");
        if (!sdk && /\b(?:import|require)\s*\((?!\s*["'])/.test(scanned)) {
          throw new Error("Workflow imports must use literal module names; computed imports cannot be pinned.");
        }
        if (!sdk && /\bimport\.meta\.(?:dir|url|path)|\b__(?:dirname|filename)\b/.test(scanned)) {
          throw new Error("Workflow modules cannot depend on their source location; use ctx.root for project files.");
        }
        return { contents, loader: /\.tsx$/.test(args.path) ? "tsx" : /\.ts$/.test(args.path) ? "ts" : "js" };
      });
    } }],
  });
  if (!result.success || result.outputs.length !== 1 || !result.metafile) throw new Error("The workflow must build to one self-contained module.");
  for (const input of Object.values(result.metafile.inputs)) {
    for (const dependency of input.imports) {
      if (dependency.external && dependency.kind !== "import-statement" && !builtin(dependency.path)) {
        throw new Error(`Unpinned runtime dependency: ${dependency.path}`);
      }
    }
  }
  const bytes = new Uint8Array(await result.outputs[0]!.arrayBuffer());
  for (const dependency of new Bun.Transpiler({ loader: "js" }).scanImports(bytes)) {
    if (!builtin(dependency.path)) throw new Error(`Unpinned workflow dependency: ${dependency.path}`);
  }
  const output = new TextDecoder().decode(bytes);
  for (const match of output.matchAll(/\b(?:__require\d*|import\.meta\.require)\s*\(\s*(["'])(.*?)\1/g)) {
    if (!builtin(match[2]!)) throw new Error(`Unpinned runtime dependency: ${match[2]}`);
  }
  if (bytes.length > maxBundleBytes) throw new Error("The workflow bundle exceeds 32 MiB.");
  return { id: digest(bytes, Bun.version), bytes, bun: Bun.version, abi };
}
function digest(bytes: Uint8Array, bun: string) {
  return createHash("sha256").update(JSON.stringify({ abi, bun })).update(bytes).digest("hex");
}
export async function publishArtifact(builds: string, artifact: Artifact): Promise<void> {
  await atomicFile(join(builds, artifact.id, "worker.js"), artifact.bytes);
  await atomicFile(join(builds, artifact.id, "manifest.json"), JSON.stringify({ id: artifact.id, bun: artifact.bun, abi: artifact.abi }));
}
export async function verifyArtifact(builds: string, id: string): Promise<string> {
  if (!/^[a-f0-9]{64}$/.test(id)) throw new Error("Invalid workflow build identifier.");
  const path = join(builds, id, "worker.js");
  const manifest = JSON.parse(await readFile(join(builds, id, "manifest.json"), "utf8"));
  const bytes = await readFile(path);
  if (bytes.length > maxBundleBytes || typeof manifest.bun !== "string" || manifest.abi !== abi || manifest.id !== id || digest(bytes, manifest.bun) !== id) {
    throw new Error("The pinned workflow build is missing, incompatible or corrupt.");
  }
  return path;
}
