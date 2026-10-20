import "../self";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { Runtime } from "../runtime";
import { MethodError } from "../errors";
import type { TestCall, TestHost } from "../testing";
import { WorkflowService } from "./service";
import { terminalStatuses } from "./summary";
import type { RunSummary } from "./contracts";

/** File-backed fixtures exercise the same pinned executors and durable storage as the host. */
export async function testWorkflowRuntime(directory: string, options: { root?: string; host?: TestHost } = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), "repogo-workflow-test-"));
  let runtime: Runtime | undefined;
  const host = async (_plugin: string, method: string, params: unknown) => {
    if (!options.host) throw new MethodError("FORBIDDEN", `This test does not serve ${method}.`);
    return options.host(method, params);
  };
  const service = await WorkflowService.open(dataDir, {
    host,
    invalidate: () => {}, notify: () => {}, logs: () => {},
  });
  try {
    runtime = await Runtime.open({ dataDir, root: resolve(options.root ?? directory), host, workflows: request => service.request(request) });
    await runtime.loadDir(resolve(directory));
    service.start();
    return {
      call: async (method: string, input: unknown = {}, call: TestCall = {}) => (await runtime!.call(method, input, call)).result,
      manifest: () => runtime!.manifest(),
      wait: (method: string, id: string, timeout = 10_000): Promise<unknown> => new Promise((resolve, reject) => {
        const finish = () => {
          clearTimeout(timer); remove();
          void runtime!.call(`${method}.get`, { id }).then(value => resolve(value.result), reject);
        };
        const remove = service.observe(summary => {
          if (summary.id === id && (terminalStatuses.has(summary.status) || summary.status === "blocked")) finish();
        });
        const timer = setTimeout(() => { remove(); reject(new Error(`Workflow ${id} did not finish within ${timeout}ms.`)); }, timeout);
        void runtime!.call(`${method}.get`, { id }).then(value => {
          const summary = value.result as RunSummary;
          if (terminalStatuses.has(summary.status) || summary.status === "blocked") finish();
        }, error => { clearTimeout(timer); remove(); reject(error); });
      }),
      close: async () => { await service.close(); runtime!.close(); await rm(dataDir, { recursive: true, force: true }); },
    };
  } catch (error) {
    await service.close(); runtime?.close(); await rm(dataDir, { recursive: true, force: true }); throw error;
  }
}
