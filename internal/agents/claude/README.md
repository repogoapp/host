# agents/claude

Everything the host knows about Claude Code. Live turns talk to the CLI
through `internal/claudecode`, the stream-json client; the rest reads Claude's
files on disk or runs one-shot commands. Shared code in `internal/agent` never
sees Claude's shapes: this package converts them to `agent.Event`,
`agent.Approval` and `agent.Question`.

`agents/codex` is laid out the same way, and the live-turn files have the same
names there.

## Live turns

| File | What it does |
| --- | --- |
| `runner.go` | The `agent.Adapter`. `Send` gets the chat's Claude process from the `SessionPool` (new or reused) and runs the turn; it passes the process its MCP servers and environment. |
| `session.go` | One live Claude process (`liveSession`). `send` writes the prompt and waits for the result, the process exiting or a stop; `stop` interrupts, then closes the process after a grace period. |
| `prompt.go` | The user message for Claude, built from the `TurnRequest`: the prompt and any attachments. |
| `events.go` | What Claude streams (text, thinking, tool calls, results, usage) becomes `agent.Event`s. |
| `tasks.go` | Tracks background tasks and subagents, so a turn isn't treated as finished or cleaned up while they still run. |
| `permission.go` | Answers Claude's `can_use_tool`. RepoGo's own tools are allowed; the rest become an `agent.Approval` for the phone, and the answer goes back to Claude. Also builds the plan-mode exit options. |
| `permission_options.go` | Claude's suggested "always allow" rules and their labels, like "Yes, and don't ask again for `npm test`". |
| `ask.go` | `AskUserQuestion` and MCP question forms, as `agent.Question`s. |
| `diagnostics.go` | Trims a failed process's stderr so the error shown is readable. |

## History on disk

| File | What it does |
| --- | --- |
| `sessions.go` | Reads `~/.claude/projects/<encoded-cwd>/<session>.jsonl`: lists chats and parses transcripts into events, so terminal sessions show up in RepoGo. |
| `claudeblock.go` | Decodes the content blocks in a transcript line. |
| `native.go` | Parses a whole transcript with the Rust parser, falling back to the Go one. |
| `turns.go` | Works out which turn each transcript row belongs to. |
| `subagent.go` | Finds a subagent's transcript from the tool call that started it. |

## Tool calls on the phone

| File | What it does |
| --- | --- |
| `toollabel.go` | Each Claude tool call's icon and text ("Reading file", "Ran `npm test`"), plus todo lists and subagent types. |

## Models and settings

| File | What it does |
| --- | --- |
| `models.go` | The model picker: asks the CLI for its models, effort levels and fast mode, plus the fixed permission modes and plan mode. |
| `contextwindow.go` | Each model's context window, because Claude doesn't record it. |
| `cli.go` | One-off `claude -p` calls, such as drafting a pull request description; the model list uses one too. |

## Account and usage

| File | What it does |
| --- | --- |
| `claude.go` | The package's starting point: the `Provider` (install details, where the binary lives) and which account is signed in. |
| `home.go` | Where Claude's config lives: `~/.claude` or `CLAUDE_CONFIG_DIR`. |
| `login.go` | Reads the sign-in link from `claude auth login` so the phone can open it. |
| `usage.go` | Plan limits and how much of each is used, from Anthropic's usage endpoint and the local credential cache. |
| `shipping.go` | Token counts from the transcripts, for the usage and cost history. |

## Hooks and MCP

| File | What it does |
| --- | --- |
| `hooks.go` | Registers RepoGo's hook in Claude's `settings.json`, so a turn started in a terminal still drives Live Activities and notifications. |
| `mcp.go` | Reads the MCP servers configured for Claude (`.claude.json`, `.mcp.json`) for the MCP screen. |

`testdata/` holds a recorded stream-json session and a sample transcript for
the tests.
