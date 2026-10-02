# RepoGo Host

The RepoGo host is the `repogo` binary that runs on your computer, so you can
drive your coding agents from the RepoGo iPhone app. It runs on macOS and Linux.

Your chats and code stay on your computer. Your phone reaches the host through
RepoGo's relay, and everything between them is end-to-end encrypted: the relay
forwards bytes it cannot read and never stores chat or code content.

## Get started

```sh
npx @repogo/host
```

This installs the host as a background service and shows a pairing code. Scan
it with your iPhone camera. If RepoGo isn't installed, an App Clip opens so you
can pair and start chatting right away.

### Requirements

- macOS or Linux (Ubuntu 22.04 or newer), on Apple silicon/arm64 or x64
- Node.js 22 or later, only to run the `npx` command
- Claude Code or Codex installed and signed in on the same computer

### Commands

| Command | What it does |
| --- | --- |
| `npx @repogo/host` | Install and start the host, then show a pairing code |
| `npx @repogo/host pair` | Show a pairing code for another device |
| `npx @repogo/host status` | Show whether the host is running |
| `npx @repogo/host start` / `stop` | Start or stop the background host |
| `npx @repogo/host logs` | Show the host's logs |
| `npx @repogo/host update` | Update to the latest version; refuses while agents, terminals or builds are running (`--now` stops them) |
| `npx @repogo/host power enable` | Keep a Mac awake with its lid closed while a device is paired (one `sudo` prompt) |
| `npx @repogo/host uninstall` | Remove the background service; keeps your pairings and data in `~/.repogo` |

## How it works

The host has three parts.

### Relay connection

The host dials out to RepoGo's relay over a WebSocket; it listens only on
`127.0.0.1`, never on your network. Your phone connects to the same relay,
which routes each message by a small header and never holds a key.

Pairing exchanges Ed25519 identities when you scan the code. Each connection
then runs a Noise XX handshake with an ML-KEM-1024 step
(`Noise_XXhfs_25519+MLKEM1024_AESGCM_SHA256`), so messages are sealed with
AES-GCM and the key exchange is post-quantum. The phone and the host each check
the other's paired identity before anything is sent.

### RPC calls

The phone talks to the host in JSON-RPC 2.0 over that encrypted channel: list
chats, send a prompt, read a file, check git status, open a terminal. The host
pushes live updates (a reply streaming in, a file changing) the same way. Every
method's params and result are listed in [`docs/host-methods.md`](docs/host-methods.md),
and every event in [`docs/host-events.md`](docs/host-events.md).

A few methods, such as starting a pairing, only answer on this computer: they
need the token in `~/.repogo/runtime.json` (readable only by you) and are
refused over the relay.

### Agent runs

The host runs the agent CLIs you already have, signed in as you: Claude Code
through its stream-json mode and Codex through `codex app-server`. Each chat
runs in its project's folder. Turns in one chat run in order; different chats
run at the same time.

The host also reads the Claude Code and Codex sessions you started yourself,
in a terminal or your editor, so their history shows up on your phone.

## What it changes on your computer

- `~/.repogo/` holds the binary, your pairings, a cache of your chat list, and
  logs. Delete it to remove everything.
- A launchd agent (macOS) or a systemd user service (Linux) starts the host at
  login.
- Hooks in `~/.claude/settings.json` and `~/.codex/hooks.json` tell the host
  when you run an agent outside RepoGo. Your own hooks are left untouched, and
  `uninstall` removes only RepoGo's. Codex asks you to trust them once
  (`codex`, then `/hooks`).
- `power enable` adds `/etc/sudoers.d/repogo`, which lets the host run
  `pmset -a disablesleep` and nothing else.

## Uninstall

```sh
npx @repogo/host uninstall
rm -rf ~/.repogo
```

## Build from source

Requires Go (see `go.mod` for the version).

```sh
go build -o ~/.repogo/bin/repogo ./repogo
~/.repogo/bin/repogo serve    # run in the foreground
```

### Testing

```sh
go test ./...
```

`internal/rpc/conformance` runs every method through the in-process router, a
local WebSocket, and a real relay, on temporary data.

### Layout

```
repogo/      the host binary: commands, config, start, update
relay/       the hosted relay that routes encrypted bytes
npm/         the `npx @repogo/host` installer
internal/    the host's packages: transport and encryption, RPC methods,
             agent runners, transcript reading, git, files, terminals
docs/        generated method and event references
```

## Support

- Website: [repogo.app](https://repogo.app)
- Report a problem: [github.com/repogoapp/host/issues](https://github.com/repogoapp/host/issues)

## License

RepoGo is source-available under the [Functional Source License, Version 1.1,
ALv2 Future License](../../LICENSE.md)
(FSL-1.1-ALv2). You can use, modify and share it for anything except a product
or service that competes with RepoGo. Each version becomes available under the
Apache License 2.0 two years after its release.
