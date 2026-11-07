package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/apphome"
)

func TestProjectsDirKeepsTheDisksSpelling(t *testing.T) {
	home := t.TempDir()
	if got, want := projectsDir(home), filepath.Join(home, "RepoGo"); got != want {
		t.Fatalf("no folder yet: got %q, want %q", got, want)
	}
	if err := os.Mkdir(filepath.Join(home, "repogo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := projectsDir(home), filepath.Join(home, "repogo"); got != want {
		t.Fatalf("lowercase folder: got %q, want %q", got, want)
	}
}

// -lan serves the local network for one run and leaves the saved setting
// alone: the conf is written back at listen, so one run would otherwise stick.
func TestTheLANFlagLastsOneRun(t *testing.T) {
	t.Setenv(apphome.EnvVar, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	conf := &localConf{Token: "t", ServerID: "s"}
	cfg, err := hostConfig(slog.New(slog.NewTextHandler(io.Discard, nil)), conf, true, "", restarter{})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LAN {
		t.Fatal("-lan did not serve the local network")
	}
	if conf.LAN {
		t.Fatal("-lan reached the saved conf")
	}
}

// --tailscale=false keeps the host off its tailnet across restarts, and
// saving one setting leaves the other as it was.
func TestTailscaleCanBeTurnedOff(t *testing.T) {
	t.Setenv(apphome.EnvVar, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	on, off := true, false
	if changed, err := saveDirect(&on, nil); err != nil || !changed {
		t.Fatalf("--lan: changed %t, %v", changed, err)
	}
	if changed, err := saveDirect(nil, &off); err != nil || !changed {
		t.Fatalf("--tailscale=false: changed %t, %v", changed, err)
	}
	if changed, _ := saveDirect(nil, &off); changed {
		t.Fatal("saving the same setting again reported a change")
	}
	path, _ := confPath()
	conf, err := loadOrCreateConf(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := hostConfig(slog.New(slog.NewTextHandler(io.Discard, nil)), conf, false, "", restarter{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tailnet || !cfg.LAN {
		t.Fatalf("tailnet %t, lan %t; want off, on", cfg.Tailnet, cfg.LAN)
	}
}
