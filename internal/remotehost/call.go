package remotehost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/errkind"
)

// CallTimeout bounds one call to another host, login included: above
// `repogo call`'s own 6 minutes, room for chats.wait's 5.
const CallTimeout = 6*time.Minute + 30*time.Second

// perHost is how many calls go to one host at once. OpenSSH allows 10
// sessions on a shared connection (MaxSessions); past that a call fails as if
// the connection had, and so would its retry.
const perHost = 8

// SwitchOnHint ends a refusal for a host that is off: where the user turns it on.
const SwitchOnHint = "the user switches one on under Other Environments, in this environment's settings in the app"

// callCommand runs on the other host: its hosts.call, the call on stdin.
var callCommand = Repogo("call hosts.call")

// Request is one tool call for another host: its hosts.call's params.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	// From is the calling chat, as the caller knows it.
	From any `json:"from"`
	// To is the id the host answered with when it was checked: it refuses a
	// call meant for another, so one sent down a connection that now reaches
	// a different host doesn't run there. Call fills it.
	To string `json:"to"`
}

// Call sends one tool call to the host hosts.list calls id and answers the
// method's result. Only a host the user switched on is dialed; one that is
// off or isn't listed is refused in the same words and nothing is looked up,
// so a caller learns no names by asking. A read is sent again once if SSH
// itself failed; a write isn't: the other host may have run it before the drop.
func (s *Service) Call(ctx context.Context, id string, req Request, readOnly bool) (json.RawMessage, error) {
	refused := errkind.New(errkind.Denied, id+" isn't an environment that is switched on; "+SwitchOnHint)
	alias, on := s.choices.alias(id)
	if !on {
		return nil, refused
	}
	h, listed := s.find(ctx, alias)
	if !listed {
		return nil, refused
	}
	if h.State != Ready {
		// It may have come back, or been switched on, since the last look: one
		// kept from before it was switched on always looks again.
		if again, ok := named(s.look(ctx, h.State == Off).Hosts, h.ID); ok {
			h = again
		}
	}
	if h.State != Ready {
		return nil, errkind.New(errkind.Unavailable, h.ID+" can't be used: "+h.Reason)
	}
	req.To = h.HostID
	call, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// Switching the host off ends this call wherever it has got to: waiting
	// its turn, signing in, or running.
	ctx, stop := s.whileOn(ctx, h.ID)
	defer stop()
	switchedOff := func() error {
		s.d.debug("hosts: call ended, switched off", "alias", h.ID)
		return errkind.New(errkind.Denied, h.ID+" was switched off; "+SwitchOnHint)
	}

	release, err := s.slot(ctx, h.ID)
	if err == nil {
		defer release()
	}
	// Asked again once the call's turn comes: the switch itself, which is
	// off before any call to the host is ended.
	if !s.choices.has(h.ID) {
		return nil, switchedOff()
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()

	began := time.Now()
	stdout, stderr, code := s.d.Send(ctx, h.ID, callCommand, call)
	if code == 255 && readOnly && ctx.Err() == nil {
		s.d.debug("hosts: call sent again", "alias", h.ID, "stderr", firstLine(string(stderr)))
		stdout, stderr, code = s.d.Send(ctx, h.ID, callCommand, call)
	}
	s.d.debug("hosts: called", "alias", h.ID, "read_only", readOnly, "exit", code, "bytes", len(stdout),
		"stderr", firstLine(string(stderr)), "ms", time.Since(began).Milliseconds())
	switch {
	case code == 0:
		var answer struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(stdout, &answer); err != nil {
			return nil, fmt.Errorf("%s answered something that isn't a result: %w", h.ID, err)
		}
		return answer.Result, nil
	case !s.choices.has(h.ID):
		return nil, switchedOff()
	case ctx.Err() != nil:
		return nil, errkind.New(errkind.Unavailable, h.ID+" didn't answer in time")
	case code == 255 && !readOnly:
		return nil, errkind.New(errkind.Unavailable, h.ID+": the connection dropped, so whether it ran is unknown; check before trying again")
	case code == 255:
		return nil, errkind.New(errkind.Unavailable, h.ID+" can't be reached: "+firstLine(string(stderr)))
	case code == 127:
		return nil, errkind.New(errkind.Unavailable, "RepoGo isn't installed on "+h.ID)
	case code == ExitNotRunning:
		return nil, errkind.New(errkind.Unavailable, "RepoGo isn't running on "+h.ID+" right now; try again in a moment")
	}
	return nil, remoteError(h.ID, string(stderr))
}

// named is the host hosts_list calls id, ignoring case. Only the id: a
// host_id is what the other host says about itself, so matching it would let
// one host take calls meant for another.
func named(hosts []Host, id string) (Host, bool) {
	for _, h := range hosts {
		if strings.EqualFold(h.ID, id) {
			return h, true
		}
	}
	return Host{}, false
}

// slot holds one of perHost calls to alias until release.
func (s *Service) slot(ctx context.Context, alias string) (release func(), err error) {
	s.mu.Lock()
	sem, ok := s.slots[alias]
	if !ok {
		sem = make(chan struct{}, perHost)
		s.slots[alias] = sem
	}
	s.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// remoteError is the other host's own refusal, as `repogo call` printed it:
// "repogo: host: chats.start from another host isn't supported yet".
func remoteError(id, stderr string) error {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	msg := strings.TrimSpace(lines[len(lines)-1])
	msg = strings.TrimPrefix(msg, "repogo: ")
	msg = clean(strings.TrimPrefix(msg, "host: "), maxLine)
	// What a release from before `repogo call` prints, which can't change now.
	if strings.Contains(msg, "unknown command") {
		return errkind.New(errkind.Unavailable, "RepoGo on "+id+" is too old; update it there")
	}
	if msg == "" {
		msg = "the call failed"
	}
	return fmt.Errorf("%s: %s", id, msg)
}
