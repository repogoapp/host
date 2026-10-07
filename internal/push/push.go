// Package push turns hook notices into APNs alerts for the paired phones. The
// relay holds the APNs key, so every send is a `push.send` to it; the host
// contributes only device tokens and a generic body.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/relay"
)

// The low-battery alert goes out once as the Mac's battery alone drops below
// lowBatteryAt, and not again until it has climbed past lowBatteryRearm.
const (
	lowBatteryAt    = 10
	lowBatteryRearm = 15
)

// Sender is the relay-addressed call hostlink exposes.
type Sender interface {
	Control(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// Send asks the relay to deliver req. dead reports Apple saying the token is
// gone, for the caller to forget; any other failure is only logged.
func Send(ctx context.Context, s Sender, log *slog.Logger, req relay.PushRequest) (dead bool) {
	_, err := s.Control(ctx, "push.send", req)
	var rpcErr *jsonrpc.Error
	switch {
	case err == nil:
	case errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeNotFound:
		return true
	default:
		log.Debug("push: not delivered", "push_type", req.PushType, "err", err)
	}
	return false
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
		return alert{"Approval needed", "Your computer is waiting for you."}, true
	case agent.ChatAwaitingUser:
		return alert{"Question for you", "Your computer is waiting for you."}, true
	case agent.ChatCompleted:
		return alert{"Task finished", "Your computer finished a task."}, true
	case agent.ChatFailed:
		return alert{"Task failed", "A task on your computer stopped with an error."}, true
	}
	return alert{}, false
}

// Battery takes each battery reading in turn and alerts the phones when the
// Mac is about to die unplugged, so a task on it is not lost to a dark screen.
func (n *Notifier) Battery(ctx context.Context, b power.Battery) {
	switch {
	case b.Percent > lowBatteryRearm || b.PluggedIn:
		n.warned = false
	case !n.warned && b.Present && b.Percent < lowBatteryAt:
		n.warned = true
		payload, _ := json.Marshal(map[string]any{
			"aps": map[string]any{
				"alert": map[string]string{"title": "Low battery",
					"body": fmt.Sprintf("Your Mac has %d%% battery left. Plug it in to keep your work running.", b.Percent)},
				"sound": "default",
			},
			"host_id": n.store.Identity().ID,
		})
		n.push(ctx, "battery", payload)
	}
}

// EnvRequest wakes the phones when a start here is waiting for secrets and no
// device is connected to be asked. Opening the app fetches the request with
// env.pending; the handle names are all the body says about it.
func (n *Notifier) EnvRequest(ctx context.Context, requestID, hostLabel string, handles []string) {
	payload, _ := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{"title": "Share secrets?",
				"body": hostLabel + " wants " + strings.Join(handles, ", ")},
			"sound": "default",
		},
		"host_id":    n.store.Identity().ID,
		"kind":       "env_request",
		"request_id": requestID,
	})
	n.push(ctx, "env:"+requestID, payload)
}

// BrowserRequest wakes the one phone an agent wants to drive the preview on,
// when it is not connected to be asked. Opening the app fetches the request
// with browser.pending.
func (n *Notifier) BrowserRequest(ctx context.Context, to device.ID, requestID string) {
	payload, _ := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{"title": "Browser needed",
				"body": "An agent wants to use the browser in RepoGo. Open the app to let it."},
			"sound": "default",
		},
		"host_id":    n.store.Identity().ID,
		"kind":       "browser_request",
		"request_id": requestID,
	})
	for _, peer := range n.store.Peers() {
		if peer.ID == to {
			n.pushPeer(ctx, peer, "browser:"+requestID, payload)
		}
	}
}

// CloudStopping wakes the phones shortly before a cloud host's session ends,
// so a running chat can be extended rather than cut off.
func (n *Notifier) CloudStopping(ctx context.Context, hostLabel string, in time.Duration) {
	payload, _ := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{"title": hostLabel + " stops soon",
				"body": fmt.Sprintf("It stops in %d minutes. Extend it in RepoGo to keep working.", int(in.Round(time.Minute).Minutes()))},
			"sound": "default",
		},
		"host_id": n.store.Identity().ID,
		"kind":    "cloud_stopping",
	})
	n.push(ctx, "cloud-stopping", payload)
}

func (n *Notifier) deliver(ctx context.Context, notice notify.Notice, a alert) {
	chatID := notice.ChatID()
	payload, _ := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert":     map[string]string{"title": a.Title, "body": a.Body},
			"sound":     "default",
			"thread-id": chatID,
		},
		"host_id": n.store.Identity().ID,
		"chat_id": chatID,
	})
	n.push(ctx, chatID, payload)
}

// push sends one APNs payload to every paired phone with a token.
func (n *Notifier) push(ctx context.Context, collapseID string, payload []byte) {
	for _, peer := range n.store.Peers() {
		n.pushPeer(ctx, peer, collapseID, payload)
	}
}

func (n *Notifier) pushPeer(ctx context.Context, peer device.Peer, collapseID string, payload []byte) {
	if peer.Push == nil {
		return
	}
	dead := Send(ctx, n.send, n.log, relay.PushRequest{
		Token: peer.Push.Token, Environment: peer.Push.Environment,
		CollapseID: collapseID, Payload: payload,
	})
	// The app registers a fresh token on its next launch.
	if dead {
		if err := n.store.ForgetPush(peer.ID, ""); err != nil {
			n.log.Warn("push: could not forget token", "device", peer.ID, "err", err)
		}
	}
}
