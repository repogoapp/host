package host_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const internalPath = "github.com/repogoapp/host/internal/"

// internalImports is each package under internal, by its path there, with the
// internal packages its shipped files import.
func internalImports(t *testing.T) map[string][]string {
	t.Helper()
	root := ".."
	out := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		pkg := filepath.ToSlash(rel)
		for _, spec := range file.Imports {
			imp, _ := strconv.Unquote(spec.Path.Value)
			if dep, ok := strings.CutPrefix(imp, internalPath); ok && !slices.Contains(out[pkg], dep) {
				out[pkg] = append(out[pkg], dep)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Wire structs and shared plumbing sit under everything: they import only each
// other, so any package may use one without pulling in a service.
func TestLeafPackagesImportOnlyLeaves(t *testing.T) {
	leaves := []string{
		"jsonrpc", "handshake", "wirecodec", "wirejson", "wiredoc",
		"errkind", "outcome", "par", "sqlitedb", "syscmd", "apphome", "devlog", "redial",
	}
	imports := internalImports(t)
	for _, pkg := range leaves {
		for _, dep := range imports[pkg] {
			if !slices.Contains(leaves, dep) {
				t.Errorf("%s imports %s; a leaf imports only leaves", pkg, dep)
			}
		}
	}
}

// One assembly: only it reaches for the transports, the wiring table and the
// providers, so no service can build a second host around itself.
func TestOnlyTheAssemblyImportsTheEdges(t *testing.T) {
	roots := []string{"host", "testhost"}
	edges := []string{"host", "rpc/registry", "wsserver", "passthrough", "api", "agents"}
	for pkg, deps := range internalImports(t) {
		if slices.Contains(roots, pkg) {
			continue
		}
		for _, dep := range deps {
			if slices.Contains(edges, dep) {
				t.Errorf("%s imports %s; only %v may", pkg, dep, roots)
			}
		}
	}
}

// rpcCallers is who may import the router besides its own families; every
// entry says why.
var rpcCallers = map[string]string{
	"host":           "builds the router",
	"testhost":       "builds the router",
	"wsserver":       "a transport: devices call methods over a socket",
	"api":            "a transport: loopback HTTP calls methods",
	"repogomcp":      "a transport: an agent calls methods as MCP tools",
	"catalog":        "lists the methods a project offers to call",
	"clientcontract": "writes the clients' types from the methods",
}

// Methods are thin over services, never the other way: a service that needs
// the router has put its logic on the wrong side of it.
func TestServicesDoNotImportTheRouter(t *testing.T) {
	for pkg, deps := range internalImports(t) {
		if pkg == "rpc" || strings.HasPrefix(pkg, "rpc/") || rpcCallers[pkg] != "" {
			continue
		}
		for _, dep := range deps {
			if dep == "rpc" || strings.HasPrefix(dep, "rpc/") {
				t.Errorf("%s imports %s; a service does not import the router", pkg, dep)
			}
		}
	}
}
