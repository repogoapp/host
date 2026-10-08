package main

import (
	"io"
	"log/slog"
	"testing"
)

func TestServePprofRefusesNonLoopback(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.2:0", "example.com:0", "nonsense"} {
		if err := servePprof(log, addr); err == nil {
			t.Errorf("servePprof(%q) = nil, want an error", addr)
		}
	}
	if err := servePprof(log, "127.0.0.1:0"); err != nil {
		t.Errorf("servePprof(127.0.0.1:0) = %v", err)
	}
}
