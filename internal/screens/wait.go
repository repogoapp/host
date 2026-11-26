package screens

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Waiting on a screen, for agents: until something shows, until it's gone,
// or until the screen stops changing. It replaces guessing how long an app
// takes (a sleep, then a read that may still be early) with reads until it's
// so, and answers the last read, so the next action needs none of its own.

// WaitFor is what a wait waits for: one of Label, Gone or Stable.
type WaitFor struct {
	// Label waits until an element's label, id or field text contains it.
	Label string `json:"label,omitempty"`
	// Gone waits until no element's does.
	Gone string `json:"gone,omitempty"`
	// Stable waits until two reads in a row show the same screen.
	Stable bool `json:"stable,omitempty"`
	// TimeoutMS bounds it: 10 s when absent, at most 60 s.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// Waited is how a wait ended: whether it got what it waited for, after how
// long, and the screen as last read (its refs are the current ones).
type Waited struct {
	Met      bool     `json:"met"`
	WaitedMS int      `json:"waited_ms"`
	Read     Elements `json:"read"`
}

// waitPoll is the pause between reads; a read itself takes a few hundred ms.
const waitPoll = 200 * time.Millisecond

// Wait reads the screen until what w asks for is so, or its time runs out.
func (m *Manager) Wait(ctx context.Context, id string, w WaitFor) (Waited, error) {
	if !exactlyOne(w.Label != "", w.Gone != "", w.Stable) {
		return Waited{}, errInvalid.Errorf("wait takes one of label, gone or stable")
	}
	timeout := 10 * time.Second
	if w.TimeoutMS > 0 {
		timeout = time.Duration(min(w.TimeoutMS, 60_000)) * time.Millisecond
	}
	c, err := m.agentController(ctx, id)
	if err != nil {
		return Waited{}, err
	}
	start := time.Now()
	previous := ""
	for {
		c.agent.lock.Lock()
		read, err := c.readElements(ctx)
		c.agent.lock.Unlock()
		if err != nil {
			return Waited{}, err
		}
		var met bool
		switch {
		case w.Label != "":
			met = anyMatches(read.Elements, w.Label)
		case w.Gone != "":
			met = !anyMatches(read.Elements, w.Gone)
		default:
			fingerprint := screenFingerprint(read)
			met, previous = fingerprint == previous, fingerprint
		}
		waited := int(time.Since(start) / time.Millisecond)
		if met || time.Since(start) >= timeout {
			return Waited{Met: met, WaitedMS: waited, Read: read}, nil
		}
		select {
		case <-ctx.Done():
			return Waited{}, ctx.Err()
		case <-time.After(waitPoll):
		}
	}
}

// anyMatches is whether an element's label, id or field text contains text.
func anyMatches(elements []Element, text string) bool {
	want := strings.ToLower(text)
	for _, e := range elements {
		if strings.Contains(strings.ToLower(e.Label), want) || strings.Contains(strings.ToLower(e.ID), want) ||
			e.isField() && strings.Contains(strings.ToLower(e.Value), want) {
			return true
		}
	}
	return false
}

// screenFingerprint is a read without its refs or the clock: two reads of
// an unchanged screen have the same one.
func screenFingerprint(e Elements) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%gx%g|", e.Width, e.Height)
	for _, el := range e.Elements {
		// The status bar's clock and signal change on their own.
		if el.isSystemUI() {
			continue
		}
		fmt.Fprintf(&b, "%s|%s|%s|%g,%g,%g,%g;", el.Role, el.Label, el.Value, el.X, el.Y, el.W, el.H)
	}
	return b.String()
}

// settleAfter is how long an action's answer waits for the screen to stop
// changing: most taps settle well inside it; a slower one answers as it is.
const settleAfter = 2500 * time.Millisecond

// Settled is the screen once it stops changing after an action, for an
// agent's answer: the read it would make next, done for it, as what changed
// since the screen before the action where most of it stayed.
func (m *Manager) Settled(ctx context.Context, id string) *Elements {
	var before *Elements
	if c, err := m.agentController(ctx, id); err == nil {
		c.mu.Lock()
		before = c.agent.stale
		c.mu.Unlock()
	}
	waited, err := m.Wait(ctx, id, WaitFor{Stable: true, TimeoutMS: int(settleAfter / time.Millisecond)})
	if err != nil {
		return nil
	}
	return changesSince(before, waited.Read)
}

// changesSince is after as an action's answer: only what's new or changed
// since before, with the refs gone, when at least half the screen stayed
// and its size didn't change; otherwise the whole read.
func changesSince(before *Elements, after Elements) *Elements {
	if before == nil || before.Width != after.Width || before.Height != after.Height {
		return &after
	}
	was := map[string]Element{}
	for _, e := range before.Elements {
		was[e.Ref] = e
	}
	diff := after
	diff.Elements = []Element{}
	for _, e := range after.Elements {
		if old, ok := was[e.Ref]; ok && sameElement(old, e) {
			diff.Unchanged++
		} else {
			diff.Elements = append(diff.Elements, e)
		}
		delete(was, e.Ref)
	}
	if diff.Unchanged*2 < len(after.Elements) {
		return &after
	}
	for _, e := range before.Elements {
		if _, gone := was[e.Ref]; gone {
			diff.Gone = append(diff.Gone, e.Ref)
		}
	}
	diff.Changes = true
	return &diff
}

// sameElement is whether a read shows an element as it was: its value, its
// frame and its states.
func sameElement(a, b Element) bool {
	checked := func(c *bool) int {
		if c == nil {
			return -1
		}
		if *c {
			return 1
		}
		return 0
	}
	return a.Value == b.Value && a.X == b.X && a.Y == b.Y && a.W == b.W && a.H == b.H &&
		a.Selected == b.Selected && checked(a.Checked) == checked(b.Checked) && a.Disabled == b.Disabled &&
		a.Focused == b.Focused && a.Covered == b.Covered
}
