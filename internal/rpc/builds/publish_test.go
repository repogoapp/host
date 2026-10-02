package builds_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/builds"
	"github.com/repogo/host/internal/host"
	"github.com/repogo/host/internal/rpc"
	buildsrpc "github.com/repogo/host/internal/rpc/builds"
	"github.com/repogo/host/internal/rpc/registry"
	"github.com/repogo/host/internal/testhost"
)

func TestPublicationRPCReadsAndReplaysPersistedAttempt(t *testing.T) {
	var state string
	h := testhost.New(t, func(cfg *host.Config) { state = cfg.State })
	router, err := registry.New(h.Config)
	if err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(h.Root)
	if err != nil {
		t.Fatal(err)
	}
	p := builds.Publication{ID: strings.Repeat("1", 32), Project: project, Path: "Demo.xcodeproj", Target: "Demo",
		BuildID: strings.Repeat("2", 32), BundleID: "com.example.demo", Version: "1.0.0", BuildNumber: "7",
		Status: builds.StatusUploaded, CreatedAt: 1, FinishedAt: 2}
	dir := filepath.Join(state, "builds", "publications", p.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := apphome.WriteJSON(filepath.Join(dir, "publish.json"), p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "publish.log"), []byte("uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	call := func(method string, params any, result any) {
		t.Helper()
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := router.Call(t.Context(), rpc.Caller{Scope: rpc.ScopeLocal}, method, raw)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if err := json.Unmarshal(reply, result); err != nil {
			t.Fatal(err)
		}
	}
	var got buildsrpc.PublicationResult
	call("builds.publish_status", buildsrpc.PublicationParams{PublicationID: p.ID}, &got)
	if got.Publication != p {
		t.Fatalf("status = %+v", got)
	}
	call("builds.publish", buildsrpc.PublishParams{RequestID: p.ID, Project: project, Path: p.Path, Target: p.Target,
		Version: p.Version, BuildNumber: p.BuildNumber}, &got)
	if got.Publication != p {
		t.Fatalf("replay = %+v", got)
	}
	var list buildsrpc.PublicationsResult
	call("builds.publish_list", buildsrpc.AppParams{Project: project, Path: p.Path, Target: p.Target}, &list)
	if len(list.Publications) != 1 || list.Publications[0] != p {
		t.Fatalf("list = %+v", list)
	}
	var log builds.LogChunk
	call("builds.publish_log", buildsrpc.PublishLogParams{PublicationID: p.ID, Offset: 2}, &log)
	if string(log.Data) != "loaded" || log.NextOffset != 8 || !log.Done {
		t.Fatalf("log = %+v", log)
	}
	raw, _ := json.Marshal(buildsrpc.PublicationParams{PublicationID: p.ID})
	if _, err := router.Call(t.Context(), rpc.Caller{Scope: rpc.ScopeLocal}, "builds.publish_cancel", raw); !errors.Is(err, rpc.ErrNotFound) {
		t.Fatalf("cancel finished attempt = %v", err)
	}
}
