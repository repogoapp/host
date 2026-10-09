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
| `npx @repogo/host pair --reusable 14d` | Show a code any number of devices can pair with until it expires (days `d` or hours `h`, up to 30 days); `--reusable off` ends it |
| `npx @repogo/host status` | Show whether the host is running |
| `npx @repogo/host devices` / `devices revoke <id>` | List the paired devices, or remove one: its connections close and it can no longer reach the host |
| `npx @repogo/host start` / `stop` | Start or stop the background host |
| `npx @repogo/host logs` | Show the host's logs |
| `npx @repogo/host update` | Update to the latest version; refuses while agents, terminals or builds are running (`--now` stops them) |
| `npx @repogo/host power enable` | Keep a Mac awake with its lid closed while a device is paired (one `sudo` prompt) |
| `npx @repogo/host uninstall` | Remove the background service; keeps your pairings and data in `~/.repogo` |

### Run it in an Apple container

For an environment nobody sits at, such as a demo machine others pair with,
run the host in a Linux container with Apple's
[`container`](https://github.com/apple/container) tool on a Mac. Nothing on
the Mac itself is touched.

1. Create the container. The host runs as its main process (a container has
   no launchd or systemd), and `npx` downloads the binary to
   `/root/.repogo/bin` first:

   ```sh
   container system start
   container run -d --name repogo-host -c 4 -m 4G \
     docker.io/library/node:22-bookworm \
     sh -c 'npx -y @repogo/host version && exec /root/.repogo/bin/repogo serve'
   container logs repogo-host    # wait for "hostlink: attached to relay"
   ```

2. Make a reusable pairing code. A normal code admits one device and lasts
   two minutes; this one admits any number until it expires:

   ```sh
   container exec repogo-host /root/.repogo/bin/repogo pair --reusable 21d
   ```

   It prints a QR, the 14-character code and its `https://repogo.app/<code>`
   link. Open the link on an iPhone, scan the QR, or tap **Enter code** on
   the pairing screen.

3. When you're done, end it. Devices that already paired stay paired until
   you remove them from the app:

   ```sh
   container exec repogo-host /root/.repogo/bin/repogo pair --reusable off
   ```

The pairing, the reusable code, chats and agent sign-ins live in the
container's own filesystem: `container stop` and `container start` keep them,
`container rm` loses them. To update, run
`container exec repogo-host /root/.repogo/bin/repogo update`; the host
replaces itself in place, so the container keeps running.

Anyone with a reusable code can pair and run agents there until it expires,
so use a container that holds nothing personal, and give its agents accounts
with spending limits.

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

The relay bounds handshakes to 8 KiB and allows 240 connection attempts per
address in each one-minute window, counting an IPv6 /64 as one address. It
tracks at most 4,096 addresses per window; past that, new addresses keep only
the concurrent-connection cap until the next window. Offline presence is best
effort: it remembers at most 4,096 departed devices with 128 peers each, and
never retains a device with no peers. When that cache is full, the oldest
departure is forgotten; connected devices still exchange traffic.

Push notifications require a grant signed by the paired phone and an Apple App
Attest assertion covering the host, token, APNs environment, and grant time. The
app only grants tokens received through its notification APIs. The relay pins
Apple's App Attest root and the app's identifier; a self-created identity and a
leaked token are insufficient. Grants are reusable for up to 7 days, with Apple's
certificates checked as of the grant's signed time, so their assertion counters
are not treated as one-shot login counters. The attestation binds the app key to the phone identity.

The relay can't revoke a grant, so an unpaired host can push until its last
grant expires; phones re-sign on every connection. Behind a proxy, set
`RELAY_CLIENT_IP_HEADER` to the header it puts the caller's address in, and
only when it sets that header on every request; otherwise leave it unset.

Self-hosted relays that send push notifications need `APPLE_APP_ID_PREFIX`
(defaults to `APNS_TEAM_ID`) and `APPLE_BUNDLE_ID` (defaults to `app.repogo`).
The iOS app needs the App Attest capability in its provisioning profile; the
App Clip receives no pushes. This grant format requires matching iOS, host, and
relay releases; signature-only grants are refused, and phones must reconnect to
register fresh grants. Push registration fails closed when App Attest is unavailable.

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
- A launchd agent (macOS) or a systemd user service (Linux) starts the host.
  On Linux, `install` also turns on lingering (`loginctl enable-linger`) so the
  host starts at boot, not at your first login; where that needs root it
  prints the `sudo` command. On macOS the host starts at login, so a Mac that
  restarts unattended needs automatic login, which FileVault doesn't allow.
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
