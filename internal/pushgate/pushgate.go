// Package pushgate is the relay's side of push.send: it checks that a phone
// granted the sending host its token, holds the rate limits, and hands what
// passes to APNs. The host never holds the APNs key.
package pushgate

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
)

// PushRequest is `push.send`: one APNs delivery on behalf of a host.
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

// PushGrant carries the device.PushProof envelope in Signature and the paired
// key whose grant authorizes this host to push to the token.
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

// ErrUnregistered is what a Deliver returns when APNs says the token is dead,
// so the host can forget it.
var ErrUnregistered = errors.New("apns: unregistered")

// HostBurst bounds how many pushes one host may send per minute: a host
// updates each phone's Live Activity every few seconds while agents work.
// Tokens are unguessable, so the exposure is our APNs quota, not a user.
const HostBurst = 240

// tokenBurst bounds pushes to one token per minute from everyone: the token
// is what an abuser targets, and identities are free to mint.
const tokenBurst = 120

// A grant is good for grantMaxAge, which bounds how long an unpaired host can
// still push; phones re-sign on every connection. grantSkew allows a fast phone clock.
const (
	grantMaxAge = 7 * 24 * time.Hour
	grantSkew   = 5 * time.Minute
)

var errGrant = errors.New("push grant does not verify for this host and token")

// Deliver sends one request to APNs.
type Deliver func(context.Context, PushRequest) error

// VerifyApp checks the App Attest evidence a grant carries, as of when it was
// signed: Apple's certificates last about a day.
type VerifyApp func(pub, msg, attestation, assertion []byte, environment string, at time.Time) error

// Gate answers push.send for every host linked to one relay.
type Gate struct {
	deliver Deliver
	verify  VerifyApp
	now     func() time.Time

	mu     sync.Mutex
	tokens map[string]*Window
}

// New is a gate that sends through deliver; nil means this relay has no APNs
// key and refuses every push.
func New(deliver Deliver, verify VerifyApp) *Gate {
	return &Gate{deliver: deliver, verify: verify, now: time.Now, tokens: map[string]*Window{}}
}

// Send checks req from host and delivers it. A refusal is a *jsonrpc.Error:
// CodeNotFound means the token is dead and the host should forget it.
func (g *Gate) Send(ctx context.Context, host device.ID, req PushRequest) error {
	if g.deliver == nil {
		return &jsonrpc.Error{Code: jsonrpc.CodeUnavailable, Message: "this relay has no APNs key"}
	}
	if req.Token == "" || len(req.Payload) == 0 {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "token and payload are required"}
	}
	if req.Environment != EnvironmentSandbox && req.Environment != EnvironmentProduction {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "environment must be sandbox or production"}
	}
	if err := g.verifyGrant(host, req); err != nil {
		return &jsonrpc.Error{Code: jsonrpc.CodeDenied, Message: err.Error()}
	}
	if !g.allowToken(req.Token) {
		return &jsonrpc.Error{Code: jsonrpc.CodeDenied, Message: "push rate limit for this token"}
	}
	switch err := g.deliver(ctx, req); {
	case errors.Is(err, ErrUnregistered):
		return &jsonrpc.Error{Code: jsonrpc.CodeNotFound, Message: err.Error()}
	case err != nil:
		return &jsonrpc.Error{Code: jsonrpc.CodeInternal, Message: err.Error()}
	}
	return nil
}

// Both signatures bind the host and token; App Attest authenticates the app
// that restricts signing to tokens obtained from its notification APIs.
func (g *Gate) verifyGrant(host device.ID, req PushRequest) error {
	gr := req.Grant
	pub := gr.PublicKey
	if !device.ValidPhoneKey(pub) || device.IDFor(pub) != gr.Device {
		return errGrant
	}
	now := g.now()
	granted := time.UnixMilli(gr.GrantedAt)
	if granted.Before(now.Add(-grantMaxAge)) || granted.After(now.Add(grantSkew)) {
		return errGrant
	}
	msg := device.PushGrantMessage(host, req.Token, req.Environment, gr.GrantedAt)
	proof, err := device.VerifyPushProof(pub, msg, gr.Signature)
	if err != nil {
		return errGrant
	}
	if g.verify == nil {
		return errGrant
	}
	return g.verify(pub, msg, proof.Attestation, proof.Assertion, req.Environment, granted)
}

// allowToken admits one push to token within its minute budget. Windows
// that have lapsed are dropped as they are met, so the map tracks only
// tokens pushed to in the last minute.
func (g *Gate) allowToken(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for t, w := range g.tokens {
		if now.Sub(w.since) > time.Minute {
			delete(g.tokens, t)
		}
	}
	w := g.tokens[token]
	if w == nil {
		w = &Window{}
		g.tokens[token] = w
	}
	return w.allow(now, tokenBurst)
}

// Window counts events in a one-minute window, for one host's pushes.
type Window struct {
	mu    sync.Mutex
	since time.Time
	count int
}

// Allow admits one more event if fewer than limit have come this minute.
func (w *Window) Allow(limit int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.allow(time.Now(), limit)
}

func (w *Window) allow(now time.Time, limit int) bool {
	if now.Sub(w.since) > time.Minute {
		w.since, w.count = now, 0
	}
	w.count++
	return w.count <= limit
}
