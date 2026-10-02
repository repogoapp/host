package cloudflare

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func fakeCLI(t *testing.T, whoami string) runFunc {
	return func(_ context.Context, env, args []string) ([]byte, error) {
		switch args[0] {
		case "whoami":
			return []byte(whoami), nil
		case "auth":
			return []byte(`{"token":"tok","type":"oauth"}`), nil
		case "deployments":
			if !slices.Contains(env, "CLOUDFLARE_ACCOUNT_ID=acc_1") {
				t.Errorf("deployments without the account: %v", env)
			}
			return []byte(`[
				{"created_on":"2026-10-01T18:00:00Z","versions":[{"version_id":"v_old"}]},
				{"created_on":"2026-10-01T19:00:00Z","versions":[{"version_id":"v_new","percentage":100}]}]`), nil
		}
		t.Fatalf("args: %v", args)
		return nil, nil
	}
}

func TestReadWorkerOnWorkersDev(t *testing.T) {
	get := func(_ context.Context, token, path string) ([]byte, error) {
		if token != "tok" {
			t.Errorf("token %q", token)
		}
		if strings.Contains(path, "/workers/domains") {
			return []byte(`{"result":[]}`), nil
		}
		return []byte(`{"result":{"subdomain":"team"}}`), nil
	}
	got, err := readWith(t.Context(), "edge", "acc_1", fakeCLI(t, ""), get)
	want := Worker{URL: "https://edge.team.workers.dev", Latest: &Deployment{VersionID: "v_new", At: time.Date(2026, 10, 1, 19, 0, 0, 0, time.UTC)}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v %+v, %v", got, got.Latest, err)
	}
}

func TestReadWorkerCustomDomainAndOnlyAccount(t *testing.T) {
	get := func(_ context.Context, _, path string) ([]byte, error) {
		if !strings.HasPrefix(path, "/accounts/acc_1/") {
			t.Errorf("path %q", path)
		}
		return []byte(`{"result":[{"hostname":"www.example.com"},{"hostname":"example.com"}]}`), nil
	}
	got, err := readWith(t.Context(), "edge", "", fakeCLI(t, `{"accounts":[{"id":"acc_1"}]}`), get)
	if err != nil || got.URL != "https://example.com" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestReadWorkerNeedsAnAccount(t *testing.T) {
	_, err := readWith(t.Context(), "edge", "", fakeCLI(t, `{"accounts":[{"id":"a"},{"id":"b"}]}`), nil)
	if !errors.Is(err, ErrAccountUnknown) {
		t.Fatalf("err = %v", err)
	}
}

// The address is a nicety: without the API, the card still shows deploys.
func TestReadWorkerWithoutAddress(t *testing.T) {
	get := func(context.Context, string, string) ([]byte, error) { return nil, errors.New("403") }
	got, err := readWith(t.Context(), "edge", "acc_1", fakeCLI(t, ""), get)
	if err != nil || got.URL != "" || got.Latest == nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}
