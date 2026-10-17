// Package projectagent finds the agents a project brings with it: one folder
// each under `.repogo/agents`, whose agent.json names a program that speaks
// the Agent Client Protocol over stdio.
package projectagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/git"
)

// Folder holds a project's agents, from its checkout's top.
const Folder = ".repogo/agents"

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

// Manifest is one agent.json, with where it was found.
type Manifest struct {
	// ID is the agent's folder name, unique within its project.
	ID string `json:"-"`
	// Name is what the agent picker shows; the folder name when unset.
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`

	// Models is what the picker offers, the first the default, for an agent
	// that can't list its own over ACP (eve); the pick reaches it as the
	// environment variable ModelEnv names.
	Models   []ProjectAgentModel `json:"models"`
	ModelEnv string              `json:"modelEnv"`

	// Dir is the agent's folder and Project the checkout holding it.
	Dir     string `json:"-"`
	Project string `json:"-"`
	// Fingerprint changes when the manifest or its project does, so a trust
	// given to one command never covers another.
	Fingerprint string `json:"-"`
}

// Workdir is where the agent's process starts and what its sessions name as cwd.
func (m Manifest) Workdir() string {
	if m.Cwd == CwdAgent {
		return m.Dir
	}
	return m.Project
}

// ProjectAgentModel is one model an agent offers: ID is what reaches the
// agent, Name what the picker shows.
type ProjectAgentModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectAgent is one agent as the picker lists it; Error is set when its
// agent.json can't run, so a broken agent shows why instead of vanishing.
// Models is empty for an agent with nothing to choose; Model is its default.
type ProjectAgent struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Error  string              `json:"error,omitempty"`
	Models []ProjectAgentModel `json:"models,omitempty"`
	Model  string              `json:"model,omitempty"`
}

// Lister is what lists a project's agents for the picker: the agents'
// runner, which adds the models an agent reported once it ran.
type Lister interface {
	ProjectAgents(path string) ([]ProjectAgent, error)
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

// List is every agent in the project holding path, by ID; nil without a Folder.
func List(path string) ([]ProjectAgent, error) {
	project := Root(path)
	dirs, err := os.ReadDir(filepath.Join(project, Folder))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ProjectAgent
	for _, d := range dirs {
		if !d.IsDir() || !id.MatchString(d.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(project, Folder, d.Name(), ManifestFile)); err != nil {
			continue
		}
		m, err := read(project, d.Name())
		entry := ProjectAgent{ID: d.Name(), Name: m.Name, Models: m.Models, Model: m.DefaultModel("")}
		if err != nil {
			entry = ProjectAgent{ID: d.Name(), Name: d.Name(), Error: err.Error()}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Find reads one agent of the project holding path.
func Find(path, name string) (Manifest, error) {
	if !id.MatchString(name) {
		return Manifest{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return read(Root(path), name)
}

func read(project, name string) (Manifest, error) {
	dir := filepath.Join(project, Folder, name)
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
	if err := checkModels(m); err != nil {
		return m, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	sum := sha256.Sum256(append([]byte(project+"\x00"), raw...))
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
		if model.Name == "" {
			m.Models[i].Name = model.ID
		}
	}
	return nil
}
