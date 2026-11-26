package tunnel

import "testing"

// A visitor can't pose as a proxy to the dev server.
func TestProxyHeadersNeverCross(t *testing.T) {
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Scheme", "X-Real-IP", "True-Client-IP", "X-Original-URL", "Forwarded"} {
		if !dropped(name) {
			t.Errorf("%s crosses the tunnel", name)
		}
	}
	if dropped("Accept") || dropped("X-Hub-Signature-256") {
		t.Error("an ordinary header was dropped")
	}
}
