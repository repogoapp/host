package redact

import (
	"strings"
	"testing"
)

func TestKnownShapesAreMasked(t *testing.T) {
	cases := map[string]string{
		"stripe":      "charge with sk_live_51Hx9AbCdEfGhIjKlMnOpQrSt done",
		"stripe test": "sk_test_4eC39HqLyjWDarjtT1zdp7dc",
		"webhook":     "whsec_8f1c2b3d4e5f60718293a4b5c6d7e8f9",
		"github":      "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"github pat":  "github_pat_11ABCDEFG0123456789_abcdefghijklmnop",
		"slack":       "xoxb-1234567890-abcdefghijklmnop",
		"anthropic":   "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123",
		"openai":      "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789",
		"plugin key":  "rgk_Qm9wcHJfaXNfdGhlX2Jlc3Rfa2V5X2V2ZXI",
		"aws":         "AKIAIOSFODNN7EXAMPLE",
		"jwt":         "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		"bearer":      "Authorization: Bearer abcdefghijklmnopqrstuvwxyz",
		"json field":  `{"api_key":"plain-looking-value-123","name":"x"}`,
		"env line":    "STRIPE_SECRET=notashapeanyoneknows",
		"yaml field":  "password: hunter2hunter2",
		"nested name": `"client_secret": "zzzzzzzzzzzzzzzz"`,
		"token field": "token=abcdefghij",
	}
	secretParts := []string{"51Hx9", "4eC39", "8f1c2b", "ABCDEFGHIJ", "11ABCDEFG", "1234567890-abc", "api03", "proj-abc", "Qm9wcHJ", "IOSFODNN", "SflKx", "abcdefghijklmnopqrstuvwxyz", "plain-looking", "notashape", "hunter2", "zzzzzzzz", "abcdefghij"}
	for name, in := range cases {
		out := Text(in)
		if !strings.Contains(out, Mask) {
			t.Errorf("%s: nothing masked in %q", name, out)
		}
		for _, part := range secretParts {
			if strings.Contains(out, part) && !strings.Contains(name, "bearer") {
				t.Errorf("%s: %q still shows %q", name, out, part)
			}
		}
	}
	if got := Text(`{"api_key":"plain-looking-value-123","name":"x"}`); got != `{"api_key":"`+Mask+`","name":"x"}` {
		t.Errorf("a field's name and the rest stay: %s", got)
	}
}

func TestOrdinaryTextIsLeftAlone(t *testing.T) {
	for _, s := range []string{
		"listed 3 tasks for project web",
		"GET /api/items?page=2 200 in 12ms",
		`{"id":"abc123","name":"Deploy","key":"home"}`,
		"the token count was 1200",
		"skating is fun, sk8 forever",
		"password: short",
	} {
		if got := Text(s); got != s {
			t.Errorf("%q became %q", s, got)
		}
	}
}

func TestVaultMasksItsValuesLongestFirst(t *testing.T) {
	v := New()
	v.Add("abc", "", "topsecretvalue", "topsecretvalue-and-more")
	got := v.Text("sent topsecretvalue-and-more then topsecretvalue, abc stays")
	if got != "sent "+Mask+" then "+Mask+", abc stays" {
		t.Fatalf("got %q", got)
	}
	// A value inside JSON is masked without breaking the rest.
	if got := v.Text(`{"token":"topsecretvalue","n":1}`); got != `{"token":"`+Mask+`","n":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestVaultIsSafeToAddWhileReading(t *testing.T) {
	v := New()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			v.Add(strings.Repeat("x", 8+i%20))
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = v.Text("xxxxxxxxxxxx and more")
	}
	<-done
}
