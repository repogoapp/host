package main

import "testing"

func TestPassthroughIsOnForRepoGosRelayOnly(t *testing.T) {
	cases := []struct{ relay, override, want string }{
		{defaultRelay, "", defaultPassthrough},
		{"wss://relay.example.com/ws", "", ""},
		{"", "", ""},
		{defaultRelay, "off", ""},
		{"wss://relay.example.com/ws", "h.example.com:443", "h.example.com:443"},
	}
	for _, c := range cases {
		if got := passthroughFor(c.relay, c.override); got != c.want {
			t.Errorf("passthroughFor(%q, %q) = %q, want %q", c.relay, c.override, got, c.want)
		}
	}
}
