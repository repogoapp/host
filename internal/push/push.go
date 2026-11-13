// Package push turns hook notices into alerts for the paired phones: APNs for
// iOS, FCM data messages for Android. The relay holds both keys, so every send
// is a push request on the host's link to it; the host contributes only
// device tokens and a generic body.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/notify"
	"github.com/repogoapp/host/internal/power"
	"github.com/repogoapp/host/internal/pushgate"
	"github.com/repogoapp/host/internal/reminder"
)

// The low-battery alert goes out once as the Mac's battery alone drops below
// lowBatteryAt, and not again until it has climbed past lowBatteryRearm.
const (
	lowBatteryAt    = 10
	lowBatteryRearm = 15
)

// Sender delivers one push through the relay: the passthrough link.
type Sender interface {
	Push(ctx context.Context, req pushgate.PushRequest) error
}

// Send asks the relay to deliver req. dead reports Apple saying the token is
// gone, for the caller to forget; any other failure is only logged.
func Send(ctx context.Context, s Sender, log *slog.Logger, req pushgate.PushRequest) (dead bool) {
	dead, err := Try(ctx, s, req)
	switch {
	case dead:
		log.Warn("push: token is dead", "push_type", req.PushType, "environment", req.Environment, "err", err)
	case err != nil:
		log.Debug("push: not delivered", "push_type", req.PushType, "err", err)
	}
	return dead
}

// Try is Send for a caller that retries: dead is a token Apple will never
// take, with err saying why; otherwise err is a failure worth trying again.
func Try(ctx context.Context, s Sender, req pushgate.PushRequest) (dead bool, err error) {
	err = s.Push(ctx, req)
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeNotFound {
		return true, err
	}
	return false, err
}

type Notifier struct {
	store *device.Store
	send  Sender
	log   *slog.Logger

	// The low-battery alert has gone out; Battery's one caller owns it.
	warned bool
}

func New(store *device.Store, send Sender, log *slog.Logger) *Notifier {
	return &Notifier{store: store, send: send, log: log}
}

// Run pushes every notice a phone should wake for until ctx ends.
func (n *Notifier) Run(ctx context.Context, bridge *notify.Bridge) {
	bridge.Follow(ctx, func(notice notify.Notice) {
		if alert, ok := alertFor(notice.Status); ok {
			n.deliver(ctx, notice, alert)
		}
	})
}

type alert struct{ Title, Body string }

// alertFor is the whole vocabulary, on purpose: the relay and Apple read
// these, so nothing about the project or the message may appear.
func alertFor(s agent.ChatStatus) (alert, bool) {
	switch s {
	case agent.ChatAwaitingApproval:
		return alert{"Approval needed", "Your environment is waiting for you."}, true
	case agent.ChatAwaitingUser:
		return alert{"Question for you", "Your environment is waiting for you."}, true
		// Off while the app shows a turn's end as a brand alert from chats.attention.
		// case agent.ChatCompleted: return alert{"Task finished", "Your environment finished a task."}, true
		// case agent.ChatFailed: return alert{"Task failed", "A task in your environment stopped with an error."}, true
	}
	return alert{}, false
}

// Battery takes each battery reading in turn and alerts the phones when the
// host is about to die unplugged, so a task on it is not lost to a dark screen.
func (n *Notifier) Battery(ctx context.Context, b power.Battery) {
	switch {
	case b.Percent > lowBatteryRearm || b.PluggedIn:
		n.warned = false
	case !n.warned && b.Present && b.Percent < lowBatteryAt:
		n.warned = true
		n.push(ctx, note{
			title:    "Low battery",
			body:     fmt.Sprintf("Your environment has %d%% battery left. Plug it in to keep your work running.", b.Percent),
			collapse: "battery",
			fields:   map[string]string{"host_id": n.hostID()},
		})
	}
}

// BrowserRequest wakes the one phone an agent wants to drive the browser on,
// when it is not connected to be asked. Opening the app fetches the request
// with browser.pending.
func (n *Notifier) BrowserRequest(ctx context.Context, to device.ID, requestID string) {
	m := note{
		title:    "Browser needed",
		body:     "An agent wants to use the browser in RepoGo. Open the app to let it.",
		collapse: "browser:" + requestID,
		fields:   map[string]string{"host_id": n.hostID(), "kind": "browser_request", "request_id": requestID},
	}
	for _, peer := range n.store.Peers() {
		if peer.ID == to {
			n.pushPeer(ctx, peer, m)
		}
	}
}

func (n *Notifier) deliver(ctx context.Context, notice notify.Notice, a alert) {
	chatID := notice.ChatID()
	n.push(ctx, note{
		title: a.Title, body: a.Body, thread: chatID, collapse: chatID,
		fields: map[string]string{"host_id": n.hostID(), "chat_id": chatID},
	})
}

// PluginNotice is a plugin's alert: `plugins.Notice` as this package takes it.
type PluginNotice struct {
	Plugin, Root, Title, Body, Page, Collapse, URL string
}

// Plugin pushes a plugin's alert to every paired phone. A tap opens the
// notice's link if it has one, else the project's page it names.
func (n *Notifier) Plugin(ctx context.Context, notice PluginNotice) {
	collapse := notice.Collapse
	if collapse == "" {
		collapse = notice.Plugin + ":" + notice.Title
	}
	n.push(ctx, note{
		title: notice.Title, body: notice.Body, thread: "plugin:" + notice.Plugin, collapse: collapse,
		fields: map[string]string{"host_id": n.hostID(), "project": notice.Root, "page": notice.Page, "url": notice.URL},
	})
}

// Reminder pushes a reminder to every paired phone. A tap opens its chat,
// as a turn's alert does; one fired late says by how much.
func (n *Notifier) Reminder(ctx context.Context, r reminder.Alert) {
	body := r.Text
	if r.Late >= time.Minute {
		body += fmt.Sprintf(" (%d min late)", int(r.Late.Minutes()))
	}
	thread := r.ChatID
	if thread == "" {
		thread = "reminders"
	}
	n.push(ctx, note{
		title: "Reminder", body: body, thread: thread, collapse: "reminder:" + r.ID,
		fields: map[string]string{"host_id": n.hostID(), "chat_id": r.ChatID},
	})
}

func (n *Notifier) hostID() string { return string(n.store.Identity().ID) }

// note is one alert as both services show it: its text, the group it joins
// (iOS's thread-id), the id a later one replaces it by, and the fields a tap
// reads (which chat, project or page it opens).
type note struct {
	title, body string
	thread      string
	collapse    string
	fields      map[string]string
}

// apns is the alert as an APNs body: `aps`, with the fields beside it.
func (m note) apns() []byte {
	aps := map[string]any{"alert": map[string]string{"title": m.title, "body": m.body}, "sound": "default"}
	if m.thread != "" {
		aps["thread-id"] = m.thread
	}
	body := map[string]any{"aps": aps}
	for k, v := range m.fields {
		body[k] = v
	}
	payload, _ := json.Marshal(body)
	return payload
}

// fcm is the alert as an FCM data message, which the Android app draws:
// kind alert, so it tells alerts from Live Updates, and the alert's own kind
// (a browser request) as alert_kind.
func (m note) fcm() []byte {
	data := map[string]any{"kind": "alert", "title": m.title, "body": m.body, "thread": m.thread, "collapse": m.collapse}
	for k, v := range m.fields {
		if k == "kind" {
			k = "alert_kind"
		}
		data[k] = v
	}
	return Data(data)
}

// push sends one alert to every paired phone with a token.
func (n *Notifier) push(ctx context.Context, m note) {
	for _, peer := range n.store.Peers() {
		n.pushPeer(ctx, peer, m)
	}
}

// pushPeer sends m to one phone in its service's shape: APNs collapses by id,
// and Android replaces a notification by the collapse id the data carries.
func (n *Notifier) pushPeer(ctx context.Context, peer device.Peer, m note) {
	if peer.Push == nil {
		return
	}
	req := Request(peer, *peer.Push)
	if IsFCM(peer.Push) {
		req.Payload, req.Priority = m.fcm(), 10
	} else {
		req.CollapseID, req.Payload = m.collapse, m.apns()
	}
	dead := Send(ctx, n.send, n.log, req)
	// The app registers a fresh token on its next launch.
	if dead {
		if err := n.store.ForgetPush(peer.ID, "", peer.Push.Token); err != nil {
			n.log.Warn("push: could not forget token", "device", peer.ID, "err", err)
		}
	}
}

// Request addresses one of peer's tokens, carrying the grant the phone signed
// for this host; the relay refuses a push without it.
func Request(peer device.Peer, target device.PushTarget) pushgate.PushRequest {
	return pushgate.PushRequest{
		Token: target.Token, Service: target.Service, Environment: target.Environment,
		Grant:     pushgate.PushGrant{Device: peer.ID, PublicKey: peer.Public, Signature: target.Grant, GrantedAt: target.GrantedAt},
		Integrity: target.Integrity,
	}
}

// IsFCM is a target Android's FCM delivers to, which takes data messages
// (Data) where APNs takes an `aps` body.
func IsFCM(target *device.PushTarget) bool {
	return target != nil && target.Service == device.PushFCM
}

// Data is an FCM data message's payload: a flat map of strings, with any
// other value as its JSON. The relay hands it to FCM as the message's data.
func Data(fields map[string]any) []byte {
	data := make(map[string]string, len(fields))
	for k, v := range fields {
		switch v := v.(type) {
		case string:
			data[k] = v
		default:
			b, _ := json.Marshal(v)
			data[k] = string(b)
		}
	}
	body, _ := json.Marshal(data)
	return body
}
