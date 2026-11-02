package claude

import "testing"

func TestParseAuthStatus(t *testing.T) {
	cases := []struct {
		name           string
		out            string
		loggedIn, ok   bool
		wantAuthMethod string
	}{
		{"subscription", `{"loggedIn": true, "authMethod": "claude.ai"}`, true, true, "claude.ai"},
		// What a token that expired and failed to refresh reports, while its
		// Keychain item is still there.
		{"signed out", `{"loggedIn": false, "authMethod": "none"}`, false, true, "none"},
		{"not json", "error: unknown command 'auth'", false, false, ""},
		{"no field", `{"authMethod": "none"}`, false, false, ""},
	}
	for _, c := range cases {
		loggedIn, method, ok := parseAuthStatus([]byte(c.out))
		if loggedIn != c.loggedIn || ok != c.ok || method != c.wantAuthMethod {
			t.Errorf("%s: got %v %q %v, want %v %q %v", c.name, loggedIn, method, ok, c.loggedIn, c.wantAuthMethod, c.ok)
		}
	}
}
