// Package pushgate is the relay's side of push.send: it checks that a phone
// granted the sending host its token, holds the rate limits, and hands what
// passes to APNs or, for Android, FCM. The host never holds either key.
package pushgate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
)

// PushRequest is `push.send`: one delivery on behalf of a host.
type PushRequest struct {
	Token string `json:"token"`
	// Service is who delivers: "apns" (also when empty, as older hosts send)
	// or "fcm", whose payload is the data message's flat map of strings.
	Service     string          `json:"service,omitempty"`
	Environment string          `json:"environment"`
	CollapseID  string          `json:"collapse_id,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	// PushType is "alert" (the default) or "liveactivity", which starts or
	// updates a Live Activity and goes to the bundle's liveactivity topic.
	PushType string `json:"push_type,omitempty"`
	// Priority is 10 (at once, the default) or 5 (when power allows: Live
	// Activity updates not worth waking the phone for; FCM's normal priority,
	// dropped after a minute).
	Priority int `json:"priority,omitempty"`
	// Grant is the phone's leave for the sending host to push to Token.
	Grant PushGrant `json:"grant"`
	// Integrity is the Play Integrity token an Android phone got with its
	// grant; the relay decodes it on the grant's first push.
	Integrity string `json:"integrity,omitempty"`
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

// The services a push goes through.
const (
	ServiceAPNs = "apns"
	ServiceFCM  = "fcm"
)

// The APNs environments a phone's token can come from.
const (
	EnvironmentSandbox    = "sandbox"
	EnvironmentProduction = "production"
)

// ErrUnregistered is what a Deliver returns when the service says the token is dead,
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

// Deliver sends one request through one service.
type Deliver func(context.Context, PushRequest) error

// VerifyApp checks the App Attest evidence a grant carries, as of when it was
// signed: Apple's certificates last about a day.
type VerifyApp func(pub, msg, attestation, assertion []byte, environment string, at time.Time) error

// Gate answers push.send for every host linked to one relay.
type Gate struct {
	deliver Deliver
	verify  VerifyApp
	now     func() time.Time

	// fcm delivers Android's pushes; nil refuses them. A grant passes Play
	// Integrity, or comes from a trusted device: a test phone running a build
	// Play didn't install, which no other phone's signature can claim to be.
	fcm       Deliver
	integrity CheckIntegrity
	trusted   map[device.ID]bool
	// Play Integrity decodes per host, a minute at a time: a host minting
	// fake grants spends Google's quota no faster than this.
	decodes map[device.ID]*Window

	mu     sync.Mutex
	tokens map[string]*Window
}

// New is a gate that sends through deliver to APNs; nil means this relay has
// no APNs key and refuses iOS's pushes.
func New(deliver Deliver, verify VerifyApp) *Gate {
	return &Gate{deliver: deliver, verify: verify, now: time.Now, tokens: map[string]*Window{}}
}

// decodesPerMinute bounds each host's Play Integrity decodes: a phone signs
// a new grant on each connect, so a real host needs a handful.
const decodesPerMinute = 20

// WithFCM has the gate deliver Android's pushes through fcm, for grants that
// pass integrity (nil passes none) or come from a trusted device.
func (g *Gate) WithFCM(fcm Deliver, integrity CheckIntegrity, trusted []device.ID) *Gate {
	g.fcm, g.integrity, g.trusted, g.decodes = fcm, integrity, map[device.ID]bool{}, map[device.ID]*Window{}
	for _, id := range trusted {
		g.trusted[id] = true
	}
	return g
}

// Send checks req from host and delivers it. A refusal is a *jsonrpc.Error:
// CodeNotFound means the token is dead and the host should forget it.
func (g *Gate) Send(ctx context.Context, host device.ID, req PushRequest) error {
	deliver := g.deliver
	switch cmp.Or(req.Service, ServiceAPNs) {
	case ServiceAPNs:
		if req.Environment != EnvironmentSandbox && req.Environment != EnvironmentProduction {
			return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "environment must be sandbox or production"}
		}
	case ServiceFCM:
		deliver = g.fcm
		if req.Environment != "" {
			return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "an fcm token has no environment"}
		}
	default:
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "service must be apns or fcm"}
	}
	if deliver == nil {
		return &jsonrpc.Error{Code: jsonrpc.CodeUnavailable, Message: "this relay has no key for " + cmp.Or(req.Service, ServiceAPNs)}
	}
	if req.Token == "" || len(req.Payload) == 0 {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "token and payload are required"}
	}
	if err := g.verifyGrant(ctx, host, req); err != nil {
		return &jsonrpc.Error{Code: jsonrpc.CodeDenied, Message: err.Error()}
	}
	if !g.allowToken(req.Token) {
		return &jsonrpc.Error{Code: jsonrpc.CodeDenied, Message: "push rate limit for this token"}
	}
	switch err := deliver(ctx, req); {
	case errors.Is(err, ErrUnregistered):
		return &jsonrpc.Error{Code: jsonrpc.CodeNotFound, Message: err.Error()}
	case err != nil:
		return &jsonrpc.Error{Code: jsonrpc.CodeInternal, Message: err.Error()}
	}
	return nil
}

// Both signatures bind the host and token; App Attest authenticates the app
// that restricts signing to tokens obtained from its notification APIs.
func (g *Gate) verifyGrant(ctx context.Context, host device.ID, req PushRequest) error {
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
	if req.Service == ServiceFCM {
		return g.verifyAndroid(ctx, host, req, msg, granted)
	}
	if g.verify == nil {
		return errGrant
	}
	return g.verify(pub, msg, proof.Attestation, proof.Assertion, req.Environment, granted)
}

// verifyAndroid passes a trusted device's grant, else asks Play Integrity
// whether the real app signed it, within the host's decode budget.
func (g *Gate) verifyAndroid(ctx context.Context, host device.ID, req PushRequest, msg []byte, granted time.Time) error {
	if g.trusted[req.Grant.Device] {
		return nil
	}
	if g.integrity == nil {
		return errors.New("this relay can't check Play Integrity, and the phone isn't a trusted device")
	}
	return g.integrity(ctx, req.Grant.PublicKey, msg, granted, req.Integrity, func() bool { return g.allowDecode(host) })
}

// allowDecode spends one of host's Play Integrity decodes this minute.
func (g *Gate) allowDecode(host device.ID) bool {
	g.mu.Lock()
	w := g.decodes[host]
	if w == nil {
		w = &Window{}
		g.decodes[host] = w
	}
	g.mu.Unlock()
	return w.Allow(decodesPerMinute)
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
