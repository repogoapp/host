package git

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitcore "github.com/repogo/host/internal/git"
	"github.com/repogo/host/internal/rpc"
)

// allowAny opens containment up for a temp repository.
type allowAny struct{}

func (allowAny) Contain(path string) (string, error) { return path, nil }

func TestMutationWireArgumentsAndErrors(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	r := rpc.New(slog.New(slog.DiscardHandler))
	Register(r, Deps{Git: gitcore.New(allowAny{}, slog.New(slog.DiscardHandler))})
	call := func(method string, args any) error {
		b, _ := json.Marshal(args)
		_, err := r.Call(context.Background(), rpc.Caller{}, method, b)
		return err
	}

	if err := call("git.create_branch", map[string]any{"path": dir, "name": "feature", "from_ref": "HEAD"}); err != nil {
		t.Fatal(err)
	}
	if err := call("git.switch_branch", map[string]any{"path": dir, "branch": "main"}); err != nil {
		t.Fatal(err)
	}
	if err := call("git.create_branch", map[string]any{"path": dir, "name": "--orphan"}); !errors.Is(err, rpc.ErrInvalidParams) {
		t.Fatalf("expected invalid params: %v", err)
	}
	for _, method := range []string{"git.pull", "git.reset_hard", "git.switch_branch", "git.create_branch", "git.commit_push"} {
		if _, err := r.Call(context.Background(), rpc.Caller{}, method, json.RawMessage(`{"path":false}`)); !errors.Is(err, rpc.ErrInvalidParams) {
			t.Fatalf("%s bad payload: %v", method, err)
		}
	}
}

func TestCommitPushResultCarriesShipped(t *testing.T) {
	dir, bare := t.TempDir(), t.TempDir()
	for _, args := range [][]string{
		{"-C", bare, "init", "-q", "--bare", "-b", "main"},
		{"-C", dir, "init", "-q", "-b", "main"},
		{"-C", dir, "config", "user.email", "test@example.com"},
		{"-C", dir, "config", "user.name", "Test"},
		{"-C", dir, "remote", "add", "origin", bare},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := rpc.New(slog.New(slog.DiscardHandler))
	Register(r, Deps{Git: gitcore.New(allowAny{}, slog.New(slog.DiscardHandler))})
	result, err := r.Call(context.Background(), rpc.Caller{}, "git.commit_push", json.RawMessage(`{"path":"`+dir+`","message":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	var wire struct {
		Shipped struct {
			EventID string `json:"event_id"`
			Branch  string `json:"branch"`
			Commits []struct {
				SHA         string `json:"sha"`
				LinesAdded  int64  `json:"lines_added"`
				FileChanges int64  `json:"file_changes"`
			} `json:"commits"`
		} `json:"shipped"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	s := wire.Shipped
	if s.EventID == "" || s.Branch != "main" || len(s.Commits) != 1 || s.Commits[0].LinesAdded != 1 || s.Commits[0].FileChanges != 1 {
		t.Fatalf("shipped: %s", b)
	}
	if strings.Contains(string(b), `"first"`) {
		t.Fatalf("commit message on the wire: %s", b)
	}
}
