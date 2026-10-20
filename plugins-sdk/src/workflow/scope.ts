import { AsyncLocalStorage } from "node:async_hooks";
import type { StepContext } from "./attempts";

export const attemptScope = new AsyncLocalStorage<StepContext>();

export const workflowScope = new AsyncLocalStorage<boolean>();
