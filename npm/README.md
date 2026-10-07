# RepoGo host

Run coding agents on your computer from your phone.

The RepoGo host runs in the background on your computer. Pair it with the
RepoGo iPhone app and you can chat with Claude Code and Codex, follow their
work, and pick up your existing sessions, all from your phone.

## Get started

```sh
npx @repogo/host
```

This installs the host as a background service and shows a pairing code.
Scan it with your iPhone camera. If you don't have RepoGo installed, an App
Clip opens so you can pair and start chatting right away.

To pair another device later:

```sh
npx @repogo/host pair
```

## Requirements

- macOS or Linux, on Apple silicon/arm64 or x64
- Node.js 22 or later (only to install and run the `npx` command)
- Claude Code or Codex installed and signed in on the same computer

## Privacy

- Your chats and code stay on your computer.
- Your phone and your computer talk end-to-end encrypted. RepoGo's relay only
  forwards encrypted bytes and never stores chat or code content.
- RepoGo needs no account to get started.

## Commands

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

## Uninstall

```sh
npx @repogo/host uninstall
rm -rf ~/.repogo
```

`uninstall` keeps your pairings and data in `~/.repogo`. Delete that folder to
remove everything.

## Support

- Website: [repogo.app](https://repogo.app)
- Source: [github.com/repogoapp/host](https://github.com/repogoapp/host)
- Report a problem: [github.com/repogoapp/host/issues](https://github.com/repogoapp/host/issues)

## License

RepoGo is source-available under the [Functional Source License, Version 1.1,
ALv2 Future License](https://github.com/repogoapp/host/blob/main/LICENSE.md)
(FSL-1.1-ALv2). You can use, modify and share it for anything except a product
or service that competes with RepoGo. Each version becomes available under the
Apache License 2.0 two years after its release.
