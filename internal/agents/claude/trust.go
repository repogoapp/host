package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/repogoapp/host/internal/agent"
)

// A repo's own Claude settings can run hooks and widen what runs unasked. A
// terminal asks to trust a folder before loading them; headless Claude never
// asks, so the host does, sharing the answer with the terminal.

var (
	allSources  = []string{"user", "project", "local"}
	userSources = []string{"user"}
)

const (
	trustQuestionID = "trust"
	trustYes        = "Trust"
	trustNo         = "Not now"
)

// settingSources is what a turn in req.Cwd loads: everything in a folder
// Claude trusts, or in one whose settings change nothing that runs; else
// the user's own, once the user has been asked and hasn't said yes.
func (r *runner) settingSources(ctx context.Context, req agent.TurnRequest, io agent.TurnIO) []string {
	if req.Cwd == "" || trustedFolder(r.configFile(), req.Cwd) || sameDir(filepath.Join(req.Cwd, ".claude"), r.home) {
		return allSources
	}
	found := projectSettings(req.Cwd)
	if len(found) == 0 {
		return allSources
	}
	r.trustMu.Lock()
	declined := r.declined[req.Cwd]
	r.trustMu.Unlock()
	if declined || io.Ask == nil {
		return userSources
	}
	answer, err := io.Ask(ctx, trustApproval(io.TurnID, found))
	if err != nil {
		return userSources
	}
	if pickedLabel(answer) != trustYes {
		r.trustMu.Lock()
		if r.declined == nil {
			r.declined = map[string]bool{}
		}
		r.declined[req.Cwd] = true
		r.trustMu.Unlock()
		return userSources
	}
	if err := markTrusted(r.configFile(), req.Cwd); err != nil {
		r.deps.Log.Warn("couldn't save Claude's trust for the folder", "cwd", req.Cwd, "err", err)
	}
	return allSources
}

// sameDir reports whether a and b are one folder, through symlinks. A chat in
// the home folder finds Claude's own ~/.claude there: the user's settings,
// which every turn loads anyway, not a project's to be trusted.
func sameDir(a, b string) bool {
	if b == "" {
		return false
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// pickedLabel is the option chosen in the trust form's one question.
func pickedLabel(answer json.RawMessage) string {
	var values map[string]json.RawMessage
	var label string
	if json.Unmarshal(answer, &values) == nil {
		_ = json.Unmarshal(values[trustQuestionID], &label)
	}
	return label
}

func trustApproval(turnID string, found []string) agent.Approval {
	text := "This project has its own Claude settings:\n• " + strings.Join(found, "\n• ") +
		"\n\nTrust it to load them, as Claude in a terminal does once you trust a folder."
	return agent.Approval{
		CallID: "trust:" + turnID, Title: "Trust this project?", Kind: agent.ApprovalQuestion,
		Questions: []agent.Question{{
			ID: trustQuestionID, Header: "Claude settings", Text: text,
			Options: []agent.QuestionOption{
				{Label: trustYes, Description: "Load its hooks and rules; Claude in the terminal won't ask either"},
				{Label: trustNo, Description: "Run this chat with your own settings only"},
			},
		}},
	}
}

// configFile is Claude's account file, where it keeps each folder's trust.
func (r *runner) configFile() string {
	if r.account != "" {
		return r.account
	}
	if r.deps.Root != "" {
		return filepath.Join(r.home, ".claude.json")
	}
	return defaultConfigFile()
}

// trustedFolder is whether Claude trusts dir or a folder above it: the
// hasTrustDialogAccepted a terminal saves. An unreadable file trusts nothing.
func trustedFolder(configFile, dir string) bool {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return false
	}
	var config struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(data, &config) != nil {
		return false
	}
	for dir = filepath.Clean(dir); ; dir = filepath.Dir(dir) {
		if config.Projects[dir].Trusted {
			return true
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false
		}
	}
}

// markTrusted sets hasTrustDialogAccepted for dir, keeping every other key
// of Claude's file as it was, and replaces the file in one rename.
func markTrusted(configFile, dir string) error {
	config := map[string]json.RawMessage{}
	mode := os.FileMode(0o600)
	if data, err := os.ReadFile(configFile); err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("read %s: %w", configFile, err)
		}
		if info, err := os.Stat(configFile); err == nil {
			mode = info.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	projects := map[string]map[string]json.RawMessage{}
	if raw, ok := config["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("read projects in %s: %w", configFile, err)
		}
	}
	dir = filepath.Clean(dir)
	if projects[dir] == nil {
		projects[dir] = map[string]json.RawMessage{}
	}
	projects[dir]["hasTrustDialogAccepted"] = json.RawMessage("true")
	raw, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	config["projects"] = raw
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(config); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configFile), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(configFile), ".claude.json.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), configFile)
}

// projectSettings names what dir's own Claude settings would do: the hooks
// they run, the commands they allow, and other keys that run code or widen
// access. Empty when there are none; an unreadable file is named too.
func projectSettings(dir string) []string {
	var found []string
	for _, name := range []string{"settings.json", "settings.local.json"} {
		path := filepath.Join(dir, ".claude", name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s claudeSettings
		if json.Unmarshal(data, &s) != nil {
			found = append(found, ".claude/"+name+" can't be read")
			continue
		}
		found = append(found, s.describe()...)
	}
	return found
}

type claudeSettings struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
	Permissions struct {
		Allow                 []string `json:"allow"`
		DefaultMode           string   `json:"defaultMode"`
		AdditionalDirectories []string `json:"additionalDirectories"`
	} `json:"permissions"`
	Env                        map[string]string `json:"env"`
	APIKeyHelper               string            `json:"apiKeyHelper"`
	AWSAuthRefresh             string            `json:"awsAuthRefresh"`
	AWSCredentialExport        string            `json:"awsCredentialExport"`
	OtelHeadersHelper          string            `json:"otelHeadersHelper"`
	EnableAllProjectMcpServers bool              `json:"enableAllProjectMcpServers"`
	EnabledMcpjsonServers      []string          `json:"enabledMcpjsonServers"`
}

func (s claudeSettings) describe() []string {
	var out []string
	events := make([]string, 0, len(s.Hooks))
	for event := range s.Hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	for _, event := range events {
		for _, group := range s.Hooks[event] {
			for _, hook := range group.Hooks {
				if hook.Command != "" {
					out = append(out, fmt.Sprintf("Runs on %s: %s", event, hook.Command))
				}
			}
		}
	}
	if len(s.Permissions.Allow) > 0 {
		out = append(out, "Allows without asking: "+strings.Join(s.Permissions.Allow, ", "))
	}
	if mode := s.Permissions.DefaultMode; mode != "" && mode != "default" {
		out = append(out, "Starts in "+mode+" mode")
	}
	if len(s.Permissions.AdditionalDirectories) > 0 {
		out = append(out, "Reaches folders outside it: "+strings.Join(s.Permissions.AdditionalDirectories, ", "))
	}
	for _, helper := range []struct{ name, command string }{
		{"apiKeyHelper", s.APIKeyHelper}, {"awsAuthRefresh", s.AWSAuthRefresh},
		{"awsCredentialExport", s.AWSCredentialExport}, {"otelHeadersHelper", s.OtelHeadersHelper},
	} {
		if helper.command != "" {
			out = append(out, fmt.Sprintf("Runs as %s: %s", helper.name, helper.command))
		}
	}
	if len(s.Env) > 0 {
		keys := make([]string, 0, len(s.Env))
		for key := range s.Env {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		out = append(out, "Sets "+strings.Join(keys, ", "))
	}
	if s.EnableAllProjectMcpServers {
		out = append(out, "Turns on every MCP server in .mcp.json")
	} else if len(s.EnabledMcpjsonServers) > 0 {
		out = append(out, "Turns on MCP servers: "+strings.Join(s.EnabledMcpjsonServers, ", "))
	}
	return out
}
