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
- Claude Code or Codex on the same computer; RepoGo can install either and sign you in from your phone

### Commands

| Command | What it does |
| --- | --- |
| `npx @repogo/host` | Install and start the host, then show a pairing code (or, once a device is paired, the status) |
| `npx @repogo/host pair` | Show a pairing code for another device |
| `npx @repogo/host status` | Show whether the host is running |
| `npx @repogo/host devices` / `devices revoke <id>` | List the paired devices, or remove one: its connections close and it can no longer reach the host |
| `npx @repogo/host start` / `stop` / `restart` | Start, stop or restart the background host |
| `npx @repogo/host logs` | Show the host's logs |
| `npx @repogo/host update` | Update to the latest version; refuses while chats, terminals or actions are running (`--now` stops them) |
| `npx @repogo/host keep-awake enable` | Keep a Mac awake with its lid closed while a device is paired (one `sudo` prompt) |
| `npx @repogo/host relay` / `relay set <wss-url>` / `relay reset` | Show the relay, switch to your own, or go back to RepoGo's; switching unpairs your devices, so pair them again |
| `npx @repogo/host tls rotate` | Replace the host's TLS key, for one that may have leaked; phones learn the new one on their next connection |
| `npx @repogo/host tunnels` / `tunnels close <slug>` | List the public URLs open to your local servers, or close one |
| `npx @repogo/host uninstall` | Remove the background service; keeps your pairings and data in `~/.repogo` |

## How it works

The host has three parts.

### Connections

The host dials out and listens only on `127.0.0.1`, never on your network.
Phones reach it one of two ways, both end-to-end encrypted:

- **Direct** (TLS passthrough), the default with RepoGo's relay. Phones run
  TLS 1.3 straight to the host, and the relay only pipes the bytes.
- **Relay** (Noise), RepoGo's earlier channel. Hosts before 0.1.42, phones
  that haven't learned the host's TLS pin yet, and custom relays use it.

Pairing gives the phone the host's identity (an Ed25519 key) and, on 0.1.42
and later, its signed TLS pin. A phone's own key is Ed25519, or P-256 when it
lives in the iPhone's Secure Enclave.

#### Direct

```
phone ──TLS 1.3──▶ relay ──same bytes──▶ host
        <host-id>.h.repogo.app      dialed back over the host's link
        reads the name, holds no key
```

1. The host holds a link to `h.repogo.app:443`, the passthrough relay in
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
   ignores CAs. `repogo tls rotate` makes a new key under the next counter;
   phones refuse the new certificate once, fetch its signed pin over the relay
   channel, and come back direct.
4. TLS is 1.3 only on both ends, so the key exchange is X25519MLKEM768.
5. The phone's hello signs the host's challenge plus 32 bytes exported from
   this TLS session (`EXPORTER-repogo-hello`), so a signature can't be replayed
   on another connection. The host checks it against its paired devices.

A phone that isn't paired is let in only while a pairing code is live, and only
for one `pair.complete` of its own key. `devices.rotate` moves a paired phone to
a new key, such as one in the Secure Enclave: both keys sign
`repogo-rotate-v1`, the host ID, the old device ID and the new key, so it needs
no pairing again.

The relay caps each address at 32 connections and each host at 16 phones
waiting to be dialed back, pings links every 25 s, and drops a control line
over 64 KB. `REPOGO_HRELAY` sets the passthrough relay's address (`host:port`)
or turns it `off`; a host on a custom relay leaves it off unless set.

#### Relay channel

Each phone connects to RepoGo's relay (or yours) over a WebSocket, and the
relay routes each message by a small header and never holds a key. Each
connection runs a Noise XX handshake with an ML-KEM-1024 step
(`Noise_XXhfs_25519+MLKEM1024_AESGCM_SHA256`), so messages are sealed with
AES-GCM and the key exchange is post-quantum. The phone and the host each check
the other's paired identity before anything is sent.

A saved pairing starts with frame `0x04` instead of the pairing handshake's
`0x01`. If the device is no longer paired, the host returns `0x05` followed by
an Ed25519 signature over `repogo-not-paired-v1`, the 16-byte phone and host
IDs, and SHA-256 of that request frame. The fresh handshake binds the refusal
to one connection attempt, so a relay cannot forge or replay it. The phone
keeps the environment listed as No Longer Paired and stops reconnecting until
the user pairs again. A missing reply remains an ordinary connection failure.

The relay bounds handshakes to 8 KiB and allows 240 connection attempts per
address in each one-minute window, counting an IPv6 /64 as one address. One
address holds at most 16 sockets at once, and one IPv6 /48 at most 64. It
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

The phone talks to the host in JSON-RPC 2.0 over either connection: list
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
relay/       the relay channel's relay, which routes encrypted bytes
hrelay/      the passthrough relay behind h.repogo.app, which pipes TLS
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
