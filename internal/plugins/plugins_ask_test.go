package plugins

import "testing"

// Voice asks before everything that isn't a read. A method with its own
// approve asks that question itself, filled from the input, when it's called.
func TestWriteQuestionAsksBeforeWhatIsntARead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		m        Asking
		found    bool
		question string
	}{
		{"messages.send", Asking{Effect: "write"}, true, "Run send on messages?"},
		{"plans.tasks.delete", Asking{Effect: "destructive"}, true, "Run tasks.delete on plans?"},
		{"plans.tasks.list", Asking{Effect: "read"}, true, ""},
		{"fly.restart", Asking{Effect: "destructive", Approve: "Restart {{machine}}?"}, true, ""},
		{"nope.missing", Asking{}, false, ""},
	} {
		got, asks := writeQuestion(tc.m, tc.found, tc.name)
		if got != tc.question || asks != (tc.question != "") {
			t.Errorf("%s: %q, %v; want %q", tc.name, got, asks, tc.question)
		}
	}
}
