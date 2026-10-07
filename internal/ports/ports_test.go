package ports

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestListPortsFindsAListenerWithItsCwdAndPortlessName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no port enumeration on Windows")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	state := t.TempDir()
	t.Setenv("PORTLESS_STATE_DIR", state)
	routes := `[{"hostname":"web.localhost","port":` + strconv.Itoa(port) + `}]`
	if err := os.WriteFile(filepath.Join(state, "routes.json"), []byte(routes), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	find := func(dir string) *Port {
		for _, p := range ListPorts(context.Background(), dir) {
			if p.Port == port && p.PID == os.Getpid() {
				return &p
			}
		}
		return nil
	}
	got := find("")
	if got == nil {
		t.Skipf("port %d not enumerated (no lsof on this machine?)", port)
	}
	if got.Portless != "web.localhost" {
		t.Errorf("portless = %q, want web.localhost", got.Portless)
	}
	if got.Cwd != cwd {
		t.Errorf("cwd = %q, want %q", got.Cwd, cwd)
	}
	if find(cwd) == nil {
		t.Errorf("a dir query for the process's own cwd dropped it")
	}
	if find(filepath.Join(cwd, "elsewhere")) != nil {
		t.Errorf("a dir query for another directory kept it")
	}
}

func TestKillPortIgnoresAnImpossiblePort(t *testing.T) {
	got, _ := KillPort(context.Background(), 0, false)
	if len(got.PIDs) != 0 || got.Command != "" {
		t.Fatalf("KillPort(0) = %+v, want nothing", got)
	}
}

// The host's own listener is refused, whatever port it is on.
func TestKillPortRefusesTheHost(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	got, err := KillPort(context.Background(), port, false)
	if !errors.Is(err, ErrSelf) {
		t.Fatalf("KillPort(own %d) = %+v, %v; want ErrSelf", port, got, err)
	}
	if len(got.PIDs) != 0 {
		t.Fatalf("signalled %v", got.PIDs)
	}
}
