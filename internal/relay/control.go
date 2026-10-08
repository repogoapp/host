package relay

import (
	"context"
	"crypto/ed25519"
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
	// Grant is the phone's leave for the sending host to push to Token.
	Grant PushGrant `json:"grant"`
}

// PushGrant is a phone's signature over device.PushGrantMessage naming the
// host that may push to one of its tokens, with the key it signed with.
type PushGrant struct {
	Device    device.ID `json:"device"`
	PublicKey []byte    `json:"public_key"`
	Signature []byte    `json:"signature"`
	GrantedAt int64     `json:"granted_at"`
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

// tokenBurst bounds pushes to one token per minute from everyone: the token
// is what an abuser targets, and identities are free to mint.
const tokenBurst = 120

// A grant is good for grantMaxAge; grantSkew allows a phone clock ahead of ours.
const (
	grantMaxAge = 90 * 24 * time.Hour
	grantSkew   = 5 * time.Minute
)

var errGrant = errors.New("push grant does not verify for this host and token")

// verifyGrant checks that the token's phone let this host push to it: the
// key is the phone it names, the signature covers this host, token and
// environment, and it was signed recently and not in the future.
func verifyGrant(host device.ID, req PushRequest, now time.Time) error {
	g := req.Grant
	pub := ed25519.PublicKey(g.PublicKey)
	if len(pub) != ed25519.PublicKeySize || device.IDFor(pub) != g.Device {
		return errGrant
	}
	granted := time.UnixMilli(g.GrantedAt)
	if granted.Before(now.Add(-grantMaxAge)) || granted.After(now.Add(grantSkew)) {
		return errGrant
	}
	msg := device.PushGrantMessage(host, req.Token, req.Environment, g.GrantedAt)
	if !ed25519.Verify(pub, msg, g.Signature) {
		return errGrant
	}
	return nil
}

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
	if err := verifyGrant(from.id, req, time.Now()); err != nil {
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeDenied, err.Error())
	}
	if !s.allowToken(req.Token) {
		return jsonrpc.Fail(msg.ID, jsonrpc.CodeDenied, "push rate limit for this token")
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

// allowToken admits one push to token within its minute budget. Windows
// that have lapsed are dropped as they are met, so the map tracks only
// tokens pushed to in the last minute.
func (s *Server) allowToken(token string) bool {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	now := time.Now()
	for t, w := range s.tokens {
		if now.Sub(w.since) > time.Minute {
			delete(s.tokens, t)
		}
	}
	w := s.tokens[token]
	if w == nil {
		w = &tokenWindow{since: now}
		s.tokens[token] = w
	}
	w.count++
	return w.count <= tokenBurst
}

type tokenWindow struct {
	since time.Time
	count int
}
