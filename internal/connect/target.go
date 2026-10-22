package connect

import (
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/repogoapp/host/internal/files"
)

// The kinds of target: a key in a dotenv file, an MCP server, a CLI's sign-in.
const (
	KindEnv = "env"
	KindMCP = "mcp"
	KindCLI = "cli"
)

// target is what a caller names, read once: `env:NAME` with a scope,
// `mcp:<url>` or `cli:<kind>`.
type target struct {
	Kind string
	Env  envKey // for env
	URL  string // for mcp
	CLI  string // for cli
}

// envKey is an `env:` target: a key and the file it lives in.
type envKey struct {
	Name  string
	Scope scope
}

// scope is which dotenv file a key lives in, as a card names it.
type scope struct {
	Kind string // file, plugin, plugins or source
	Name string // the path, the plugin, or the source's handle
}

var (
	envName    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	pluginName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	cliKind    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

func parseTarget(raw, rawScope string) (target, error) {
	kind, rest, _ := strings.Cut(raw, ":")
	switch kind {
	case KindEnv:
		if !envName.MatchString(rest) {
			return target{}, ErrInvalid.Errorf("%q is not an env key name: capitals, digits and _", rest)
		}
		sc, err := parseScope(rawScope)
		if err != nil {
			return target{}, err
		}
		return target{Kind: kind, Env: envKey{Name: rest, Scope: sc}}, nil
	case KindMCP:
		if strings.TrimSpace(rest) == "" {
			return target{}, ErrInvalid.Errorf("an mcp: target needs the server's URL")
		}
		return target{Kind: kind, URL: serverURL(rest)}, nil
	case KindCLI:
		if !cliKind.MatchString(rest) {
			return target{}, ErrInvalid.Errorf("%q is not a CLI", rest)
		}
		return target{Kind: kind, CLI: rest}, nil
	}
	return target{}, ErrInvalid.Errorf("a target is env:<NAME>, mcp:<url> or cli:<tool>")
}

// serverURL is a pasted address as the MCP service stores it: a bare host
// gets https, so a card's URL matches a connected server's.
func serverURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	before, _, _ := strings.Cut(raw, "#")
	return before
}

func parseScope(raw string) (scope, error) {
	kind, name, _ := strings.Cut(raw, ":")
	switch {
	case kind == "plugins" && name == "":
		return scope{Kind: kind}, nil
	case kind == "plugin" && pluginName.MatchString(name):
	case kind == "source" && name != "":
	case kind == "file" && strings.TrimSpace(name) != "" && !strings.ContainsRune(name, 0):
	default:
		return scope{}, ErrInvalid.Errorf("scope must be file:<path>, plugin:<name>, plugins or source:<handle>")
	}
	return scope{Kind: kind, Name: name}, nil
}

// envFile is a scope resolved on this machine: the file, and how the phone
// tells the user where their value goes.
type envFile struct {
	Path        string
	Destination string
}

// resolve finds a scope's file here. A `file:` path is the caller's choice,
// so the sheet shows it whole; every other scope is the host's to find.
func (s *Service) resolve(project string, sc scope) (envFile, error) {
	switch sc.Kind {
	case "file":
		path, err := s.filePath(project, sc.Name)
		if err != nil {
			return envFile{}, err
		}
		return envFile{Path: path, Destination: s.tilde(path)}, nil
	case "plugin":
		dir, err := s.d.PluginDir(project, sc.Name)
		if err != nil {
			return envFile{}, err
		}
		return envFile{Path: filepath.Join(dir, ".env"), Destination: "The " + sc.Name + " plugin's .env"}, nil
	case "plugins":
		return envFile{Path: s.d.SharedEnv, Destination: "The .env every plugin reads"}, nil
	case "source":
		for _, src := range s.d.Sources.List() {
			if src.Handle == sc.Name {
				return envFile{Path: src.Path, Destination: "The env source " + sc.Name}, nil
			}
		}
		return envFile{}, ErrNotFound.Errorf("no env source %q", sc.Name)
	}
	return envFile{}, ErrInvalid.Errorf("unknown scope %q", sc.Kind)
}

// filePath is a `file:` path on this machine, its folders' links resolved:
// relative to the project and staying inside it, or `~/…` and absolute paths
// staying under the home folder.
func (s *Service) filePath(project, name string) (string, error) {
	var path, within string
	switch {
	case strings.HasPrefix(name, "~/"):
		path = filepath.Join(s.d.Home, name[2:])
	case filepath.IsAbs(name):
		path = filepath.Clean(name)
	default:
		root, err := s.d.Contain(project)
		if err != nil {
			return "", err
		}
		if within, err = resolveExisting(root); err != nil {
			return "", err
		}
		path = filepath.Join(root, name)
		if path == root {
			return "", errRefused.Errorf("%s is the project, not a file in it", name)
		}
	}
	dir, err := resolveExisting(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	switch {
	case within != "" && !files.Within(within, dir):
		return "", errRefused.Errorf("%s is outside the project", name)
	case !files.Within(s.home(), dir):
		return "", errRefused.Errorf("%s is outside your home folder", name)
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

// tilde writes a path under the home folder as ~/…, as people read it.
func (s *Service) tilde(path string) string {
	home := s.home()
	if rel, err := filepath.Rel(home, path); err == nil && files.Within(home, path) {
		return "~/" + rel
	}
	return path
}

// resolveExisting resolves the links of dir's nearest folder that exists,
// keeping the rest, which writing will create. Only a missing folder is
// stepped over; any other failure is refused.
func resolveExisting(dir string) (string, error) {
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		switch {
		case err == nil:
			return filepath.Join(resolved, rest), nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", errRefused.Errorf("can't read %s", filepath.Base(dir))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(dir, rest), nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// home is the home folder with its links resolved, as resolved paths are compared to it.
func (s *Service) home() string {
	if resolved, err := filepath.EvalSymlinks(s.d.Home); err == nil {
		return resolved
	}
	return s.d.Home
}
