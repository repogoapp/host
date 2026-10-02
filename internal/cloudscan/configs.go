package cloudscan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// readFile reads a config through the scoped root, refusing anything over
// 1 MiB: these files are a few lines, and a scan must not stall on one.
func readFile(scoped *os.Root, relative string) ([]byte, error) {
	file, err := scoped.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maxSize = 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSize {
		return nil, fmt.Errorf("config file exceeds 1 MiB")
	}
	return data, nil
}

// readVercelLink reads .vercel/project.json, which links its parent folder,
// or .vercel/repo.json, which links several folders of one repository.
func readVercelLink(scoped *os.Root, root, relative string) ([]Project, error) {
	source := filepath.Join(root, relative)
	data, err := readFile(scoped, relative)
	if err != nil {
		return nil, err
	}
	var raw struct {
		OrgID     string `json:"orgId"`
		ProjectID string `json:"projectId"`
		Name      string `json:"projectName"`
		Projects  []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Directory string `json:"directory"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.OrgID == "" {
		return nil, fmt.Errorf("missing orgId")
	}
	base := filepath.Dir(filepath.Dir(source))
	if filepath.Base(source) == "project.json" {
		if raw.ProjectID == "" {
			return nil, fmt.Errorf("missing projectId")
		}
		return []Project{{Provider: ProviderVercel, Name: raw.Name, ID: raw.ProjectID, TeamID: raw.OrgID, Directory: base, ConfigPath: source}}, nil
	}
	projects := []Project{}
	for _, project := range raw.Projects {
		if project.ID == "" || project.Directory == "" {
			return nil, fmt.Errorf("repo mapping requires id and directory")
		}
		if filepath.IsAbs(project.Directory) {
			return nil, fmt.Errorf("repo directory must be relative")
		}
		directory := filepath.Join(base, project.Directory)
		// Compared resolved, since the folder itself may sit behind a symlink
		// (macOS's /var is /private/var).
		resolved, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return nil, err
		}
		realBase, err := filepath.EvalSymlinks(base)
		if err != nil {
			return nil, err
		}
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(realBase, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("repo directory escapes link root")
		}
		mapped, err := filepath.Rel(realRoot, resolved)
		if err != nil {
			return nil, err
		}
		info, err := scoped.Stat(mapped)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("repo mapping is not an accessible directory")
		}
		projects = append(projects, Project{Provider: ProviderVercel, Name: project.Name, ID: project.ID, TeamID: raw.OrgID, Directory: directory, ConfigPath: source})
	}
	return projects, nil
}

// readFly reads the app fly.toml names. `fly launch` writes it; a file
// without one isn't linked to an app yet.
func readFly(scoped *os.Root, relative string, project *Project) error {
	data, err := readFile(scoped, relative)
	if err != nil {
		return err
	}
	keys, err := tomlTopLevel(data)
	if err != nil {
		return err
	}
	project.Name = keys["app"]
	project.ID = keys["app"]
	return nil
}

// readWrangler reads a Worker's name and, when the file pins one, its
// account, from wrangler.toml, wrangler.json or wrangler.jsonc.
func readWrangler(scoped *os.Root, relative string, project *Project) error {
	data, err := readFile(scoped, relative)
	if err != nil {
		return err
	}
	var name, account string
	if filepath.Ext(relative) == ".toml" {
		keys, err := tomlTopLevel(data)
		if err != nil {
			return err
		}
		name, account = keys["name"], keys["account_id"]
	} else {
		var raw struct {
			Name      string `json:"name"`
			AccountID string `json:"account_id"`
		}
		if err := json.Unmarshal(stripJSONC(data), &raw); err != nil {
			return err
		}
		name, account = raw.Name, raw.AccountID
	}
	project.Name = name
	project.ID = name
	project.TeamID = account
	return nil
}

// tomlTopLevel reads the string keys above the first table, which is where
// fly.toml keeps `app` and wrangler.toml keeps `name` and `account_id`.
// Other values are skipped; this is not a TOML parser.
func tomlTopLevel(data []byte) (map[string]string, error) {
	keys := map[string]string{}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		value = strings.TrimSpace(value)
		if value == "" || (value[0] != '"' && value[0] != '\'') {
			continue
		}
		end := strings.IndexByte(value[1:], value[0])
		if end < 0 {
			return nil, fmt.Errorf("unterminated string for %s", key)
		}
		keys[key] = value[1 : end+1]
	}
	return keys, nil
}

// stripJSONC turns wrangler.jsonc into JSON: it drops // and /* */ comments
// and trailing commas, leaving strings untouched.
func stripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '"':
			start := i
			for i++; i < len(data) && data[i] != '"'; i++ {
				if data[i] == '\\' {
					i++
				}
			}
			out = append(out, data[start:min(i+1, len(data))]...)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			out = bytes.TrimRight(out, " \t")
			for i < len(data) && data[i] != '\n' {
				i++
			}
			i--
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			end := strings.Index(string(data[i+2:]), "*/")
			if end < 0 {
				return out
			}
			i += end + 3
		case c == '}' || c == ']':
			last := len(out) - 1
			for last >= 0 && strings.IndexByte(" \t\r\n", out[last]) >= 0 {
				last--
			}
			if last >= 0 && out[last] == ',' {
				out = append(out[:last], out[last+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}
