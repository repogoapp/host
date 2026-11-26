// Package push sends alerts to the paired phones: a chat waiting on the user
// or ending, a plugin's notice, a reminder, low battery. APNs for iOS, FCM
// data messages for Android. The relay holds both keys, so every send
// is a push request on the host's link to it. Each alert is sealed for its
// phone (pushseal); a phone that can't open sealed alerts gets none.
package push

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/power"
	"github.com/repogoapp/host/internal/pushgate"
	"github.com/repogoapp/host/internal/pushseal"
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
	// subtitle sits between the title and body (in the header on Android).
	subtitle string
	thread   string
	collapse string
	fields   map[string]string
	// wake has iOS also wake the app in the background (content-available),
	// so it can fetch what the alert is about before the user opens it.
	wake bool
}

// sealedAlert is what an iOS alert's box holds: everything the phone shows
// or a tap reads, which the notification service extension puts back.
type sealedAlert struct {
	Title    string            `json:"title"`
	Subtitle string            `json:"subtitle,omitempty"`
	Body     string            `json:"body"`
	Thread   string            `json:"thread,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
	// At is when the host sent it (Unix ms), inside the box so the relay can't
	// change it: a phone refuses an alert too old, or one it already opened.
	At int64 `json:"at"`
}

// sealedAPNs is the alert for a phone whose notification service extension
// opens boxes: fixed text that shows if it can't, mutable-content so iOS runs
// it, and the box beside `aps` with the host that sealed it.
func (m note) sealedAPNs(key []byte, host string, at int64) ([]byte, error) {
	env, err := pushseal.Seal(key, host, pushseal.Alert, "", sealedAlert{m.title, m.subtitle, m.body, m.thread, m.fields, at})
	if err != nil {
		return nil, err
	}
	aps := map[string]any{
		"alert": map[string]string{"title": "RepoGo", "body": "New notification"},
		"sound": "default", "mutable-content": 1,
	}
	if m.wake {
		aps["content-available"] = 1
	}
	return json.Marshal(map[string]any{
		"aps":     aps,
		"host_id": host,
		"sealed":  env,
	})
}

// fcm is the alert as an FCM data message, which the Android app draws:
// kind alert, so it tells alerts from Live Updates, the alert's own kind (a
// browser request) as alert_kind, and when it was sent, as sealedAlert.At.
func (m note) fcm(at int64) []byte {
	data := map[string]any{"kind": "alert", "title": m.title, "body": m.body, "thread": m.thread, "collapse": m.collapse,
		"timestamp": strconv.FormatInt(at, 10)}
	if m.subtitle != "" {
		data["subtitle"] = m.subtitle
	}
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

// pushSender sends a chat's alert to the phone that sent its turn, the one
// the user has with them; every other open app shows its in-app alert. It
// goes to every phone when no phone sent the turn, or the sender was removed
// or can't take sealed alerts. A send that fails is not resent to the rest.
func (n *Notifier) pushSender(ctx context.Context, sender device.ID, m note) {
	if sender != "" {
		if peer, err := n.store.Peer(sender); err == nil && peer.Push != nil && AlertKey(peer) != nil {
			n.pushPeer(ctx, peer, m)
			return
		}
	}
	n.push(ctx, m)
}

// NeedsYou is a chat blocked on the user, as the app's own alert shows it:
// the chat, who asks where, and what it asks.
type NeedsYou struct {
	ChatID, Agent, Project string
	// Sender is the device that sent the turn, empty for none.
	Sender device.ID
	// Prompt is the chat's last user message; Title the chat's.
	Prompt, Title string
	// Question is a form to answer rather than a call to approve.
	Question bool
	// Ask is what it wants: the question, or the call and its target.
	Ask string
}

// NeedsYouAlert alerts the phones that a chat waits on the user. It replaces
// the chat's earlier alert, and a tap opens the chat to answer.
func (n *Notifier) NeedsYouAlert(ctx context.Context, w NeedsYou) {
	what := "needs approval"
	fallback := "Approval needed"
	if w.Question {
		what, fallback = "has a question", "Question for you"
	}
	n.pushSender(ctx, w.Sender, note{
		title:    clip(cmp.Or(oneLine(w.Title), oneLine(w.Prompt), fallback), 80),
		subtitle: who(w.Agent, what, w.Project),
		body:     clip(cmp.Or(oneLine(w.Ask), fallback), 240),
		thread:   w.ChatID, collapse: w.ChatID,
		fields: map[string]string{"host_id": n.hostID(), "chat_id": w.ChatID, "kind": "needs_you"},
	})
}

// AskOf is what an approval asks, for its alert: a question's text, or the
// call and what it acts on ("Bash: npm test").
func AskOf(a *agent.Approval) (ask string, question bool) {
	if a == nil {
		return "", false
	}
	if a.Kind == agent.ApprovalQuestion || len(a.Questions) > 0 {
		if len(a.Questions) == 0 {
			return a.Title, true
		}
		ask = cmp.Or(a.Questions[0].Text, a.Questions[0].Header, a.Title)
		if more := len(a.Questions) - 1; more > 0 {
			ask += fmt.Sprintf(" (+%d more)", more)
		}
		return ask, true
	}
	var input map[string]any
	_ = json.Unmarshal(a.Input, &input)
	for _, key := range []string{"command", "file_path", "path", "url", "pattern", "description"} {
		if v, ok := input[key].(string); ok && strings.TrimSpace(v) != "" {
			return a.Title + ": " + v, false
		}
	}
	return a.Title, false
}

// who is "Claude needs approval in web": the agent, what happened, where.
func who(agentKind, what, project string) string {
	name := agentKind
	if name != "" {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	s := strings.TrimSpace(name + " " + what)
	if project != "" {
		s += " in " + project
	}
	return s
}

// TurnEnd is a turn that finished, failed or was stopped, as the app's own
// alert for it shows: the user's message, who ended where, and the recap.
type TurnEnd struct {
	ChatID, Agent, Project string
	// Sender is the device that sent the turn, empty for none.
	Sender device.ID
	// Kind is finished, failed or stopped (chatlive's attention kinds).
	Kind string
	// Prompt is the chat's last user message; Title the chat's, for a turn
	// with none (a scheduled one).
	Prompt, Title  string
	Summary, Error string
}

// TurnEnded alerts the phone that sent the turn (pushSender), sealed as every
// alert is. An app that is open shows its in-app alert instead (`kind` turn_end).
func (n *Notifier) TurnEnded(ctx context.Context, t TurnEnd) {
	verb := cmp.Or(t.Kind, "finished") // finished, failed or stopped
	ended := who(t.Agent, verb, t.Project)
	m := note{
		title:    clip(cmp.Or(oneLine(t.Prompt), oneLine(t.Title), "Task "+verb), 80),
		subtitle: ended,
		body:     clip(cmp.Or(oneLine(t.Summary), oneLine(t.Error), ended), 240),
		thread:   t.ChatID, collapse: "end:" + t.ChatID,
		fields: map[string]string{"host_id": n.hostID(), "chat_id": t.ChatID, "kind": "turn_end"},
		// The app fetches the chat's new rows, so a tap opens on the reply.
		wake: true,
	}
	n.pushSender(ctx, t.Sender, m)
}

// Line is s on one line, cut to at most n characters: a title or body as an
// alert or a Live Activity shows it.
func Line(s string, n int) string { return clip(oneLine(s), n) }

// oneLine folds whitespace, so a multi-line message reads as one.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip cuts s to at most n characters, with an ellipsis.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// pushPeer sends m to one phone in its service's shape: APNs collapses by id,
// and Android replaces a notification by the collapse id the data carries.
func (n *Notifier) pushPeer(ctx context.Context, peer device.Peer, m note) {
	key := AlertKey(peer)
	if peer.Push == nil || key == nil {
		return
	}
	req := Request(peer, *peer.Push)
	host := n.hostID()
	at := time.Now().UnixMilli()
	var payload []byte
	var err error
	if IsFCM(peer.Push) {
		payload, err = SealData(key, host, m.fcm(at))
		req.Priority = 10
	} else {
		payload, err = m.sealedAPNs(key, host, at)
		req.CollapseID = pushseal.CollapseID(key, m.collapse)
	}
	if err != nil {
		n.log.Error("push: could not seal", "device", peer.ID, "err", err)
		return
	}
	req.Payload = payload
	dead := Send(ctx, n.send, n.log, req)
	// The app registers a fresh token on its next launch.
	if dead {
		if err := n.store.ForgetPush(peer.ID, "", peer.Push.Token); err != nil {
			n.log.Warn("push: could not forget token", "device", peer.ID, "err", err)
		}
	}
}

// Backoff is how long a push waits after failures rounds in a row failed:
// first, doubling up to most, so a phone the relay keeps refusing can't spend
// the host's push budget.
func Backoff(failures int, first, most time.Duration) time.Duration {
	gap := first
	for range failures - 1 {
		if gap >= most {
			return most
		}
		gap *= 2
	}
	return min(gap, most)
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
