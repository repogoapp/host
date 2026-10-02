package relay

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/jsonrpc"
)

// ControlID is the envelope target that addresses the relay itself: all
// zero, which no device can be the fingerprint of. The only frames the relay
// reads are the ones sent here; routed frames stay opaque.
var ControlID = device.IDFromBytes(make([]byte, device.IDLen))

// Presence notices, from ControlID to every device a departing device was
// talking to: gone when its last socket closes, back when it attaches again.
const (
	PresenceGone = "peer.gone"
	PresenceBack = "peer.back"
)

// Presence is a presence notice's params. Reason, on PresenceGone only, says
// whether its socket closed or it stopped answering pings.
type Presence struct {
	Device device.ID `json:"device"`
	Reason string    `json:"reason,omitempty"`
}

const (
	ReasonClosed       = "closed"
	ReasonUnresponsive = "unresponsive"
)

// PushRequest is `push.send`: one APNs delivery on behalf of a host. The
// relay holds the APNs key so the host never has to.
type PushRequest struct {
	Token       string          `json:"token"`
	Environment string          `json:"environment"`
	CollapseID  string          `json:"collapse_id,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	// PushType is "alert" (the default) or "liveactivity", which starts or
	// updates a Live Activity and goes to the bundle's liveactivity topic.
	PushType string `json:"push_type,omitempty"`
	// Priority is APNs' 10 (at once, the default) or 5 (when power allows;
	// Live Activity updates that are not worth waking the phone for).
	Priority int `json:"priority,omitempty"`
}

// PushTypeLiveActivity is PushRequest.PushType for a Live Activity.
const PushTypeLiveActivity = "liveactivity"

// The APNs environments a phone's token can come from.
const (
	EnvironmentSandbox    = "sandbox"
	EnvironmentProduction = "production"
)

// ErrUnregistered is what Push returns when APNs says the token is dead, so
// the host can forget it.
var ErrUnregistered = errors.New("apns: unregistered")

// pushBurst bounds how many pushes one connection may request per minute: a
// host's Live Activities update every few seconds per running chat and phone.
// Tokens are unguessable, so the exposure is our APNs quota, not a user.
const pushBurst = 240

const (
	// maxControls bounds one connection's control requests in flight.
	maxControls = 16
	// controlTimeout bounds one control request, APNs included.
	controlTimeout = 10 * time.Second
)

// control answers a request addressed to the relay on its own goroutine, so a
// slow APNs never holds up the frames behind it.
func (s *Server) control(ctx context.Context, from *conn, payload []byte) {
	msg, err := jsonrpc.Decode(payload)
	if err != nil || !msg.IsRequest() {
		return
	}
	select {
	case from.controls <- struct{}{}:
	default:
		from.reply(jsonrpc.Fail(msg.ID, jsonrpc.CodeUnavailable, "too many control requests in flight"))
		return
	}
	go func() {
		defer func() { <-from.controls }()
		ctx, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		from.reply(s.answer(ctx, from, msg))
	}()
}

// answer is one control request's reply. The caller is the id the handshake
// verified, which is all the authorization push.send needs.
func (s *Server) answer(ctx context.Context, from *conn, msg *jsonrpc.Message) *jsonrpc.Message {
	switch {
	case msg.Method != "push.send":
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeMethodNotFound, "unknown control method")
	case s.cfg.Push == nil:
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeUnavailable, "this relay has no APNs key")
	case !from.allowPush():
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeDenied, "push rate limit")
	}
	var req PushRequest
	if err := jsonrpc.Into(msg.Params, &req); err != nil || req.Token == "" || len(req.Payload) == 0 {
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeInvalidParams, "token and payload are required")
	}
	if req.Environment != EnvironmentSandbox && req.Environment != EnvironmentProduction {
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeInvalidParams, "environment must be sandbox or production")
	}
	switch err := s.cfg.Push(ctx, req); {
	case errors.Is(err, ErrUnregistered):
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeNotFound, err.Error())
	case err != nil:
		s.log.Warn("relay: push failed", "from", from.id, "err", err)
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeInternal, err.Error())
	}
	reply, _ := jsonrpc.Result(msg.ID, map[string]bool{"ok": true})
	return reply
}

// reply queues a control reply for c.
func (c *conn) reply(m *jsonrpc.Message) {
	b, err := jsonrpc.Encode(m)
	if err != nil {
		return
	}
	if wire, err := Encode(ControlID, b); err == nil {
		c.enqueue(wire)
	}
}

func (c *conn) allowPush() bool {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()
	now := time.Now()
	if now.Sub(c.pushWindow) > time.Minute {
		c.pushWindow, c.pushCount = now, 0
	}
	c.pushCount++
	return c.pushCount <= pushBurst
}
