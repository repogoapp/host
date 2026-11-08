# Host migrations

How the host's data and its contract with the phones change between versions, and what each
kind of change costs. Read this before touching a schema, a method's params or result, or an
event. `AGENTS.md` has the short rules; this is the why and the how.

## The three databases

All three live in `~/.repogo/`. They are treated differently because only one of them can be
rebuilt.

| File | What it holds | Can it be rebuilt? | On a schema change |
| --- | --- | --- | --- |
| `cache.db` | Chats, events, the project list: what `session` parsed from the agents' transcripts | Yes, from the transcripts | Deleted and rebuilt |
| `state.db` | What the user made: pins, names, marks, queues, schedules, action runs | **No** | Columns added; nothing ever dropped |
| `usage.db` | The token ledger (one row per model request) and `activity` (devices' screen time and prompting, and agents' work, as spans) | **No**: rows outlive the transcripts, and screen time exists nowhere else | Columns added; a version bump re-reads the transcripts |

### `cache.db`: change the schema, the host rebuilds

`internal/store/schema.sql` is the only description. `store.go` hashes the file into
`PRAGMA user_version` (`schemaVersion`); when a host opens a cache with another hash it deletes
the file and resyncs from the transcripts (`openCurrent`). So:

- **Any edit to `schema.sql` is the whole migration.** Add, rename or drop freely. No migration code.
- **It costs a full resync** on each host's first start after the update: every transcript is
  read again (about 6 GB on a busy Mac, a few minutes). The chat list fills back in while it runs.
- **Nothing the user made is lost**: marks live in `state.db`, which is attached, not deleted.
- **Never put user data in it.** Anything that can't be re-derived from a transcript or the
  disk belongs in `state.db`.

### `state.db`: add only, never drop

`internal/store/state.sql` runs on every open, and `openState` reconciles the file with it:

| The file has | `openState` |
| --- | --- |
| A column `state.sql` gained | Adds it. It must be nullable (no `NOT NULL`, not a key) |
| A column with another type, nullability or key than `state.sql` says | **Refuses to start**: a rename or retype is a migration of its own |
| A table or column `state.sql` doesn't name | Keeps it, logs a warning |

Why extra columns are kept rather than dropped:

- **Rollbacks.** A failed update rolls the host back (`UpdateFailed`). The newer version may have
  added a column before it failed; the older one must still start and must not destroy those rows.
- **A rename reads as drop + add.** Dropping automatically would empty the column the rename was for.
- **SQLite can't drop an indexed or keyed column** without rebuilding the table.

So, in practice:

- **Adding a field:** add a nullable column to `state.sql`. Done.
- **A misleading name:** keep the column, rename it in Go, and say so in the `state.sql` comment
  (`project_marks.pinned_at` held a pin's place and is now read only as "pinned").
- **Removing a field:** stop using it and delete it from `state.sql`. The column stays in old files,
  unread. That's fine.
- **A real reshape** (a rename that must happen, a type change, splitting a table): add a
  `user_version` migration runner at that point, append-only, numbered. Not before.

### `usage.db`: the ledger is history

`internal/shipping/ledger.go`. Its rows outlive the transcripts they came from (Claude Code deletes
transcripts after 30 days by default), so **purging it loses every month the agents have already
deleted**. Never delete it to "migrate".

- **Adding a column:** `CREATE TABLE` names it for new files; `addColumn` adds it to older ones.
  Give it a default.
- **A parser rule changed** (a new field read from transcripts, a fix): bump `ledgerVersion`. The
  host forgets its file fingerprints and re-reads every transcript, correcting rows in place. Rows
  whose transcript is gone keep their old values (for `cwd`, empty: they count in totals, in no project).

### `activity`: spans, never totals

`internal/shipping/activity.go`. One table for every kind, since they are always read on one timeline: `user` (a device's screen time), `prompt` (the user typing, dictating or talking to the voice agent, with `input` naming which) and `agent` (an agent working on a turn). The per-kind columns are documented in the schema string, and CHECKs hold the rules between them.

- **Who writes is fenced in code.** `user` and `prompt` rows only through `usage.record_time`, each device extending only its own rows of the same kind (`kind = excluded.kind AND actor = caller`); `agent` rows only from the chat-status hook (`Store.OnStatus` → `Ledger.AgentStatus`). Clearing deletes `user` and `prompt` rows.
- **No stop has to arrive.** A device's span ends at its last checkpoint; an agent span left open by a stopped host is closed at start at its chat's last sign of life (`CloseOpenAgents`), and an interrupt closes one at the turn's last sign of life, never at the settling.
- **Grouping is read-time.** No repository column: `root_commit` scoping goes through `Store.Checkouts`.

## The wire: old and new phones at once

Phones and hosts update separately. A phone talks to hosts older and newer than itself, so every
contract change must work in both directions.

### Adding a field to a result or event

- **Tag it `omitempty`** in Go. The generator then makes it optional on the phone (`String?`,
  `decodeIfPresent`; `= null` on Android).
  - **Without it, a new phone breaks on old hosts**: the generated Swift decodes plain fields as
    required, so a missing key fails the whole reply (the project list vanishes).
- **Old phones are fine either way**: Swift's decoder skips keys it doesn't know; Android's JSON is
  `ignoreUnknownKeys = true`.

### Adding a param

- **Make it optional** (`omitempty`) and keep the old behaviour when it's absent. Old phones won't send it.
- **An older host ignores a param it doesn't know**, and answers as if it weren't there. If that
  answer would be wrong rather than just less detailed, gate the param on a capability.

### Capabilities

`host.info` lists them (`internal/hostinfo`). Add one when a phone must know a host can do something
before asking, because an older host would silently answer differently.

- Example: `usage_projects`. An older host ignores `usage.history`'s `cwd` and returns the whole
  environment's usage, which a phone would show as one project's. The phone asks for a project only
  when the host lists the capability, and otherwise says "Update This Environment".
- Capability strings are wire values: never rename one.
- `HostState.supports` gives an environment that hasn't answered yet the benefit of the doubt
  (offline hosts fail the call anyway).

### Regenerating the contract

After changing a method's params or result, or an event's payload:

```sh
go test ./internal/rpc/conformance -update   # methods: docs/methods, host-methods.md, HostAPI.swift, HostAPI.kt, HostAPI.ts
go test ./internal/emit -update              # events: docs/events, host-events.md, HostEvents.swift
```

- **Types shared by an event live in `HostEvents.swift`.** `store.Project` is one: a new project
  field needs the `emit` update too, or the phone never sees it.
- **The generators write what the working tree says.** Other sessions' unfinished changes in
  the same files come along. Commit the regenerated files together with the code they describe.
- Then run RemoteTransport's tests (`swift test --package-path apps/swift/Packages/RemoteTransport`):
  `HostMethodExampleTests` decodes every example with the app's types.

## When a host updates

What a user sees on the first start after an update, so a release can say so:

- **`schema.sql` changed:** the chat list resyncs (minutes on a busy machine).
- **`ledgerVersion` bumped:** usage answers carry `complete: false` ("still indexing") until every
  transcript is read again.
- **`state.sql` gained a column:** nothing visible.

Every environment pays for its own update: the Mac, each container, each machine. Batch schema
changes into one release when you can, so the resync and the re-read happen once.

## Checklist

- [ ] **`schema.sql` changed?** Fine; expect a resync, and say so in the release notes.
- [ ] **`state.sql` changed?** Only nullable columns added; nothing renamed or retyped.
- [ ] **Ledger changed?** Column added with a default. If the parsers changed, `ledgerVersion`
      bumped.
- [ ] **New result or event field?** Tagged `omitempty`.
- [ ] **New param?** Optional. If an older host would answer *wrongly* without it, a capability gates it.
- [ ] **Contract regenerated?** `conformance -update`, plus `emit -update` for event types.
      RemoteTransport tests pass.
- [ ] **Phone built against both?** It decodes an old host's reply, and an old phone decodes the new one.
