package agent

import (
	"testing"
	"time"
)

func TestStallSweepRespectsProviderWorkAndHumanInput(t *testing.T) {
	cutoff := time.Now()
	tr := &turn{status: TurnStatus{State: StateRunning}, lastEvent: cutoff.Add(-time.Hour)}
	if !tr.lastEventBefore(cutoff) {
		t.Fatal("inactive turn not stalled")
	}
	tr.providerBusy = true
	if tr.lastEventBefore(cutoff) {
		t.Fatal("background work marked stalled")
	}
	tr.providerBusy = false
	tr.status.Approval = &Approval{CallID: "approval"}
	if tr.lastEventBefore(cutoff) {
		t.Fatal("human input marked stalled")
	}
	tr.status.Approval = nil
	tr.activity(false)
	if tr.lastEventBefore(cutoff) {
		t.Fatal("recent protocol activity marked stalled")
	}
}
