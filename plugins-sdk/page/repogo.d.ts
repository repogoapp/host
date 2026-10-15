// `window.repogo`: what the RepoGo app gives a project's own HTML pages (a
// tab.json page with a `path`). Not on `url` pages or browser tabs. Calls go
// through the app to the environment's plugins, the same way cards do.
//
// Reference it from a page's script:  /// <reference path="…/repogo.d.ts" />

/** A plugin's refusal: a code, a sentence, and messages by input field ("/title"). */
export type RepogoError = {
  code: string;
  message: string;
  field_errors?: Record<string, string>;
};

/** A method's answer: its result, or the plugin's error. */
export type RepogoAnswer<T = unknown> = { result: T } | { error: RepogoError };

export type Repogo = {
  /**
   * Runs `<plugin>.<method>` (a query or a mutation) once.
   * Resolves with the plugin's result or its refusal; never rejects for a refusal.
   */
  call<T = unknown>(method: string, input?: Record<string, unknown>): Promise<RepogoAnswer<T>>;

  /**
   * Keeps a query live: `onData` runs with its answer now, and again each time
   * a write changes it, from any page, card or agent. Returns a function that
   * ends the subscription; leaving the page ends it too.
   */
  subscribe<T = unknown>(
    method: string,
    input: Record<string, unknown>,
    onData: (result: T | null, error: RepogoError | null) => void,
  ): () => void;
  subscribe<T = unknown>(method: string, onData: (result: T | null, error: RepogoError | null) => void): () => void;

  /** Opens a web page in the project's browser, reusing the tab on that site unless `newTab`. */
  browse(url: string, options?: { newTab?: boolean }): void;
};

declare global {
  interface Window {
    /** Present only on a project's own pages inside the RepoGo app. */
    repogo?: Repogo;
  }
}
