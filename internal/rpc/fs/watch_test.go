package fs_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/rpc"
	fsrpc "github.com/repogo/host/internal/rpc/fs"
)

type recordWatch struct {
	paths, active []string
	resync        bool
	stopped       device.ID
}

func (w *recordWatch) Watch(_ device.ID, paths, active []string, resync bool) error {
	w.paths, w.active, w.resync = paths, active, resync
	return nil
}

func (w *recordWatch) Stop(caller device.ID) { w.stopped = caller }

func watchRouter(w *recordWatch) *rpc.Router {
	router := rpc.New(slog.New(slog.DiscardHandler))
	fsrpc.Register(router, fsrpc.Deps{Watch: w})
	return router
}

// fs.watch hands the device's set through whole; resync is optional.
func TestWatchPassesTheSetThrough(t *testing.T) {
	for _, resync := range []bool{false, true} {
		w := &recordWatch{}
		params := map[string]any{"paths": []string{"/var/proj"}, "active": []string{"/var/proj"}}
		if resync {
			params["resync"] = true
		}
		b, _ := json.Marshal(params)
		if _, err := watchRouter(w).Call(context.Background(), rpc.Caller{Device: "a"}, "fs.watch", b); err != nil {
			t.Fatal(err)
		}
		if want := []string{"/var/proj"}; !slices.Equal(w.paths, want) || !slices.Equal(w.active, want) || w.resync != resync {
			t.Errorf("resync %v: got %+v", resync, w)
		}
	}
}

func TestStopDropsTheCaller(t *testing.T) {
	w := &recordWatch{}
	if _, err := watchRouter(w).Call(context.Background(), rpc.Caller{Device: "a"}, "fs.stop", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if w.stopped != "a" {
		t.Errorf("stopped %q, want a", w.stopped)
	}
}
