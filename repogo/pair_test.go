package main

import "testing"

func TestReusableHours(t *testing.T) {
	for arg, want := range map[string]int{"14d": 336, "36h": 36, "1d": 24} {
		if got, err := reusableHours(arg); err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", arg, got, err, want)
		}
	}
	for _, arg := range []string{"d", "0d", "-1d", "2w", "14"} {
		if _, err := reusableHours(arg); err == nil {
			t.Errorf("%s was accepted", arg)
		}
	}
}
