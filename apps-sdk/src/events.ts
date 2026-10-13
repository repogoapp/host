export type Event = { name: string; payload: unknown; at: string };
export type Listener = (event: Event) => unknown | Promise<unknown>;

// One bus for every app. Names are `<app>.<collection>.<created|updated|deleted>`
// for writes and `<app>.<method>` for calls.
export class EventBus {
  private listeners = new Map<string, Listener[]>();
  private all: Listener[] = [];

  on(name: string, listener: Listener) {
    const list = this.listeners.get(name) ?? [];
    list.push(listener);
    this.listeners.set(name, list);
  }

  onAny(listener: Listener) {
    this.all.push(listener);
  }

  async emit(name: string, payload: unknown) {
    const event = { name, payload, at: new Date().toISOString() };
    const listeners = [...(this.listeners.get(name) ?? []), ...this.all];
    const results = await Promise.allSettled(listeners.map((listener) => listener(event)));
    for (const result of results) {
      if (result.status === "rejected") console.error(`[events] ${name}:`, result.reason);
    }
  }
}
