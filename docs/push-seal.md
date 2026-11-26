# Sealed pushes

Every push the host sends is sealed for the phone it goes to: alerts, Live Activities (iOS) and Live Updates (Android). RepoGo's relay, Apple and Google pass ciphertext, and a phone that sent no key gets nothing. `internal/pushseal` seals; the iOS and Android apps open. `internal/pushseal/testdata/vectors.json` pins exact outputs for their tests.

## The key

- The phone makes 32 random bytes per host and sends them with `devices.push_key`, a paired method: it travels only over the phone's end-to-end TLS channel to the host, never readable by the relay.
- The host keeps it in its 0600 identity file (`push_key` on the peer). Revoking the phone deletes it.
- `enc = HKDF-SHA256(secret, salt empty, info "repogo-push-seal-v1 enc")`, 32 bytes.
- `kid` is the first 4 bytes of `SHA-256("repogo-push-seal-v1 kid" ‖ secret)` in hex. `devices.hello` answers it as `push_key_id`, so a phone sends its key only when they differ.

## Negotiation

- The phone sends feature `push_seal` in `devices.hello`: 1 opens Live Activity and Android boxes, 2 also has an iPhone's notification service extension open alerts. The host answers the level it seals for, now 2; an older host answers nothing, and the phone sends no key.
- **Nothing goes out in the clear.** A phone gets Live Activity or Live Update pushes only when the host holds its key and its `push_seal` is at least 1, and alerts only at 2 on iOS (its notification service extension) or 1 on Android. Any other phone, an app from before sealing included, gets no pushes from this host.
- A hello without the feature drops the key.

## The box

`{"v": 1, "kid": "<8 hex>", "box": "<base64url, no padding>"}`

- `box` = `nonce(12) ‖ ciphertext ‖ tag(16)`, AES-256-GCM under `enc`, a random nonce each time.
- Plaintext: `0x01 ‖ length(2, big-endian) ‖ JSON ‖ zeros to a multiple of 32 bytes`.
- Associated data: `"repogo-push-seal-v1"`, then host id, purpose and bind, each prefixed with its 2-byte big-endian length.

| Purpose | Bind | JSON |
| --- | --- | --- |
| `agents.state` | empty | `working`, `needs_you`, `finished`, `chats` (up to 4 `{title, status}`), `at` (Unix ms) |
| `steps.attrs` | empty | `key`, `project` |
| `steps.state` | the activity's key | the step state, plus `at` |
| `fcm` | empty | the data map the Android app reads: a Live Update's or an alert's fields |
| `alert` | empty | an iOS alert's `title`, `subtitle`, `body`, `thread`, `fields` |

## On the wire

- **iOS:** `content-state` is `{"sealed": <box>}`. An agents start keeps `attributes: {"host_id"}`; a steps start sends `attributes: {"host_id", "sealed": <box>}`, and its alert is `RepoGo` / `An activity started`.
- **Android:** the data map is `{"host_id", "v": "1", "kid", "box"}`, for Live Updates and alerts alike: an Android app at `push_seal` 1 opens both.
- **iOS alerts,** for a phone at `push_seal` 2 (its notification service extension opens them): `aps` is `{"alert": {"title": "RepoGo", "body": "New notification"}, "sound": "default", "mutable-content": 1}`, beside it `host_id` and `sealed`, a box of purpose `alert` holding `title`, `subtitle`, `body`, `thread` and the tap `fields`. A turn's end also carries `"content-available": 1`, so iOS wakes the app to fetch the chat's new rows; the app keeps its own record of the boxes it fetched for, apart from the extension's. The collapse id is `hex(HMAC-SHA256(HKDF(secret, info "repogo-push-seal-v1 collapse"), id))`, first 16 bytes. An iPhone at 1 gets no alerts.

## What the phones refuse

- A box that doesn't open is never shown: cards draw "Open RepoGo to see activity", Android drops the message.
- Once a host holds the phone's key, plaintext Live Activity states and Android messages from it are refused, since the relay could have written them. Android also drops plaintext naming no host, or a host it isn't paired with.
- An alert carries when it was sent (`at`; Android's map, `timestamp`) inside the box. An alert with no time, one sent more than a day ago, or a box already opened, is a replay: an iPhone shows it as the fixed text, Android drops it. Both remember opened boxes, by their nonce bytes, for a day plus five minutes of clock skew, across restarts. Live Activity states are ordered by their own `at`, Android's Live Updates by generation and revision, so a late end still clears its card.
- What a tap acts on (chat, page, link) comes only from the box: an iPhone drops the fields the relay sent beside it.
- An iPhone can't refuse plaintext alerts: iOS runs the notification service extension only for `mutable-content`, which a relay can leave off. This ends when the apps stop reading plaintext altogether (the plan's step 8).

## Design limits

- **No forward secrecy.** One secret seals every push for a phone and host until it changes. Whoever takes it, from the phone or the host, can open pushes they captured earlier. Re-pairing or a new key limits what comes after, not what came before.
- **A new TLS key means pairing again.** Rotating the host's TLS certificate doesn't teach paired phones the new pin.

## What stays visible

The start alert's fixed text; that an iOS alert may wake the app (`content-available`); `event`, `timestamp`, `stale-date`, `dismissal-date`, `attributes-type`, `input-push-token`; the host id, token, grant, topic and environment; when and how often pushes go, and on Android their priority; the size to the nearest 32 bytes.
