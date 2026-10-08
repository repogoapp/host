# agents/codex

Everything the host knows about OpenAI Codex. Live turns talk to the CLI
through `internal/codexappserver`, the `codex app-server` JSON-RPC client; the
rest reads Codex's files on disk or runs a throwaway app server. Shared code in
`internal/agent` never sees Codex's shapes: this package converts them to
`agent.Event`, `agent.Approval` and `agent.Question`.

`agents/claude` is laid out the same way, and the live-turn files have the
same names there.

## Live turns

| File | What it does |
| --- | --- |
| `runner.go` | The `agent.Adapter`. `Send` gets the chat's app server from the `SessionPool` (new or reused), then starts or resumes the chat's thread and runs the turn. |
| `session.go` | One live `codex app-server` holding one thread (`liveSession`), alive across the chat's turns. `send` runs `turn/start` with the turn's model, effort, sandbox and approval policy and waits for `turn/completed`, the process exiting or a stop; `stop` sends `turn/interrupt`, then closes the process after a grace period. |
| `events.go` | Codex's notifications (message and reasoning deltas, `item/started`, `item/completed`, token usage) become `agent.Event`s. Only the chat's own thread speaks; a subagent's prose stays in its own rollout. |
| `permission.go` | Answers Codex's command and file-change approvals. RepoGo's own tools are allowed; the rest become an `agent.Approval` for the phone, with Codex's decisions (`accept`, `acceptForSession`, `decline`, …) as the options. |
| `ask.go` | Codex's `request_user_input` and MCP question forms, as `agent.Question`s. |
| `writerlock.go` | Codex lets one process write a thread at a time, through a lock file under `~/.codex/thread-writer-locks`. Before resuming, `startSession` checks that lock, so a chat open in a terminal or the desktop app fails at once with "open in Codex on your environment" instead of after starting an app server. |

## History on disk

| File | What it does |
| --- | --- |
| `sessions.go` | Reads `~/.codex/sessions/YYYY/MM/DD/rollout-<iso>-<uuid>.jsonl`: lists chats and parses rollouts into events, so terminal sessions show up in RepoGo. |
| `native.go` | Parses a whole rollout with the Rust parser, falling back to the Go one. |
| `turns.go` | Works out which turn each rollout row belongs to, including where turns overlap. |
| `subagent.go` | Finds the rollout a `spawn_agent` call started. |

## Tool calls on the phone

| File | What it does |
| --- | --- |
| `toollabel.go` | Each Codex tool call's icon and text ("Ran `npm test`", "Edited file"). A code cell that calls one tool reads as that tool. |
| `codecell.go` | Reads the `tools.<name>(…)` calls inside a Codex code cell, so `toollabel.go` can name them. |

## Models and settings

| File | What it does |
| --- | --- |
| `models.go` | The model picker: `model/list` for models and efforts, the sandboxes Codex reports, plus the fixed approval policies, plan mode and personalities. |
| `cli.go` | One-shot calls on a throwaway app server; the model list, usage and resets use it. |

## Account and usage

| File | What it does |
| --- | --- |
| `codex.go` | The package's starting point: the `Provider` (install details, Codex's release downloads) and which account is signed in. |
| `home.go` | Where Codex's config lives: `~/.codex` or `CODEX_HOME`. |
| `login.go` | Reads the page and code from `codex login --device-auth` so the phone can show them. |
| `usage.go` | Rate limits and how much of each is used, from `account/rateLimits/read` first, then the rollouts. |
| `resets.go` | Spends one of the account's rate-limit reset credits. |
| `shipping.go` | Token counts from the rollouts, for the usage and cost history; a fork's copied history isn't counted twice. |

## Hooks and MCP

| File | What it does |
| --- | --- |
| `hooks.go` | Appends the helper to `hooks.json` beside the user's own hooks, and removes only ours on uninstall; Codex runs them once the user trusts them. |
| `mcp.go` | Reads the `[mcp_servers.<name>]` tables in Codex's `config.toml` for the MCP screen. |

`testdata/` holds a sample rollout for the tests.
