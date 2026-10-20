package connect

import (
	"path/filepath"
	"regexp"
	"strings"
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
	Kind string // plugin, plugins, source or project
	Name string // the plugin, the source's handle, or the project's .env file
}

var (
	envName     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	pluginName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	projectFile = regexp.MustCompile(`^\.env[A-Za-z0-9._-]*$`)
	cliKind     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
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
	case kind == "project" && projectFile.MatchString(name):
	default:
		return scope{}, ErrInvalid.Errorf("scope must be plugin:<name>, plugins, source:<handle> or project:<.env file>")
	}
	return scope{Kind: kind, Name: name}, nil
}

// envFile is a scope resolved on this machine: the file, and how the phone
// tells the user where their value goes.
type envFile struct {
	Path        string
	Destination string
}

// resolve finds a scope's file here; nothing a caller sends is used as a path.
func (s *Service) resolve(project string, sc scope) (envFile, error) {
	switch sc.Kind {
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
	case "project":
		root, err := s.d.Contain(project)
		if err != nil {
			return envFile{}, err
		}
		return envFile{Path: filepath.Join(root, sc.Name), Destination: sc.Name + " in " + filepath.Base(root)}, nil
	}
	return envFile{}, ErrInvalid.Errorf("unknown scope %q", sc.Kind)
}
