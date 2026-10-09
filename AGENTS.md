# RepoGo host (Go)

Applies to everything in this repo. Checked out as `apps/host` inside the RepoGo
app repo, that repo's root `AGENTS.md` and internal notes apply too.

One Go module (`github.com/repogo/host`): the `repogo` host binary and the hosted
`relay`. `README.md` is the public, customer-facing page. No hosted database and
one small relay is the point; keep it that way.

## Never

- Do not build or test unless asked; say what you changed and what you did not run.
- Do not read, print, or echo env files, secrets, or credentials.
- Do not add legacy shims, compat layers, or migrations. Build forward.

## Testability

Everything shipped must be testable with repeatable tests on temporary data, never
the developer's real accounts or `~/.repogo`. Entry points: `go test ./...`
here (including `internal/rpc/conformance`) and, in the app repo,
`swift test --package-path ../swift/Packages/RemoteTransport`. The tests that read
the iOS client (`../swift`) skip in a host-only clone. The emit catalog test reads
the client, so renaming an event means changing both sides.
Changing an event's fields means `go test ./internal/emit -update`: it rewrites
`docs/events`, which RemoteTransport's tests decode with the app's types. The
same holds for methods: `go test ./internal/rpc/conformance -update` rewrites
`docs/host-methods.md`, `docs/methods`, and RemoteTransport's
`Generated/HostAPI.swift`: params and result as Swift types for only the methods
the Swift code names as `HostAPI.<method>` (all of them cost the App Clip about
0.75 MB). Name a new one, then rerun `-update`.

## Code standards

The least code that does the job. Read the nearest existing examples and match
them; comments explain why, in one line, only where it isn't obvious.

1. Comments state a reason, never a history ("used to", "no longer", "previously").
2. No comment longer than three lines; a longer decision goes in the work's plan.
3. The second copy is the trigger: needing something twice moves the first to a shared place.
4. No code behind a constant `false`, no fields nobody sets or reads, no nil-defaulting of injected deps.
5. Docs move with code: `docs/host-methods.md`, `docs/host-events.md` and the Roles table match `ls internal` and the router. This repo is public: `README.md`, comments and docs carry no plan numbers, internal paths or references to other RepoGo repos.

## Roles — one sentence per layer

| Package | Role | Smell |
| --- | --- | --- |
| `jsonrpc`, `handshake` | Wire structs and codes, no behaviour | any import |
| `wsserver`, `relay/`, `internal/relay`, `hostlink`, `hostclient`, `securechan` | Move bytes: server, dumb router, host dial, Go test client, end-to-end seal/open | knows what a chat is |
| `appattest` | Verify Apple app attestation and assertions for push authorization | app or host state |
| `rpc`, `rpc/<family>` | One method → one core call; typed params and result, access and lifetime on the `rpc.Add` line. Thin | logic a second transport would copy |
| `rpc/registry` | Wiring table only; every service required | needs helper funcs |
| `host` | Build and wire every service (`New`), admit devices (`Listen`), run owned workers (`Start`), stop admission, cancel, wait, then close storage (`Close`) | a second assembly anywhere else |
| `testhost` | `host.New` on temporary dirs with process-level integrations stubbed | wiring of its own |
| `chat` | Chat workflows: send, start, resolve, delete, ship | a transport type |
| `repogo` | Flags, runtime file, production `host.Config`, update/restart, start | over ~200 lines |
| `agent` | Turn lifecycle and shared CLI mechanics | provider registry or provider dispatch |
| `claudecode`, `codexappserver` | One CLI's own protocol, typed; no RepoGo types | anything the adapter converts |
| `stdiorpc` | Own a child process and newline-delimited JSON-RPC framing | provider methods or chat semantics |
| `agents`, `agents/<provider>` | Construct fresh providers; own each provider's protocol/configuration and home resolution | a shared service importing a provider |
| `session` | Shared transcript reading, tailing, and caching; provider interfaces | provider-specific parsing or kind dispatch |
| `store` | `cache.db`, the disposable SQLite cache of what `session` parsed (drop and reparse, never migrate), and `state.db`, what the user made (added to, never wiped; one writer per table) | a version ledger; state in the cache |
| `schedule` | The user's schedules: validates them, computes each next run, and on a minute ticker starts every due one as a new chat; rows in `state.db` through `store` | writing SQL itself |
| `chatsync`, `chatlive`, `projectwatch`, `projectsync` | Triggers that call `store.Sync`; the store's one listener announces chat rows. No parsing | a caller publishing a chat row itself |
| `git`, `github`, `ship` | Git reporting, `gh` wrapper, PR glue. One git runner in `git` | a second `exec.Command("git"` |
| leaf services (`files`, `device`, `notify`, `push`, `liveactivity`, `ports`, `forward`, `terminal`, `actions`, …) | One resource each | |
| `shipping` | The usage ledger (`usage.db`, one row per API request, kept after transcripts go; not disposable, a `user_version` bump re-reads every file) and its history and yearly report; provider folders own transcript token parsing, Rust mirrors it in `session/native` | resolving provider homes itself |

Errors: a core sentinel says its kind with `errkind.New` (invalid, not found,
denied, unavailable); `rpc.Code` maps the four kinds onto the wire, so the router
never names a package's errors. Client mistakes are invalid, never a silent OK.
Every service is required: `host.New`, `chat.New` and `registry.New` refuse a
missing dependency by name, and tests get the whole graph from `internal/testhost`,
which builds through `host.New`.

Methods: `rpc.Add(r, "family.method", d.handler, opts...)` with a handler
`func(ctx, rpc.Caller, Params) (Result, error)`. Params and results are named,
exported types (`<Method>Params`, `<Method>Result`, `rpc.None`, `rpc.Ack`); a
family with many keeps them in `types.go`. `rpc.Local` and `rpc.Paired` declare
who may call; `rpc.Detached` marks a mutation that must finish if the caller
leaves. Changing a method's types means `go test ./internal/rpc/conformance -update`.

## Native transcript parser

The Go parsers in `internal/agents/{claude,codex}` are canonical; `internal/session/native/rust`
is a Rust port linked with `-tags native` (`make native`). Both must agree on
every event field because store content hashes depend on it: run
`make native-check` after touching either parser. `make native-check-local`
is opt-in: it compares them on the user's own local history, so never in CI.
