// Package lint turns the code standards in AGENTS.md into test
// failures, so review is not the only thing enforcing them.
package lint_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sleeps lets a test call time.Sleep, keyed "path:Func" relative to the module root;
// every entry says why no hook or testwait.For can replace it.
var sleeps = map[string]string{}

// backgrounds lets non-main code start a context that no caller owns, keyed
// "path:Func"; every entry says why the work outlives the request.
var backgrounds = map[string]string{
	"internal/power/power.go:Keeper.Release":                   "runs at shutdown, after the host's context has ended",
	"internal/agent/manager.go:Manager.newTurn":                "a turn outlives the call that queued it; Stop cancels it",
	"internal/clilogin/clilogin.go:launch":                     "a sign-in CLI outlives the call that started it; its timeout ends it",
	"internal/wsserver/conn.go:conn.Send":                      "a push belongs to no request; wsconn bounds the write",
	"internal/projectwatch/projectwatch.go:Manager.joinLocked": "a watch room lives until its last subscriber leaves and its linger ends",
	"internal/hostclient/hostclient.go:Client.read":            "the Go test client's read loop; Close ends the socket",
	"internal/hostclient/hostclient.go:Client.readRecord":      "the Go test client's read loop; Close ends the socket",
}

// history catches comments that narrate what the code once was. Words such as
// "no longer" or "was removed" also describe runtime state, so only phrases
// that name the code's past or a compatibility branch are matched.
var history = regexp.MustCompile(`(?i)\b(previously|formerly|legacy|deprecated|backwards?[- ]compat\w*|wire[- ]compat\w*|compat (?:layer|shim|path|field)s?|for now|used to be|we used to|no longer (?:needed|used|supported|exists?|called|required)|(?:was|were) renamed|renamed from|old (?:shape|layout|format|field|api|clients?|hosts?|phones?|apps?))\b`)

type file struct {
	rel  string // slash path relative to the module root
	fset *token.FileSet
	ast  *ast.File
	test bool
}

func (f file) at(p token.Pos) string {
	return fmt.Sprintf("%s:%d", f.rel, f.fset.Position(p).Line)
}

// hostFiles parses every hand-written Go file in the module.
func hostFiles(t *testing.T) []file {
	t.Helper()
	root := filepath.Join("..", "..")
	var files []file
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "build", "node_modules", "npm", "target":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if ast.IsGenerated(f) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		files = append(files, file{rel: rel, fset: fset, ast: f, test: strings.HasSuffix(path, "_test.go")})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 100 {
		t.Fatalf("found %d Go files; the walk is not at the module root", len(files))
	}
	return files
}

// importName is the local name of path in f, or "" when f does not import it.
func importName(f *ast.File, path string) string {
	for _, spec := range f.Imports {
		if p, _ := strconv.Unquote(spec.Path.Value); p == path {
			if spec.Name != nil {
				return spec.Name.Name
			}
			return path[strings.LastIndex(path, "/")+1:]
		}
	}
	return ""
}

// calls visits every call of pkg.Name in f with the enclosing func's key.
func calls(f file, pkg string, visit func(call *ast.CallExpr, name, fn string)) {
	local := importName(f.ast, pkg)
	if local == "" || local == "_" {
		return
	}
	for _, decl := range f.ast.Decls {
		fn := ""
		if d, ok := decl.(*ast.FuncDecl); ok {
			fn = d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				fn = recvName(d.Recv.List[0].Type) + "." + fn
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
					visit(call, sel.Sel.Name, fn)
				}
			}
			return true
		})
	}
}

func recvName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return recvName(e.X)
	case *ast.IndexExpr:
		return recvName(e.X)
	case *ast.IndexListExpr:
		return recvName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// Package docs, build constraints and license headers sit above the package
// clause and are exempt: a package's overview is where its role is written.
func TestCommentsAreShort(t *testing.T) {
	for _, f := range hostFiles(t) {
		exempt := map[*ast.CommentGroup]bool{}
		for _, decl := range f.ast.Decls {
			if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.IMPORT && g.Doc != nil {
				for _, spec := range g.Specs {
					if p, _ := strconv.Unquote(spec.(*ast.ImportSpec).Path.Value); p == "C" {
						exempt[g.Doc] = true // the cgo preamble is C, not a comment
					}
				}
			}
		}
		for _, g := range f.ast.Comments {
			if g.End() < f.ast.Package || exempt[g] {
				continue
			}
			if n := commentLines(g); n > 3 {
				t.Errorf("%s: %d-line comment; keep it to three lines", f.at(g.Pos()), n)
			}
		}
	}
}

func commentLines(g *ast.CommentGroup) int {
	n := 0
	for _, c := range g.List {
		if isDirective(c.Text) {
			continue
		}
		n += strings.Count(strings.TrimRight(c.Text, "\n"), "\n") + 1
	}
	return n
}

func isDirective(text string) bool {
	for _, p := range []string{"//go:", "//line ", "//export ", "//nolint", "// +build", "//lint:"} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

func TestCommentsGiveReasonsNotHistory(t *testing.T) {
	for _, f := range hostFiles(t) {
		for _, g := range f.ast.Comments {
			for _, c := range g.List {
				if m := history.FindString(c.Text); m != "" {
					t.Errorf("%s: %q narrates history; say why the code is as it is", f.at(c.Pos()), m)
				}
			}
		}
	}
}

// Tests wait on a hook, a channel or testwait.For, never on the clock.
func TestTestsDoNotSleep(t *testing.T) {
	used := map[string]bool{}
	for _, f := range hostFiles(t) {
		if !f.test {
			continue
		}
		calls(f, "time", func(call *ast.CallExpr, name, fn string) {
			if name != "Sleep" {
				return
			}
			key := f.rel + ":" + fn
			if sleeps[key] != "" {
				used[key] = true
				return
			}
			t.Errorf("%s: time.Sleep in a test; wait on a hook, a channel or testwait.For", f.at(call.Pos()))
		})
	}
	stale(t, sleeps, used)
}

// A context no caller owns is only for main and tests; anything else that
// outlives its request is listed in backgrounds.
func TestContextBackgroundIsWiring(t *testing.T) {
	used := map[string]bool{}
	for _, f := range hostFiles(t) {
		if f.test || f.ast.Name.Name == "main" || testSupport(f.rel) {
			continue
		}
		calls(f, "context", func(call *ast.CallExpr, name, fn string) {
			if name != "Background" && name != "TODO" {
				return
			}
			key := f.rel + ":" + fn
			if backgrounds[key] != "" {
				used[key] = true
				return
			}
			t.Errorf("%s: context.%s outside main; take the caller's context, or add %q to backgrounds with why it outlives the request", f.at(call.Pos()), name, key)
		})
	}
	stale(t, backgrounds, used)
}

// testSupport packages exist only for tests to import.
func testSupport(rel string) bool {
	return strings.HasPrefix(rel, "internal/testhost/") || strings.HasPrefix(rel, "internal/testwait/")
}

// Services log through the *slog.Logger they are given; only main sets the
// default.
func TestNoGlobalLogger(t *testing.T) {
	global := map[string]bool{
		"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "LogAttrs": true,
		"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
		"Default": true, "SetDefault": true, "With": true,
	}
	for _, f := range hostFiles(t) {
		if f.test || f.ast.Name.Name == "main" {
			continue
		}
		calls(f, "log/slog", func(call *ast.CallExpr, name, _ string) {
			if global[name] {
				t.Errorf("%s: slog.%s uses the global logger; take a *slog.Logger", f.at(call.Pos()), name)
			}
		})
	}
}

// internal/git's runner is the one place git is executed (AGENTS.md roles).
func TestOneGitRunner(t *testing.T) {
	for _, f := range hostFiles(t) {
		if f.test || strings.HasPrefix(f.rel, "internal/git/") {
			continue
		}
		calls(f, "os/exec", func(call *ast.CallExpr, name, _ string) {
			if name != "Command" && name != "CommandContext" {
				return
			}
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.BasicLit); ok && lit.Value == `"git"` {
					t.Errorf("%s: exec of git outside internal/git; use its runner", f.at(call.Pos()))
				}
			}
		})
	}
}

// agents/architecture_test.go catches case and ==/!= on an agent's name; this
// catches the same comparison made through the strings package.
func TestSharedCodeDoesNotMatchAgentNamesWithStrings(t *testing.T) {
	match := map[string]bool{"EqualFold": true, "HasPrefix": true, "HasSuffix": true, "Contains": true, "Compare": true, "Index": true, "TrimPrefix": true, "TrimSuffix": true, "CutPrefix": true, "CutSuffix": true}
	for _, f := range hostFiles(t) {
		if f.test || strings.HasPrefix(f.rel, "internal/agents/") {
			continue
		}
		calls(f, "strings", func(call *ast.CallExpr, name, _ string) {
			if !match[name] {
				return
			}
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.BasicLit); ok && (lit.Value == `"claude"` || lit.Value == `"codex"`) {
					t.Errorf("%s: strings.%s with %s branches on an agent kind", f.at(call.Pos()), name, lit.Value)
				}
			}
		})
	}
}

// stale fails on allowlist entries whose exception is gone, so lists only shrink.
func stale(t *testing.T, list map[string]string, used map[string]bool) {
	t.Helper()
	for key := range list {
		if !used[key] {
			t.Errorf("allowlist entry %q no longer matches anything; delete it", key)
		}
	}
}
