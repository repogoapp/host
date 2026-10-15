package main

import "testing"

func TestRelayForPrefersTheFlagThenTheSavedRelay(t *testing.T) {
	own, off := "wss://relay.example.com/ws", ""
	for _, tc := range []struct {
		name    string
		flagged *string
		saved   string
		want    string
	}{
		{"nothing set", nil, "", defaultRelay},
		{"saved", nil, own, own},
		{"flag over saved", &own, "wss://other.example.com/ws", own},
		{"empty flag disables", &off, own, ""},
	} {
		if got := relayFor(tc.flagged, tc.saved); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCheckRelayWantsAWebSocketURL(t *testing.T) {
	for _, ok := range []string{"wss://relay.example.com/ws", "ws://192.168.1.4:8080/ws"} {
		if err := checkRelay(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "relay.example.com", "https://relay.example.com/ws", "wss:///ws"} {
		if err := checkRelay(bad); err == nil {
			t.Errorf("%q passed as a relay URL", bad)
		}
	}
}
