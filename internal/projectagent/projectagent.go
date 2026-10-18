// Package projectagent finds the agents a project can run: each a folder with
// an agent.json naming a program that speaks the Agent Client Protocol over
// stdio, in the project's or the environment's agents or plugins folder.
package projectagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/git"
)

// Folder holds a project's agents, from its checkout's top, and PluginFolder
// its plugins, any of which may bring an agent.json too.
const (
	Folder       = ".repogo/agents"
	PluginFolder = ".repogo/plugins"
)

// GlobalFolder and GlobalPluginFolder are the environment's, under the host's
// home: every project lists their agents, and its own of the same name win.
const (
	GlobalFolder       = "agents"
	GlobalPluginFolder = "plugins"
)

// ManifestFile is what makes a folder under Folder an agent.
const ManifestFile = "agent.json"

// Where an agent runs: the project, for agents that work on its code, or the
// agent's own folder, for an app such as eve that must start in its root.
const (
	CwdProject = "project"
	CwdAgent   = "agent"
)

// ErrNotFound is a name with no agent.json behind it.
var ErrNotFound = errkind.New(errkind.NotFound, "project agent not found")

// id is a folder name an agent can have: it lands in paths and chat rows.
var id = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// logo is an icon that names a logo the app ships, such as "claude".
var logo = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Manifest is one agent.json, with where it was found.
type Manifest struct {
	// ID is the agent's folder name, unique within its project.
	ID string `json:"-"`
	// Name is what the agent picker shows; the folder name when unset.
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`
	// Icon is what the picker draws for the agent; "" draws the default.
	Icon string `json:"icon"`

	// Models is what the picker offers, the first the default, for an agent
	// that can't list its own over ACP (eve); the pick reaches it as the
	// environment variable ModelEnv names.
	Models   []ProjectAgentModel `json:"models"`
	ModelEnv string              `json:"modelEnv"`

	// Dir is the agent's folder and Project the checkout it works for, which
	// holds Dir unless the agent is global.
	Dir     string `json:"-"`
	Project string `json:"-"`
	// Fingerprint changes when the manifest or its Key does, so a process
	// started from one never serves another.
	Fingerprint string `json:"-"`
}

// Workdir is where the agent's process starts and what its sessions name as cwd.
func (m Manifest) Workdir() string {
	if m.Cwd == CwdAgent {
		return m.Dir
	}
	return m.Project
}

// Key names the agent's process. One that runs in its own folder serves
// every project, as a second there may refuse to start; a global one that
// runs in the project's folder needs a process per project.
func (m Manifest) Key() string {
	if m.Cwd == CwdAgent {
		return m.Dir
	}
	return m.Dir + "\x00" + m.Project
}

// ProjectAgentModel is one model an agent offers: ID is what reaches the
// agent, Name and Icon what the picker shows.
type ProjectAgentModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon,omitempty"`
}

// ProjectAgent is one agent as the picker lists it; Error is set when its
// agent.json can't run, so a broken agent shows why instead of vanishing.
// Models is empty for an agent with nothing to choose; Model is its default.
type ProjectAgent struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Icon   string              `json:"icon,omitempty"`
	Error  string              `json:"error,omitempty"`
	Models []ProjectAgentModel `json:"models,omitempty"`
	Model  string              `json:"model,omitempty"`
}

// Lister is what lists a project's agents for the picker: the agents'
// runner, which adds the models an agent reported once it ran. Its
// ProjectAgentIcon is one agent's icon for its chats' rows.
type Lister interface {
	ProjectAgents(path string) ([]ProjectAgent, error)
	ProjectAgentIcon(path, name string) string
}

// DefaultModel is the model a turn runs when it names none, or one the
// manifest no longer lists; "" without models.
func (m Manifest) DefaultModel(asked string) string {
	for _, model := range m.Models {
		if model.ID == asked {
			return asked
		}
	}
	if len(m.Models) == 0 {
		return ""
	}
	return m.Models[0].ID
}

// Root is the checkout a folder is in, so a chat in a subfolder finds the
// project's agents.
func Root(path string) string {
	if root := git.Root(path); root != "" {
		return root
	}
	return filepath.Clean(path)
}

// folders is where the project's agents are found, the lowest first: the
// environment's folders in global, then the project's plugins and agents.
func folders(project string, global []string) []string {
	return append(append([]string{}, global...), filepath.Join(project, PluginFolder), filepath.Join(project, Folder))
}

// List is every agent the project holding path can run, by ID, from its own
// folders and the environment's in global, lowest first; a later folder wins
// a name two share.
func List(path string, global []string) ([]ProjectAgent, error) {
	project := Root(path)
	byID := map[string]ProjectAgent{}
	for _, folder := range folders(project, global) {
		ids, err := agentIDs(folder)
		if err != nil {
			return nil, err
		}
		for _, name := range ids {
			m, err := read(project, folder, name)
			entry := ProjectAgent{ID: name, Name: m.Name, Icon: m.Icon, Models: m.Models, Model: m.DefaultModel("")}
			if err != nil {
				entry = ProjectAgent{ID: name, Name: name, Error: err.Error()}
			}
			byID[name] = entry
		}
	}
	if len(byID) == 0 {
		return nil, nil
	}
	out := make([]ProjectAgent, 0, len(byID))
	for _, a := range byID {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// agentIDs is each folder in folder holding a manifest; none when it's missing.
func agentIDs(folder string) ([]string, error) {
	dirs, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if !d.IsDir() || !id.MatchString(d.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(folder, d.Name(), ManifestFile)); err == nil {
			out = append(out, d.Name())
		}
	}
	return out, nil
}

// Find reads one agent the project holding path can run, from the highest of
// the folders List reads that has it.
func Find(path string, global []string, name string) (Manifest, error) {
	if !id.MatchString(name) {
		return Manifest{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	project := Root(path)
	all := folders(project, global)
	for i := len(all) - 1; i >= 0; i-- {
		m, err := read(project, all[i], name)
		if !errors.Is(err, ErrNotFound) {
			return m, err
		}
	}
	return Manifest{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// read is folder's agent name, working for project.
func read(project, folder, name string) (Manifest, error) {
	dir := filepath.Join(folder, name)
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{ID: name, Name: name}
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{ID: name, Name: name}, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	m.ID, m.Dir, m.Project = name, dir, project
	if m.Name == "" {
		m.Name = name
	}
	switch {
	case m.Command == "":
		return m, fmt.Errorf("%s: command is empty", ManifestFile)
	case m.Cwd == "":
		m.Cwd = CwdProject
	case m.Cwd != CwdProject && m.Cwd != CwdAgent:
		return m, fmt.Errorf("%s: cwd is %q, not %q or %q", ManifestFile, m.Cwd, CwdProject, CwdAgent)
	}
	if err := checkIcon(m.Icon); err != nil {
		return m, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if err := checkModels(m); err != nil {
		return m, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	sum := sha256.Sum256(append([]byte(m.Key()+"\x00"), raw...))
	m.Fingerprint = hex.EncodeToString(sum[:])
	return m, nil
}

// checkModels is a models list the agent can be handed: each with an id,
// none twice, and the variable that carries the pick.
func checkModels(m Manifest) error {
	if len(m.Models) == 0 {
		return nil
	}
	if m.ModelEnv == "" {
		return errors.New("models needs modelEnv, the variable the agent reads its model from")
	}
	seen := map[string]bool{}
	for i, model := range m.Models {
		if model.ID == "" {
			return fmt.Errorf("model %d has no id", i+1)
		}
		if seen[model.ID] {
			return fmt.Errorf("model %q is listed twice", model.ID)
		}
		seen[model.ID] = true
		if err := checkIcon(model.Icon); err != nil {
			return fmt.Errorf("model %q: %w", model.ID, err)
		}
		if model.Name == "" {
			m.Models[i].Name = model.ID
		}
	}
	return nil
}

// checkIcon is an icon the app can draw: "" for the default, an https image
// or the name of a logo it ships. An unknown name draws the default.
func checkIcon(icon string) error {
	if icon == "" || logo.MatchString(icon) {
		return nil
	}
	u, err := url.Parse(icon)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("icon %q is not an https URL or a logo name", icon)
	}
	return nil
}
