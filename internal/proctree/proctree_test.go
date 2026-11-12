package proctree

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// Killing the head of a tree ends a child it started, which would otherwise
// outlive it.
func TestKillEndsTheWholeTree(t *testing.T) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 > NUL")
	} else {
		cmd = exec.Command("sh", "-c", "sleep 30 & wait")
	}
	Own(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err := Kill(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tree is still running")
	}
}
