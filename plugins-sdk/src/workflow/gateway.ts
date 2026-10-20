import { MethodError, toCallError } from "../errors";
import type { WorkflowCallbacks, WorkflowRequest } from "./contracts";
import { WorkflowService } from "./service";

/** Unavailable workflow storage must not prevent ordinary plugins from serving. */
export class WorkflowGateway {
  private service: WorkflowService | null = null;
  private opening: Promise<void> | null = null;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private started = false;
  private closed = false;
  private error = "Workflow storage is unavailable.";
  private constructor(private directory: string, private callbacks: WorkflowCallbacks) {}
  static async open(directory: string, callbacks: WorkflowCallbacks) {
    const gateway = new WorkflowGateway(directory, callbacks);
    await gateway.connect();
    return gateway;
  }
  private connect(): Promise<void> {
    if (this.service || this.closed) return Promise.resolve();
    if (this.opening) return this.opening;
    this.opening = WorkflowService.open(this.directory, this.callbacks).then(service => {
      this.service = service;
      if (this.started) service.start();
    }, error => { this.error = toCallError(error).message; }).finally(() => { this.opening = null; });
    return this.opening;
  }
  start() {
    if (this.closed) return;
    this.started = true;
    this.service?.start();
    if (this.timer || this.service) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.connect().then(() => this.start());
    }, 5000);
  }
  async request(request: WorkflowRequest) {
    if (this.closed) throw new MethodError("INTERNAL", "The workflow service is closing.");
    await this.connect();
    if (!this.service) throw new MethodError("INTERNAL", `Workflow storage is unavailable: ${this.error}`);
    return this.service.request(request);
  }
  async close() {
    this.closed = true;
    if (this.timer) clearTimeout(this.timer);
    await this.opening;
    await this.service?.close();
  }
}
