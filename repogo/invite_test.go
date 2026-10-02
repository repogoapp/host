package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/device"
)

// A provisioned host pairs from the invite `repogo invite` prints, once.
func TestInvitePrintsOneSingleUseInvite(t *testing.T) {
	home := t.TempDir()
	t.Setenv(apphome.EnvVar, home)
	store, err := device.Open(filepath.Join(home, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	pairer := device.NewPairer(store)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"error":"denied"}`, http.StatusUnauthorized)
			return
		}
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/rpc/host/status":
			w.Write([]byte(`{}`))
		case "/v1/rpc/pair/begin":
			invite, err := pairer.Begin("wss://relay.example/ws")
			if err != nil {
				t.Error(err)
				return
			}
			qr, _ := invite.Encode()
			json.NewEncoder(w).Encode(map[string]any{"invite": invite, "qr": qr})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if err := writeConf(filepath.Join(home, "runtime.json"), &localConf{Port: port, Token: "secret", ServerID: "s"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := printInvite(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/v1/rpc/host/status" || paths[1] != "/v1/rpc/pair/begin" {
		t.Fatalf("calls = %v, want host.status then pair.begin", paths)
	}
	var line struct {
		Invite    string    `json:"invite"`
		HostID    device.ID `json:"host_id"`
		ExpiresAt int64     `json:"expires_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	invite, err := device.DecodeInvite(line.Invite)
	if err != nil {
		t.Fatal(err)
	}
	if line.HostID != store.Identity().ID || invite.InviterID != line.HostID || invite.ExpiresAt != line.ExpiresAt {
		t.Fatalf("line %+v does not match invite %+v", line, invite)
	}
	if invite.Address != "wss://relay.example/ws" {
		t.Fatalf("address = %q", invite.Address)
	}

	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	joiner := device.Peer{ID: phone.ID, Public: phone.Public, Label: "iPhone", Platform: "ios"}
	if _, err := pairer.Complete(joiner, device.Proof(invite.Code, phone.Public)); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := pairer.Complete(joiner, device.Proof(invite.Code, phone.Public)); err == nil {
		t.Fatal("the invite admitted a second device")
	}
}
