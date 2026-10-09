package hostinfo

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestAMacHasNoCloudSession(t *testing.T) {
	s := newService(reading{})
	if err := s.LoadCloud(filepath.Join(t.TempDir(), "cloud.json"), env(nil)); err != nil {
		t.Fatal(err)
	}
	if s.Cloud() != nil {
		t.Fatalf("cloud %+v with no environment", s.Cloud())
	}
	if _, err := s.SetStopsAt(time.Now().Add(time.Hour)); !errors.Is(err, ErrNotCloud) {
		t.Fatalf("SetStopsAt on a Mac: %v, want ErrNotCloud", err)
	}
	status, _ := s.Status(context.Background())
	if status.Cloud != nil {
		t.Fatalf("status cloud %+v on a Mac", status.Cloud)
	}
	if !slices.Contains(status.Capabilities, CapabilitySchedules) || !slices.Contains(status.Capabilities, CapabilityUsage) ||
		!slices.Contains(status.Capabilities, CapabilityDevices) {
		t.Fatalf("capabilities %v on a Mac, want devices, schedules and usage", status.Capabilities)
	}
}

func TestAnExtensionSurvivesARestartAndANewSessionReplacesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloud.json")
	first := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	started := env(map[string]string{CloudKindEnv: "vercel-sandbox", CloudStopsAtEnv: first.Format(time.RFC3339)})

	s := newService(reading{})
	if err := s.LoadCloud(path, started); err != nil {
		t.Fatal(err)
	}
	if c := s.Cloud(); c == nil || c.Provider != "vercel-sandbox" || !c.StopsAt.Equal(first) {
		t.Fatalf("cloud %+v, want vercel-sandbox until %v", c, first)
	}
	extended := first.Add(time.Hour)
	if _, err := s.SetStopsAt(extended); err != nil {
		t.Fatal(err)
	}
	status, _ := s.Status(context.Background())
	if status.Cloud == nil || !status.Cloud.StopsAt.Equal(extended) {
		t.Fatalf("status cloud %+v, want until %v", status.Cloud, extended)
	}
	if slices.Contains(status.Capabilities, CapabilitySchedules) || !slices.Contains(status.Capabilities, CapabilityUsage) ||
		!slices.Contains(status.Capabilities, CapabilityDevices) {
		t.Fatalf("capabilities %v on a bounded session, want devices and usage only", status.Capabilities)
	}

	// A restart within the session (a host update) has no environment of its
	// own to say otherwise: the extension holds.
	restarted := newService(reading{})
	if err := restarted.LoadCloud(path, env(nil)); err != nil {
		t.Fatal(err)
	}
	if c := restarted.Cloud(); c == nil || !c.StopsAt.Equal(extended) {
		t.Fatalf("after restart %+v, want until %v", c, extended)
	}

	// Start names a fresh session, which replaces the saved one.
	next := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	resumed := newService(reading{})
	if err := resumed.LoadCloud(path, env(map[string]string{CloudKindEnv: "vercel-sandbox", CloudStopsAtEnv: next.Format(time.RFC3339)})); err != nil {
		t.Fatal(err)
	}
	if c := resumed.Cloud(); c == nil || !c.StopsAt.Equal(next) {
		t.Fatalf("new session %+v, want until %v", c, next)
	}
}

func TestSetStopsAtRefusesThePast(t *testing.T) {
	s := newService(reading{})
	started := env(map[string]string{CloudKindEnv: "vercel-sandbox", CloudStopsAtEnv: time.Now().Add(time.Hour).Format(time.RFC3339)})
	if err := s.LoadCloud(filepath.Join(t.TempDir(), "cloud.json"), started); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStopsAt(time.Now().Add(-time.Minute)); !errors.Is(err, ErrStopsAtPast) {
		t.Fatalf("past stops_at: %v, want ErrStopsAtPast", err)
	}
}

func TestWatchStopWarnsBeforeTheEndAndAgainAfterAnExtension(t *testing.T) {
	s := newService(reading{})
	started := env(map[string]string{CloudKindEnv: "vercel-sandbox", CloudStopsAtEnv: time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)})
	if err := s.LoadCloud(filepath.Join(t.TempDir(), "cloud.json"), started); err != nil {
		t.Fatal(err)
	}
	warnings := make(chan Cloud, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.WatchStop(ctx, 1500*time.Millisecond, func(c Cloud) { warnings <- c })

	first := <-warnings
	select {
	case extra := <-warnings:
		t.Fatalf("second warning %+v with no extension", extra)
	case <-time.After(300 * time.Millisecond):
	}

	extended := time.Now().Add(2 * time.Second).UTC().Truncate(time.Second).Add(time.Second)
	if _, err := s.SetStopsAt(extended); err != nil {
		t.Fatal(err)
	}
	select {
	case again := <-warnings:
		if !again.StopsAt.Equal(extended) || again.StopsAt.Equal(first.StopsAt) {
			t.Fatalf("warning after extension %+v, want until %v", again, extended)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no warning after the extension")
	}
}
