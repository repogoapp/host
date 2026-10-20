import { stepPolicy } from "../policy";
import { RunJournal } from "../journal";

const journalPolicy = stepPolicy();
const journal = await RunJournal.open(process.argv[2]!);
await journal.append({ type: "run.started", workflow: "report", buildId: "build-a", input: null, timeoutMs: 30 * 86_400_000 });
await journal.append({ type: "step.started", stepId: "first", attempt: 1, policy: journalPolicy });
await journal.append({ type: "step.completed", stepId: "first", attempt: 1, result: { saved: true } });
await journal.append({ type: "step.started", stepId: "second", attempt: 1, policy: journalPolicy });
await Bun.write(Bun.stdout, "ready\n");
await Bun.stdin.text();
await journal.close();
