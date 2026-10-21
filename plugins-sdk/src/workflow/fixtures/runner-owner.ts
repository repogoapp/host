import { RunJournal } from "../journal";
import { runWorkflow } from "../runner";

const journal = await RunJournal.open(process.argv[2]!);
try {
  await runWorkflow(journal, { workflow: "report", input: { customer: "one" } }, async (_, { step }) => {
    await step.run("first", () => ({ saved: true }));
    return await step.run("second", async () => {
      await Bun.write(Bun.stdout, "ready\n");
      await Bun.stdin.text();
      return null;
    });
  });
} finally {
  await journal.close();
}
