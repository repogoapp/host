package agents_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Services depend on contracts; only composition roots may import providers.
func TestServicePackagesDoNotImportProviders(t *testing.T) {
	for _, pkg := range []string{"agent", "agentcatalog", "agentusage", "session", "notify", "mcp", "shipping", "liveactivity", "chatwire", "toollabel"} {
		err := filepath.WalkDir(filepath.Join("..", pkg), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				imp, _ := strconv.Unquote(spec.Path.Value)
				if imp == "github.com/repogo/host/internal/agents" || strings.HasPrefix(imp, "github.com/repogo/host/internal/agents/") {
					t.Errorf("%s imports provider package %s", path, imp)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// kindComparisons lets a shared file compare against an agent's name, keyed by
// slash path relative to the module root; every entry says why.
var kindComparisons = map[string]string{}

func isAgentName(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, _ := strconv.Unquote(lit.Value)
	return lit.Value, v == "claude" || v == "codex" || v == "cursor"
}

// Wire fields may name an agent; branching on one may not. Outside the agent
// folders nothing switches on, compares with, or names an agent's kind, so a
// new agent never means finding every `case "claude"`.
func TestSharedCodeDoesNotBranchOnAgentKind(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(path)
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "build", "node_modules", "npm":
				return filepath.SkipDir
			}
			if strings.HasSuffix(slash, "internal/agents") {
				return filepath.SkipDir
			}
			return nil
		}
		// kinds.go declares the constants; tests may name agents freely.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(slash, "internal/agent/kinds.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if n.Sel.Name == "KindClaude" || n.Sel.Name == "KindCodex" || n.Sel.Name == "KindCursor" {
					t.Errorf("%s: %s outside its agent folder", fset.Position(n.Pos()), n.Sel.Name)
				}
			case *ast.CaseClause:
				for _, e := range n.List {
					if lit, ok := isAgentName(e); ok {
						t.Errorf("%s: case %s branches on an agent kind", fset.Position(e.Pos()), lit)
					}
				}
			case *ast.BinaryExpr:
				if n.Op != token.EQL && n.Op != token.NEQ {
					return true
				}
				for _, e := range []ast.Expr{n.X, n.Y} {
					if lit, ok := isAgentName(e); ok && kindComparisons[strings.TrimPrefix(slash, "../../")] == "" {
						t.Errorf("%s: comparison with %s branches on an agent kind", fset.Position(e.Pos()), lit)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
