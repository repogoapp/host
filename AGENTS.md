# RepoGo host (Go)

Applies to everything in this repo. Checked out as `apps/host` inside the RepoGo
app repo, that repo's root `AGENTS.md` and internal notes apply too.

One Go module (`github.com/repogoapp/host`): the `repogo` host binary and the hosted
relay, `hrelay`. `README.md` is the public, customer-facing page. No hosted database and
one small relay is the point; keep it that way.

## Never

- Do not build or test unless asked; say what you changed and what you did not run.
- Do not read, print, or echo env files, secrets, or credentials.
- Do not add legacy shims, compat layers, or migrations. Build forward.
- Do not rename or retype a `state.db` column: `openState` refuses to start on a column whose
  shape differs from `state.sql`, and a rename leaves the old rows in a column nothing reads.
  Add only nullable columns (it adds them on start); fix a misleading name in code and its
  `state.sql` comment instead, as `project_marks.pinned_at` is read only as "pinned". A column or
  table `state.sql` no longer names is kept and logged, never dropped, so a host an update
  rolled back to still starts with a newer host's rows intact. When a real reshape can't be
  done that way, add a `user_version` migration runner then, append-only, not before.
  `HOST-MIGRATIONS.md` covers all three databases and keeping old and new phones working.

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
0.75 MB). Name a new one, then rerun `-update`. The same run writes the Android
client's `HostAPI.kt` (events included) and the desktop client's `HostAPI.ts` from the same types and names.

## Code standards

The least code that does the job. Read the nearest existing examples and match
them; comments explain why, in one line, only where it isn't obvious.

1. Comments state a reason, never a history ("used to", "no longer", "previously").
2. No comment longer than five lines; a longer decision goes in the work's plan.
3. The second copy is the trigger: needing something twice moves the first to a shared place.
4. No code behind a constant `false`, no fields nobody sets or reads, no nil-defaulting of injected deps.
5. Docs move with code: `docs/host-methods.md`, `docs/host-events.md` and the Roles table match `ls internal` and the router. This repo is public: `README.md`, comments and docs carry no plan numbers, internal paths or references to other RepoGo repos.

## Roles — one sentence per layer

| Package | Role | Smell |
| --- | --- | --- |
| `jsonrpc`, `handshake`, `wirecodec`, `tunnelwire` | Wire structs, codes and the per-message codec, no other behaviour; `tunnelwire` is public so the tunnel gateway shares it | any import |
| `chatwire`, `wirejson` | What leaves the host: a chat's transcript as the phone receives it, and required arrays at JSON boundaries | parsing transcripts |
| `wsserver`, `wsconn`, `passthrough`, `hosttls`, `redial`, `hostclient`, `api` | Move bytes: server, shared WebSocket connection, the relay that pipes phones' TLS and the host's link to it, the host's pinned TLS key, the one reconnect loop, Go test client, loopback HTTP onto `rpc` | knows what a chat is |
| `pushgate` | The relay's side of a push: the phone's grant, rate limits, APNs | host state |
| `appattest` | Verify Apple app attestation and assertions for push authorization | app or host state |
| `rpc`, `rpc/<family>` | One method → one core call; typed params and result, access and lifetime on the `rpc.Add` line. Thin | logic a second transport would copy |
| `rpc/registry` | Wiring table only; every service required | needs helper funcs |
| `host` | Build and wire every service (`New`), admit devices (`Listen`), run owned workers (`Start`), stop admission, cancel, wait, then close storage (`Close`) | a second assembly anywhere else |
| `testhost` | `host.New` on temporary dirs with process-level integrations stubbed | wiring of its own |
| `chat` | Chat workflows: send, start, resolve, delete, ship | a transport type |
| `repogo` | Flags, runtime file, production `host.Config`, update/restart, start | over ~200 lines |
| `hostinfo`, `hostsetup`, `hostupdate`, `release`, `service` | The host itself: health, whether the machine is ready, verified releases, self-update with rollback, the background service | chat or project state |
| `agent` | Turn lifecycle and shared CLI mechanics | provider registry or provider dispatch |
| `claudecode`, `codexappserver` | One CLI's own protocol, typed; no RepoGo types | anything the adapter converts |
| `stdiorpc` | Own a child process and newline-delimited JSON-RPC framing | provider methods or chat semantics |
| `agents`, `agents/<provider>` | Construct fresh providers; own each provider's protocol/configuration and home resolution | a shared service importing a provider |
| `clitool`, `clilogin`, `binfetch`, `nodejs`, `agentcatalog`, `agentusage` | The CLIs on this machine: find, install, update and sign in; what each offers (models, modes) and how much of its limits is spent | running a turn |
| `clientprompt`, `catalog`, `toollabel`, `turnsummary` | What a chat shows: the system prompt for the client's renderer, what an environment or project offers to call as text, tool-call labels, the turn recap for chat lists, voice and alerts | a transport type |
| `session` | Shared transcript reading, tailing, and caching; provider interfaces | provider-specific parsing or kind dispatch |
| `store` | `cache.db`, the disposable SQLite cache of what `session` parsed (drop and reparse, never migrate), and `state.db`, what the user made (added to, never wiped; one writer per table) | a version ledger; state in the cache |
| `store` (projects) | The project list's order: by `used_at` (last prompt or pick), then never-used by `activity_at`; pinned projects are always listed, and devices order the pins themselves. A name or pin carries the time the device set it (`name_updated_at`, `pinned_updated_at`); an older write is ignored, and devices show the newest across a project's checkouts | ordering pins on the host; a name or pin without its time |
| `schedule` | The user's schedules: validates them, computes each next run, and on a minute ticker starts every due one as a new chat; rows in `state.db` through `store` | writing SQL itself |
| `chatsync`, `chatlive`, `projectwatch`, `projectsync` | Triggers that call `store.Sync`; the store's one listener announces chat rows. No parsing | a caller publishing a chat row itself |
| `git`, `github` | Git reporting and shipping, `gh` wrapper and PRs. One git runner in `git` | a second `exec.Command("git"` |
| leaf services (`files`, `device`, `notify`, `push`, `liveactivity`, `ports`, `forward`, `terminal`, `actions`, `envsource`, `connect`, `mcp`, `repogomcp`, `plugins`, `tunnel`, `handoff`, `power`, `procscan`, `project`, `projectagent`, `reminder`, `stepactivity`, `uploads`) | One resource each | |
| `shipping` | The usage ledger (`usage.db`, one row per API request, kept after transcripts go; not disposable, a `user_version` bump re-reads every file) and its history and yearly report; provider folders own transcript token parsing | resolving provider homes itself |
| `emit`, `errkind`, `outcome`, `par`, `sqlitedb`, `syscmd`, `apphome`, `devlog` | Shared plumbing, one way each: device events, error kinds, what a plugin method or MCP tool answers, fan-out, SQLite pragmas, short system tools, `~/.repogo`, dev log copies | a second way to do the same |
| `wiredoc`, `clientcontract`, `lint`, `testwait` | Development only: the generated wire docs and the clients' contracts (Swift, Kotlin and TypeScript), the code standards as tests, polling in tests | imported by production code |

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
leaves; `rpc.Tool` offers it to agents and cards, and `rpc.Plugins` lets a
plugin's `ctx.host` call it without that (`mcp.call` behind `ctx.mcp`). Changing a method's types means `go test ./internal/rpc/conformance -update`.

Order: a family's `Register` is the one place its methods are ordered, grouped
by purpose: reads first, then mutations. `Router.Specs()` lists families by
name and each family's methods in the order `Register` adds them, so
`host-methods.md`, the MCP tools file and the Swift contract read like
`Register`; moving an `rpc.Add` line moves them too, so regenerate. In the
Swift contract each type follows the method that first uses it: a method's
params, its result, then the types they hold. Hand-written lists of a family's
methods (`types.go`, `methodExports`, the client's API types and test
decoders) follow the same order, families A–Z with a blank line between.
Code that runs steps in sequence (a test that must create a branch before
switching to it, a store) keeps its own order.

