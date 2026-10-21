package activities

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/stepactivity"
)

func router(t *testing.T) (*rpc.Router, device.ID) {
	t.Helper()
	devices, err := device.Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := rpc.New(log)
	Register(r, Deps{Activities: stepactivity.New(devices, log)})
	return r, devices.Identity().ID
}

// Only a plugin's ctx.host reaches it: a phone, a card or the CLI can't
// show an activity in a plugin's name.
func TestSetIsForPlugins(t *testing.T) {
	r, self := router(t)
	params, _ := json.Marshal(stepactivity.Request{ID: "a", Content: stepactivity.Content{Title: "Deploying"}})
	for name, c := range map[string]rpc.Caller{
		"phone": {Device: "phone", Scope: rpc.ScopeRemote},
		"local": {Device: self, Scope: rpc.ScopeLocal},
	} {
		if _, err := r.Call(t.Context(), c, "activities.set", params); err == nil {
			t.Errorf("%s caller allowed", name)
		}
	}
	plugin := rpc.Caller{Device: self, Scope: rpc.ScopeLocal, Plugin: "vercel"}
	if _, err := r.Call(t.Context(), plugin, "activities.set", params); err != nil {
		t.Fatalf("plugin: %v", err)
	}
	bad, _ := json.Marshal(stepactivity.Request{ID: "a"})
	if _, err := r.Call(t.Context(), plugin, "activities.set", bad); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("no title: %v", err)
	}
}
