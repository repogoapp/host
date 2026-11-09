# RepoGo Host

The RepoGo host is the `repogo` binary that runs on your computer, so you can
drive your coding agents from the RepoGo iPhone app. It runs on macOS and Linux.

Your chats and code stay on your computer. Your phone reaches the host through
RepoGo's relay, and everything between them is end-to-end encrypted: the relay
forwards bytes it cannot read and never stores chat or code content.

## Get started

```sh
curl -fsSL https://repogo.app/install.sh | sh
```

This installs the host as a background service and shows a pairing code. Scan
it with your iPhone camera. With Node.js 22 or later, `npx @repogo/host` does
the same. Either way `repogo` is linked into `~/.local/bin`, so later commands
are `repogo <command>`; `repogo help` lists them. If RepoGo isn't installed, an App Clip opens so you
can pair and start chatting right away.

### Requirements

- macOS or Linux (Ubuntu 22.04 or newer), on Apple silicon/arm64 or x64
- Node.js 22 or later only for the `npx` install
- Claude Code or Codex on the same computer; RepoGo can install either and sign you in from your phone

### Commands

| Command | What it does |
| --- | --- |
| `repogo` | Install and start the host, then show a pairing code (or, once a device is paired, the status) |
| `repogo pair` | Show a pairing code for another device |
| `repogo status` | Show whether the host is running |
| `repogo devices` / `devices revoke <id>` | List the paired devices, or remove one: its connections close and it can no longer reach the host |
| `repogo devices revoke --all` | Remove every paired device and end any reusable invite (asks first; `--yes` skips) |
| `repogo start` / `stop` / `restart` | Start, stop or restart the background host |
| `repogo start --lan` | Also let phones on the same Wi-Fi connect straight to this machine, skipping the relay. It stays on for whatever network the machine joins, a café's included, so anyone there can reach the port (but can't connect without pairing). `--lan=false` turns it off |
| `repogo start --tailscale=false` | Stop serving phones on this machine's Tailscale address. On by default: phones on your tailnet connect straight to it. Turn it off on a shared or work tailnet, where every member can reach the port |
| `repogo logs` | Show the host's logs |
| `repogo update` | Update to the latest version; refuses while chats, terminals or actions are running (`--now` stops them) |
| `repogo keep-awake enable` | Keep a Mac awake with its lid closed while a device is paired (one `sudo` prompt) |
| `repogo tls` | Show the TLS key phones pin, and its counter |
| `repogo tls rotate` | Replace the host's TLS key, for one that may have leaked; every paired device pairs again |
| `repogo tunnels` / `tunnels close <slug>` | List the public URLs open to your local servers, or close one |
| `repogo urls [<plugin>]` | List the plugin methods that answer plain HTTP: each URL, this machine's or the public one, and whether it needs a key |
| `repogo keys <plugin>` | Print a plugin's key for its HTTP methods that take one, making it the first time; `--new` replaces it, `--delete` removes it, and `repogo keys` lists every plugin's |
| `repogo uninstall` | Remove the background service; keeps your pairings and data in `~/.repogo` |

## How it works

The host has three parts.

### Connections

The host dials out, and on its own listens only on `127.0.0.1` (and, for
plugin methods that opt in, its Apple container networks; see below). Phones reach
it through RepoGo's relay, `h.repogo.app`, over TLS 1.3 that runs
straight from the phone to the host: the relay only pipes the bytes.

Pairing gives the phone the host's identity (an Ed25519 key) and its signed
TLS pin. A phone's own key is Ed25519, or P-256 when it lives in the iPhone's
Secure Enclave.

```
phone ──TLS 1.3──▶ relay ──same bytes──▶ host
        <host-id>.h.repogo.app      dialed back over the host's link
        reads the name, holds no key
```

1. The host holds a link to `h.repogo.app:443`, the relay in
   [`hrelay/`](hrelay/). That TLS ends at the relay with a Let's Encrypt
   certificate, and the host proves its identity by signing a challenge.
2. A phone opens TLS to `<host-id>.h.repogo.app`. The relay reads the server
   name from the ClientHello, sends the host a dial notice with a one-time
   token, and the host dials back with it. The relay then copies bytes both
   ways; it never holds a key for the phone's TLS.
3. The host serves its own certificate. Its P-256 key is made on first run in
   `~/.repogo/tls-key.json` (0600) and a fresh self-signed certificate wraps it
   at each start. The phone pins the key's SPKI SHA-256, signed by the host's
   identity over `repogo-tls-pin-v1`, the host ID, the SPKI and a counter, and
   ignores CAs. `repogo tls rotate` makes a new key under the next counter; no
   phone trusts it, so every device pairs again.
4. TLS is 1.3 only on both ends, so the key exchange is X25519MLKEM768.
5. The phone's hello signs the host's challenge plus 32 bytes exported from
   this TLS session (`EXPORTER-repogo-hello`), so a signature can't be replayed
   on another connection. The host checks it against its paired devices.
6. A hello may ask for compression (`"deflate": true`), and the host's reply
   confirms it. From then on every message, both ways, is a binary frame: one
   codec byte (`0x00` raw, `0x01` deflate), then the message. Each message is
   compressed alone, and only when it is 1 KB or more and shrinks to 90% or
   less; it may not inflate past the 8 MB message limit. Without the flag,
   messages stay plain text frames.

A phone that isn't paired is let in only while a pairing code is live, and only
for one `pair.complete` of its own key. `devices.rotate` moves a paired phone to
a new key, such as one in the Secure Enclave: both keys sign
`repogo-rotate-v1`, the host ID, the old device ID and the new key, so it needs
no pairing again.

The relay caps each address at 32 connections and each host at 16 phones
waiting to be dialed back, pings links every 25 s, and drops a control line
over 64 KB. `REPOGO_HRELAY` sets the relay's address (`host:port`) or turns it
`off`, and `REPOGO_HRELAY_CA` names a PEM file of extra roots for a relay with
a private certificate. It also meters each host's phone traffic, all its phones
together: a 32 MB burst, then 4 MB/s; a log line past 2 GB in a UTC day and a
refusal past 10 GB until midnight; and it closes a stream idle for 2 minutes.

#### Direct on your network

From 0.1.44 (with RepoGo 1.0.4), the host also serves phones without the relay:

- **Tailscale, on by default.** When Tailscale is up, the host listens on this
  machine's tailnet address, port 47443, and follows the address as Tailscale
  comes, goes or changes it. `repogo start --tailscale=false` turns it off.
- **Local network, off by default.** `repogo start --lan` also listens on the
  machine's LAN address, on whatever network it joins; `--lan=false` turns it
  off. The setting is saved in `~/.repogo/runtime.json`.

`host.status` reports each address, and the phone tries them before the relay.
Each listener serves the same pinned TLS and session-bound hello as the relay
route, and caps how many connections may wait before their hello.

#### Plugin methods over HTTP

From 0.1.44, a plugin method may opt into plain HTTP, at two addresses, which
answer 404 to everything until one does:

- **This machine and its containers**, port 47444: `127.0.0.1`, and the gateway
  of each Apple container network while a container runs on it. Each listener
  takes calls only from its own network, and never listens on the LAN.
- **The public address**, `h-<host id>.repogo.dev`, through RepoGo's tunnel
  gateway, over the link the host keeps to it while a tunnel or such a method
  is open. The gateway ends TLS, so it sees these requests, as it does a tunnel's.

`repogo urls` lists each method's URLs. Each plugin has one key for its methods
that take one, kept in `state.db` and shown by `repogo keys <plugin>`. The plugins SDK's README covers who may call
each method and what it answers.

#### Push notifications

The relay holds the APNs key, so the host sends each push on its link to it.
Push notifications require a grant signed by the paired phone and an Apple App
Attest assertion covering the host, token, APNs environment, and grant time. The
relay checks the grant against the host its link proved. The app only grants
tokens received through its notification APIs. The relay pins Apple's App
Attest root and the app's identifier; a self-created identity and a leaked token
are insufficient. Grants are reusable for up to 7 days, with Apple's
certificates checked as of the grant's signed time, so their assertion counters
are not treated as one-shot login counters. The attestation binds the app key to
the phone identity.

The relay can't revoke a grant, so an unpaired host can push until its last
grant expires; phones re-sign on every connection. Each host may send 240
pushes a minute and each token may receive 120. The iOS app needs the App
Attest capability in its provisioning profile; the App Clip receives no pushes.
Push registration fails closed when App Attest is unavailable.

### RPC calls

The phone talks to the host in JSON-RPC 2.0 over that connection: list
chats, send a prompt, read a file, check git status, open a terminal. The host
pushes live updates (a reply streaming in, a file changing) the same way. Every
method's params and result are listed in [`docs/host-methods.md`](docs/host-methods.md),
and every event in [`docs/host-events.md`](docs/host-events.md).

A few methods, such as starting a pairing, only answer on this computer: they
need the token in `~/.repogo/runtime.json` (readable only by you) and are
refused from a phone.

### Agent runs

The host runs the agent CLIs you already have, signed in as you: Claude Code
through its stream-json mode and Codex through `codex app-server`. Each chat
runs in its project's folder. Turns in one chat run in order; different chats
run at the same time.

The host also reads the Claude Code and Codex sessions you started yourself,
in a terminal or your editor, so their history shows up on your phone.

### Plugins

RepoGo supports plugins: small apps in `~/.repogo/plugins`, written with
[`@repogo/plugins`](plugins-sdk/README.md), that run on this machine. They're
meant for custom developer tooling that helps your own workflow (a deploy
button, a status page, a webhook that starts an agent), not for production
applications.

The host provides the building blocks: collections, key-value storage, streams
and files, methods that cards, agents, crons and HTTP can call, and access to
the host's own methods and your agents. You build the plugins, alone or with an
agent, and they run with your account's access, so it's your responsibility to
make sure they're secure: check what an agent wrote, require a key on methods
served over HTTP, and choose the permission mode of any agent a plugin starts.

## What it changes on your computer

- `~/.repogo/` holds the binary, your pairings, the host's identity and TLS
  keys, a cache of your chat list, and logs. Delete it to remove everything.
- A launchd agent (macOS) or a systemd user service (Linux) starts the host.
  On Linux, `install` also turns on lingering (`loginctl enable-linger`) so the
  host starts at boot, not at your first login; where that needs root it
  prints the `sudo` command. On macOS the host starts at login, so a Mac that
  restarts unattended needs automatic login, which FileVault doesn't allow.
- Hooks in `~/.claude/settings.json` and `~/.codex/hooks.json` tell the host
  when you run an agent outside RepoGo. Your own hooks are left untouched, and
  `uninstall` removes only RepoGo's. Codex asks you to trust them once
  (`codex`, then `/hooks`).
- `keep-awake enable` adds `/etc/sudoers.d/repogo`, which lets the host run
  `pmset -a disablesleep` and nothing else.

## Uninstall

```sh
repogo uninstall
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
local WebSocket, and the host's pinned TLS, on temporary data.

### Layout

```
repogo/      the host binary: commands, config, start, update
hrelay/      the relay behind h.repogo.app: pipes phones' TLS, sends pushes
npm/         the `npx @repogo/host` installer
plugins-sdk/ `@repogo/plugins`, the package plugins are written with
tunnelwire/  wire types shared with the tunnel gateway
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
