package fly

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestReadApp(t *testing.T) {
	run := func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "machine":
			return []byte(`[]`), nil
		case "releases":
			return []byte(`[
				{"Version":2,"Status":"failed","CreatedAt":"2026-10-01T18:00:00Z"},
				{"Version":3,"Status":"complete","CreatedAt":"2026-10-01T19:00:00Z"},
				{"Version":1,"Status":"complete","CreatedAt":"2026-09-30T19:00:00Z"}]`), nil
		case "certs":
			return []byte(`[{"hostname":"www.example.com","status":"Ready"},{"hostname":"example.com","status":"Ready"},{"hostname":"new.example.com","status":"Awaiting configuration"}]`), nil
		}
		t.Fatalf("args: %v", args)
		return nil, nil
	}
	got, err := readWith(t.Context(), "api", run)
	want := App{Machines: &MachineSummary{Regions: []string{}}, URL: "https://example.com", Latest: &Release{Version: 3, Status: "complete", At: time.Date(2026, 10, 1, 19, 0, 0, 0, time.UTC)}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v %+v, %v", got, got.Latest, err)
	}
}

// Without a ready certificate, or when certs can't be read, the URL is the
// app's fly.dev address; an app never deployed has no release.
func TestReadAppDefaults(t *testing.T) {
	got, err := readWith(t.Context(), "api", func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "certs" {
			return nil, errors.New("offline")
		}
		return []byte(`[]`), nil
	})
	if err != nil || !reflect.DeepEqual(got, App{URL: "https://api.fly.dev", Machines: &MachineSummary{Regions: []string{}}}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestReadAppFailsWithoutReleases(t *testing.T) {
	_, err := readWith(t.Context(), "api", func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "releases" {
			return nil, errors.New("Could not find App")
		}
		return []byte(`[]`), nil
	})
	if err == nil {
		t.Fatal("read succeeded without releases")
	}
}
