package agent

import "testing"

func TestActiveIsATurnInFlightOrBlocked(t *testing.T) {
	for _, s := range []ChatStatus{ChatQueued, ChatWorking, ChatAwaitingApproval, ChatAwaitingUser} {
		if !s.Active() {
			t.Errorf("%s is not active", s)
		}
	}
	for _, s := range []ChatStatus{ChatUnknown, ChatIdle, ChatCompleted, ChatFailed, ChatCancelled, ChatInterrupted} {
		if s.Active() {
			t.Errorf("%s is active", s)
		}
	}
}
