# agents/cursor

The host runs the installed official `cursor-agent acp` binary. `internal/cursoragent`
contains typed native protocol messages; `internal/stdiorpc` owns its process and
JSON-RPC framing. No npm SDK, private backend client or separate SDK bridge is used.

| File | Responsibility |
| --- | --- |
| `cursor.go` | Identity, isolated provider environment, installer/updater, browser-login URL parsing and CLI auth status. |
| `runner.go` | Session pool, native new/load, project validation and per-session MCP configuration. |
| `session.go` | Prompt/attachment conversion, turn lifetime, cancellation and in-memory conversation view. |
| `events.go` | Ordered text/thinking/tool conversion and native history replay. |
| `permission.go` | Permission options, question label-to-ID mapping and plan approval callbacks. |
| `models.go` | Model variants/modes advertised by a neutral native session. |
| `sessions.go` | Discover native ACP stores, replay/cached snapshots, change watching, rename and delete. |

Cursor's SQLite store is the durable conversation. Only ACP sessions are listed;
ordinary CLI/IDE session stores have not been shown to interoperate with ACP.
Replay has no original turn timestamps, so those remain unknown. Turn identities
are derived from session ID and user-turn order. Native replay can describe tools
less precisely than live updates; live approval denial is retained separately from
`completed` tool status. Replay does not run a model turn.

The default preserves Cursor's own allowlists and routes permission callbacks to
the phone. The host's explicit auto-accept-edits/full-access settings answer eligible
callbacks with allow-once, without changing Cursor's persistent allowlist. Other
settings such as reasoning/context/fast controls must be selected as Cursor model
variants; unsupported separate controls fail explicitly. New callback methods fail
with JSON-RPC method-not-found rather than receiving an implicit approval.

Focused tests use a fake subprocess and temporary native stores. They cover
same-process turns, process restart/resume, replay deduplication, approval denial,
allow-once, cancellation during approval, question IDs, plan approval, native
rename/delete, catalog IDs and invalid answers. The registry test keeps Claude and
Codex's capabilities intact and adds Cursor's implemented capabilities. Tests are
written but have not been run for this implementation turn.

Next verification: run the focused Go tests, then repeat live question/approval,
resume and cancellation probes against CLI `2026.09.28-64d2043`. The earlier real
probes used `2026.06.26-7079533`; they do not validate this Go implementation.
