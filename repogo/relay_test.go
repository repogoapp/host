package main

import "testing"

func TestRelayIsRepoGosUnlessSet(t *testing.T) {
	empty, own := "", "h.example.com:443"
	cases := []struct {
		flagged  *string
		override string
		want     string
	}{
		{nil, "", defaultRelay},
		{nil, "off", ""},
		{nil, own, own},
		{&empty, own, ""},
		{&own, "off", own},
	}
	for _, c := range cases {
		if got := relayFor(c.flagged, c.override); got != c.want {
			t.Errorf("relayFor(%v, %q) = %q, want %q", c.flagged, c.override, got, c.want)
		}
	}
}
