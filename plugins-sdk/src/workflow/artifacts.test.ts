import { afterEach, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { captureArtifact, publishArtifact, verifyArtifact } from "./artifacts";

const roots: string[] = [];
afterEach(async () => { for (const root of roots.splice(0)) await rm(root, { recursive: true, force: true }); });
async function source(contents: string) {
  const root = await mkdtemp(join(tmpdir(), "repogo-workflow-build-")); roots.push(root);
  await mkdir(join(root, "plugins", "example"), { recursive: true });
  await writeFile(join(root, "plugins", "example", "app.ts"), contents);
  return { dir: join(root, "plugins"), builds: join(root, "builds") };
}
const plugin = 'export default { methods: {} };';

test.each([
  'const name = process.argv[2]; await import(name);',
  'const name = process.argv[2]; require(name);',
  'console.log(import.meta.dir);',
  'console.log(__dirname);',
  'import "./missing.node";',
  'try { require("unresolved-optional-workflow-package"); } catch {}',
  'try { import.meta.require("unresolved-runtime-workflow-package"); } catch {}',
  'import "./.env";',
])("nonportable imports fail before an executable is published: %s", async prefix => {
  const f = await source(prefix + plugin);
  await expect(captureArtifact(f.dir)).rejects.toThrow();
});

test("artifact verification refuses tampering and accepts its exact saved executable", async () => {
  const f = await source(plugin);
  const artifact = await captureArtifact(f.dir);
  await publishArtifact(f.builds, artifact);
  const path = await verifyArtifact(f.builds, artifact.id);
  await writeFile(path, 'throw new Error("tampered");');
  await expect(verifyArtifact(f.builds, artifact.id)).rejects.toThrow("corrupt");
});
