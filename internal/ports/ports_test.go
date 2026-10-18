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

func TestListPlatformPortsFindsAListenerWithItsCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no port enumeration on Windows")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	cwd, _ := os.Getwd()
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	for _, l := range listPlatformPorts(context.Background()) {
		if l.port == port && l.pid == os.Getpid() {
			if l.cwd != cwd {
				t.Errorf("cwd = %q, want %q", l.cwd, cwd)
			}
			return
		}
	}
	t.Skipf("port %d not enumerated (no lsof on this machine?)", port)
}

func TestPortsNamesPortlessFiltersByDirAndLeavesOutTheHost(t *testing.T) {
	dir := t.TempDir()
	listeners := []listener{
		{port: 5000, pid: 1, command: "node", cwd: filepath.Join(dir, "web")},
		{port: 3000, pid: 2, command: "vite", cwd: dir},
		{port: 4000, pid: 9, command: "repogo", cwd: dir},
		{port: 8080, pid: 3, command: "nginx", cwd: "/"},
	}
	routes := map[int]string{3000: "web.localhost"}

	all := ports(listeners, 9, routes, "")
	if len(all) != 3 || all[0].Port != 3000 || all[0].Portless != "web.localhost" || all[2].Port != 8080 {
		t.Fatalf("ports = %+v, want 3000 (named), 5000 and 8080 without the host's 4000", all)
	}
	mine := ports(listeners, 9, routes, dir)
	if len(mine) != 2 || mine[0].Port != 3000 || mine[1].Port != 5000 {
		t.Fatalf("ports in %s = %+v, want 3000 and 5000", dir, mine)
	}
}

func TestPortlessRoutesReadsTheStateDir(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PORTLESS_STATE_DIR", state)
	routes := `[{"hostname":"web.localhost","port":` + strconv.Itoa(3000) + `}]`
	if err := os.WriteFile(filepath.Join(state, "routes.json"), []byte(routes), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := portlessRoutes()[3000]; got != "web.localhost" {
		t.Fatalf("portlessRoutes()[3000] = %q, want web.localhost", got)
	}
}

func TestKillPortIgnoresAnImpossiblePort(t *testing.T) {
	if got, _ := KillPort(context.Background(), 0, false); got != 0 {
		t.Fatalf("KillPort(0) = %d, want nothing", got)
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
		t.Fatalf("KillPort(own %d) = %d, %v; want ErrSelf", port, got, err)
	}
	if got != 0 {
		t.Fatalf("signalled %d", got)
	}
}
