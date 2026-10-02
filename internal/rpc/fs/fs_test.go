package fs_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogo/host/internal/files"
	ghcore "github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/rpc"
	fsrpc "github.com/repogo/host/internal/rpc/fs"
)

type roots = files.StaticRoots

// register serves the fs family over svc.
func register(t *testing.T, svc *files.Service) *rpc.Router {
	t.Helper()
	router := rpc.New(slog.New(slog.DiscardHandler))
	fsrpc.Register(router, fsrpc.Deps{Files: svc, Watch: &recordWatch{}})
	return router
}

func TestFileOperationsRPC(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	router := register(t, files.New(files.Config{Roots: roots{root}}))
	call := func(method string, args map[string]any) json.RawMessage {
		t.Helper()
		params, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := router.Call(context.Background(), rpc.Caller{}, method, params)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return reply
	}
	reply := call("fs.search", map[string]any{"path": root, "query": "hello", "mode": "content", "limit": 5, "case_sensitive": true})
	var result struct {
		Results []struct {
			Path    string `json:"path"`
			Matches []struct {
				Line   int `json:"line"`
				Ranges []struct {
					Start int `json:"start_col"`
					End   int `json:"end_col"`
				} `json:"ranges"`
			} `json:"matches"`
		} `json:"results"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(reply, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || len(result.Results[0].Matches) != 1 || len(result.Results[0].Matches[0].Ranges) != 1 || result.Truncated || result.Results[0].Path != "file.txt" || result.Results[0].Matches[0].Ranges[0].End != 5 {
		t.Fatalf("reply: %s", reply)
	}
	if reply := call("fs.mkdir", map[string]any{"path": filepath.Join(root, "folder")}); string(reply) != `{"ok":true}` {
		t.Fatalf("mkdir: %s", reply)
	}
	if reply := call("fs.rename", map[string]any{"path": filepath.Join(root, "file.txt"), "new_path": filepath.Join(root, "folder", "new.txt")}); string(reply) != `{"ok":true}` {
		t.Fatalf("rename: %s", reply)
	}
	if data, err := os.ReadFile(filepath.Join(root, "folder", "new.txt")); err != nil || string(data) != "hello" {
		t.Fatalf("renamed file: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("rename source remains: %v", err)
	}
	if reply := call("fs.delete", map[string]any{"path": filepath.Join(root, "folder")}); string(reply) != `{"ok":true}` {
		t.Fatalf("delete: %s", reply)
	}
	if _, err := os.Stat(filepath.Join(root, "folder")); !os.IsNotExist(err) {
		t.Fatalf("deleted folder remains: %v", err)
	}
	// Without `create`, a write replaces the file as it always has.
	created := filepath.Join(root, "created.txt")
	call("fs.write", map[string]any{"path": created, "content": []byte("one"), "create": true})
	call("fs.write", map[string]any{"path": created, "content": []byte("two")})
	if data, err := os.ReadFile(created); err != nil || string(data) != "two" {
		t.Fatalf("created then written: %q, %v", data, err)
	}
	params, _ := json.Marshal(map[string]any{"path": root, "query": "hello", "mode": "invalid"})
	if _, err := router.Call(context.Background(), rpc.Caller{}, "fs.search", params); !errors.Is(err, rpc.ErrInvalidParams) {
		t.Fatalf("invalid mode: %v", err)
	}
}

// A host with no projects still gets one: the new folder lands in the projects
// directory, and a second of the same name or a path is refused.
func TestNewProjectRPC(t *testing.T) {
	dir := t.TempDir()
	svc := files.New(files.Config{Roots: ghcore.NewLayout(dir), Folders: ghcore.NewLayout(dir)})
	router := register(t, svc)
	// Paths come back resolved; on a Mac the temp dir is under a symlink.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string) (json.RawMessage, error) {
		params, _ := json.Marshal(map[string]any{"name": name})
		return router.Call(context.Background(), rpc.Caller{}, "fs.new_project", params)
	}
	reply, err := call("site")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(map[string]string{"path": filepath.Join(resolved, "site")})
	if string(reply) != string(want) {
		t.Fatalf("reply: %s, want %s", reply, want)
	}
	if projects, err := svc.Projects(); err != nil || len(projects) != 1 || projects[0].Path != filepath.Join(resolved, "site") {
		t.Fatalf("projects: %+v, %v", projects, err)
	}
	for _, name := range []string{"site", "", ".hidden", "..", "a/b", "../escape", "na me"} {
		if _, err := call(name); !errors.Is(err, rpc.ErrInvalidParams) {
			t.Errorf("%q: %v, want invalid", name, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("refused names left %d entries", len(entries))
	}
}

// Malformed params are the caller's mistake on every method, never a panic
// or an internal error.
func TestFilesystemRPCGuards(t *testing.T) {
	router := register(t, files.New(files.Config{Roots: roots{t.TempDir()}, Folders: ghcore.NewLayout(t.TempDir()), Picks: &picks{}}))
	for _, method := range router.Names() {
		t.Run(method, func(t *testing.T) {
			for _, p := range []string{`{`, `[]`, `{"path":1}`} {
				if p == `{"path":1}` && (method == "fs.watch" || method == "fs.stop") {
					p = `{"paths":1}` // no path field; a wrong-typed set is the same mistake
				}
				if p == `{"paths":1}` && method == "fs.stop" {
					continue // takes no params, so any object is one
				}
				if _, err := router.Call(context.Background(), rpc.Caller{}, method, json.RawMessage(p)); !errors.Is(err, rpc.ErrInvalidParams) {
					t.Fatalf("params %s: %v", p, err)
				}
			}
		})
	}
}

func TestFileOperationErrors(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	router := register(t, files.New(files.Config{Roots: roots{root}}))
	for _, tc := range []struct {
		name, method string
		params       map[string]any
		want         error
	}{
		{"search mode", "fs.search", map[string]any{"path": root, "query": "x", "mode": "invalid"}, rpc.ErrInvalidParams},
		{"search query", "fs.search", map[string]any{"path": root, "mode": "content"}, rpc.ErrInvalidParams},
		{"search limit", "fs.search", map[string]any{"path": root, "query": "x", "mode": "content", "limit": -1}, rpc.ErrInvalidParams},
		{"search outside", "fs.search", map[string]any{"path": outside, "query": "x", "mode": "content"}, rpc.ErrDenied},
		{"search missing", "fs.search", map[string]any{"path": missing, "query": "x", "mode": "content"}, rpc.ErrNotFound},
		{"delete outside", "fs.delete", map[string]any{"path": outside}, rpc.ErrDenied},
		{"delete missing", "fs.delete", map[string]any{"path": missing}, rpc.ErrNotFound},
		{"delete root", "fs.delete", map[string]any{"path": root}, rpc.ErrDenied},
		{"rename outside", "fs.rename", map[string]any{"path": file, "new_path": filepath.Join(outside, "new")}, rpc.ErrDenied},
		{"rename missing", "fs.rename", map[string]any{"path": missing, "new_path": filepath.Join(root, "new")}, rpc.ErrNotFound},
		{"rename overwrite", "fs.rename", map[string]any{"path": file, "new_path": file}, rpc.ErrInvalidParams},
		{"mkdir outside", "fs.mkdir", map[string]any{"path": filepath.Join(outside, "new")}, rpc.ErrDenied},
		{"mkdir missing parent", "fs.mkdir", map[string]any{"path": filepath.Join(missing, "new")}, rpc.ErrNotFound},
		{"read directory", "fs.read", map[string]any{"path": root}, rpc.ErrInvalidParams},
		{"create existing", "fs.write", map[string]any{"path": file, "content": []byte("new"), "create": true}, rpc.ErrInvalidParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatal(err)
			}
			reply := router.Dispatch(context.Background(), rpc.Caller{}, &jsonrpc.Message{JSONRPC: jsonrpc.Version, ID: json.RawMessage(`1`), Method: tc.method, Params: params})
			if reply == nil || reply.Error == nil || reply.Error.Code != rpc.Code(tc.want) || len(reply.Result) != 0 {
				t.Fatalf("reply: %+v; want error %v", reply, tc.want)
			}
		})
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Fatalf("file modified: %q, %v", data, err)
	}
}

type picks []string

func (p *picks) Pick(path string, _ time.Time) error { *p = append(*p, path); return nil }

// The folder picker: fs.list with dirs_only opens on home, fs.add_project makes
// the chosen folder a root, and fs.new_project with a parent makes and picks one.
func TestFolderPickerRPC(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(home, "Desktop", "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	picked := &picks{}
	router := register(t, files.New(files.Config{Roots: roots{}, Home: home, Folders: ghcore.NewLayout(t.TempDir()), Picks: picked}))
	call := func(method string, args map[string]any) (json.RawMessage, error) {
		params, _ := json.Marshal(args)
		return router.Call(context.Background(), rpc.Caller{}, method, params)
	}

	reply, err := call("fs.list", map[string]any{"path": "", "dirs_only": true})
	if err != nil {
		t.Fatal(err)
	}
	var listing fsrpc.ListResult
	if err := json.Unmarshal(reply, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != home || len(listing.Entries) != 1 || listing.Entries[0].Name != "Desktop" {
		t.Fatalf("home listing: %s", reply)
	}
	// Without dirs_only it is the env source picker: files too, still under home.
	if err := os.WriteFile(filepath.Join(app, ".env"), []byte("A=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	reply, err = call("fs.list", map[string]any{"path": app, "hidden": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reply, &listing); err != nil || len(listing.Entries) != 1 || listing.Entries[0].Name != ".env" {
		t.Fatalf("env picker listing: %s", reply)
	}
	if _, err := call("fs.list", map[string]any{"path": filepath.Dir(home), "hidden": true}); !errors.Is(err, rpc.ErrDenied) {
		t.Fatalf("fs.list outside home: %v, want denied", err)
	}

	if _, err := call("fs.add_project", map[string]any{"path": app}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("fs.add_project", map[string]any{"path": home}); !errors.Is(err, rpc.ErrDenied) {
		t.Fatalf("picking home: %v, want denied", err)
	}
	if _, err := call("fs.new_project", map[string]any{"name": "site", "parent": filepath.Join(home, "Desktop")}); err != nil {
		t.Fatal(err)
	}
	if want := []string{app, filepath.Join(home, "Desktop", "site")}; len(*picked) != 2 || (*picked)[0] != want[0] || (*picked)[1] != want[1] {
		t.Fatalf("picked %v, want %v", *picked, want)
	}
}

// The phone sends the mod_time it holds; the same file answers unchanged and
// empty, a newer one in full.
func TestReadRPCAnswersUnchanged(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "font.ttf")
	if err := os.WriteFile(path, []byte("glyphs"), 0o600); err != nil {
		t.Fatal(err)
	}
	router := register(t, files.New(files.Config{Roots: roots{root}}))
	type readReply struct {
		Content   []byte `json:"content"`
		ModTime   int64  `json:"mod_time"`
		Unchanged bool   `json:"unchanged"`
	}
	read := func(modTime int64) readReply {
		t.Helper()
		params, _ := json.Marshal(map[string]any{"path": path, "mod_time": modTime})
		raw, err := router.Call(context.Background(), rpc.Caller{}, "fs.read", params)
		if err != nil {
			t.Fatalf("fs.read: %v", err)
		}
		var reply readReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}

	first := read(0)
	if first.Unchanged || string(first.Content) != "glyphs" {
		t.Fatalf("first read = %+v, want the content", first)
	}
	if again := read(first.ModTime); !again.Unchanged || len(again.Content) != 0 {
		t.Errorf("read at the same mod_time = %+v, want unchanged without content", again)
	}
	if older := read(first.ModTime - 1000); older.Unchanged || string(older.Content) != "glyphs" {
		t.Errorf("read at an older mod_time = %+v, want the content", older)
	}
}
