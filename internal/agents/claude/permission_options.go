package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/repogo/host/internal/claudecode"
)

type permissionChange struct {
	Type        string           `json:"type"`
	Destination string           `json:"destination"`
	Behavior    string           `json:"behavior"`
	Mode        string           `json:"mode"`
	Rules       []permissionRule `json:"rules"`
	Directories []string         `json:"directories"`
}
type permissionRule struct {
	Tool    string  `json:"toolName"`
	Content *string `json:"ruleContent"`
}

func validPermissionText(value string, limit int) bool {
	if strings.TrimSpace(value) == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if r <= 8 || r == 11 || r == 12 || r >= 14 && r <= 31 || r == 127 {
			return false
		}
	}
	return true
}

func permissionChanges(raw []json.RawMessage) []permissionChange {
	if len(raw) == 0 || len(raw) > 100 {
		return nil
	}
	changes := make([]permissionChange, 0, len(raw))
	for _, r := range raw {
		var c permissionChange
		if json.Unmarshal(r, &c) != nil || !slices.Contains([]string{"session", "cliArg", "userSettings", "projectSettings", "localSettings"}, c.Destination) {
			return nil
		}
		switch c.Type {
		case "addRules":
			if c.Behavior != "allow" || len(c.Rules) == 0 || len(c.Rules) > 100 {
				return nil
			}
			for _, rule := range c.Rules {
				if !validPermissionText(rule.Tool, 512) || rule.Content != nil && !validPermissionText(*rule.Content, 10000) {
					return nil
				}
			}
		case "addDirectories":
			if len(c.Directories) == 0 || len(c.Directories) > 100 {
				return nil
			}
			for _, dir := range c.Directories {
				if !validPermissionText(dir, 4096) {
					return nil
				}
			}
		case "setMode":
			if c.Mode != "acceptEdits" {
				return nil
			}
		default:
			return nil
		}
		changes = append(changes, c)
	}
	return changes
}

func suggestedPermissions(p claudecode.PermissionRequest, cwd string) ([]json.RawMessage, string) {
	changes := permissionChanges(p.Suggestions)
	var input map[string]string
	_ = json.Unmarshal(p.Input, &input)
	var label string
	switch p.ToolName {
	case "Bash", "PowerShell":
		label = shellPermissionLabel(changes, p.ToolName)
	case "Read", "Glob", "Grep", "Edit", "Write", "NotebookEdit":
		path := input["file_path"]
		if p.ToolName == "NotebookEdit" {
			path = input["notebook_path"]
		}
		if p.ToolName == "Glob" || p.ToolName == "Grep" {
			path = input["path"]
			if path == "" {
				path = cwd
			}
		}
		write := p.ToolName == "Edit" || p.ToolName == "Write" || p.ToolName == "NotebookEdit"
		label = filePermissionLabel(changes, cwd, path, write)
	case "SandboxNetworkAccess":
		if len(changes) == 1 && changes[0].Destination == "localSettings" && len(changes[0].Rules) == 1 {
			rule := changes[0].Rules[0]
			if rule.Tool == p.ToolName && rule.Content != nil && *rule.Content == input["host"] {
				label = "Yes, and don't ask again for " + input["host"]
			}
		}
	default:
		name := p.ToolName
		if strings.TrimSpace(p.DisplayName) != "" {
			name = strings.TrimSpace(p.DisplayName)
		}
		label = "Yes, and don't ask again for " + name + " commands"
		if !strings.HasPrefix(p.ToolName, "mcp__") {
			return allowRule(p.ToolName, ""), label
		}
		if strings.HasPrefix(p.ToolName, "mcp__computer-use__") {
			label = "Yes, and don't ask again for " + name
		}
		for _, change := range changes {
			if change.Type != "addRules" {
				return nil, ""
			}
			for _, rule := range change.Rules {
				if rule.Tool != p.ToolName || rule.Content != nil {
					return nil, ""
				}
			}
		}
	}
	if len(changes) == 0 || label == "" {
		return nil, ""
	}
	return p.Suggestions, label
}

func displayList(values []string) string {
	if len(strings.Join(values, ", ")) > 50 {
		return "similar"
	}
	if len(values) == 1 {
		return values[0]
	}
	if len(values) == 2 {
		return strings.Join(values, " and ")
	}
	return strings.Join(values[:len(values)-1], ", ") + ", and " + values[len(values)-1]
}

func displayPaths(paths []string) string {
	var unique []string
	for _, path := range paths {
		path = filepath.Clean(path)
		if !slices.Contains(unique, path) {
			unique = append(unique, path)
		}
	}
	names := make([]string, 0, len(unique))
	for i, path := range unique {
		parts := strings.Split(strings.Trim(path, string(filepath.Separator)), string(filepath.Separator))
		name := path
		for depth := 1; depth <= len(parts); depth++ {
			candidate := strings.Join(parts[len(parts)-depth:], string(filepath.Separator))
			clash := false
			for j, other := range unique {
				if i != j && (other == candidate || strings.HasSuffix(other, string(filepath.Separator)+candidate)) {
					clash = true
					break
				}
			}
			if !clash {
				name = candidate
				break
			}
		}
		names = append(names, strings.TrimSuffix(name, string(filepath.Separator))+string(filepath.Separator))
	}
	if len(names) <= 2 {
		return displayList(names)
	}
	return names[0] + ", " + names[1] + " and " + strconv.Itoa(len(names)-2) + " more"
}

func shellPermissionLabel(changes []permissionChange, tool string) string {
	var commands, reads, dirs []string
	for _, change := range changes {
		switch change.Type {
		case "addDirectories":
			if change.Destination != "session" {
				return ""
			}
			dirs = append(dirs, change.Directories...)
		case "addRules":
			for _, rule := range change.Rules {
				if rule.Content == nil {
					return ""
				}
				switch rule.Tool {
				case tool:
					command := strings.TrimSuffix(*rule.Content, ":*")
					if !slices.Contains(commands, command) {
						commands = append(commands, command)
					}
				case "Read":
					reads = append(reads, strings.TrimSuffix(*rule.Content, "/**"))
				default:
					return ""
				}
			}
		default:
			return ""
		}
	}
	paths := append(slices.Clone(dirs), reads...)
	switch {
	case len(paths) == 0 && len(commands) > 0:
		return "Yes, and don't ask again for " + displayList(commands) + " commands"
	case len(reads) > 0 && len(dirs) == 0 && len(commands) == 0:
		return "Yes, allow reading from " + displayPaths(reads) + " from this project"
	case len(paths) > 0 && len(commands) == 0:
		return "Yes, and always allow access to " + displayPaths(paths) + " from this project"
	case len(paths) == 1 && len(commands) == 1:
		return "Yes, and allow access to " + displayPaths(paths) + " and " + displayList(commands) + " commands"
	case len(paths) > 0 && len(commands) > 0:
		return "Yes, and allow " + displayPaths(paths) + " access and " + displayList(commands) + " commands"
	default:
		return ""
	}
}

func insideDirectory(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func filePermissionLabel(changes []permissionChange, cwd, path string, write bool) string {
	if len(changes) == 0 {
		return ""
	}
	tool := "Read"
	if write {
		tool = "Edit"
	}
	if path != "" && !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	covered, broad, accept, editRule := false, false, false, false
	for _, change := range changes {
		if change.Destination != "session" {
			return ""
		}
		var paths []string
		switch change.Type {
		case "setMode":
			if !write || change.Mode != "acceptEdits" {
				return ""
			}
			accept = true
		case "addDirectories":
			if !write {
				return ""
			}
			paths = change.Directories
		case "addRules":
			for _, rule := range change.Rules {
				if rule.Tool != tool {
					return ""
				}
				editRule = editRule || rule.Tool == "Edit"
				if rule.Content == nil {
					broad = true
				} else {
					paths = append(paths, strings.TrimSuffix(*rule.Content, "/**"))
				}
			}
		default:
			return ""
		}
		for _, dir := range paths {
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(cwd, dir)
			}
			covered = covered || path != "" && insideDirectory(dir, path)
		}
	}
	home, _ := os.UserHomeDir()
	ownSettings := write && path != "" && (insideDirectory(filepath.Join(cwd, ".claude"), path) || home != "" && insideDirectory(filepath.Join(home, ".claude"), path))
	if !covered && !broad && !(write && accept && !ownSettings) {
		return ""
	}
	if ownSettings && editRule {
		return "Yes, and allow Claude to edit its own settings for this session"
	}
	if path == "" || insideDirectory(cwd, path) {
		if write {
			return "Yes, allow all edits during this session"
		}
		return "Yes, during this session"
	}
	dir := filepath.Base(filepath.Dir(path)) + string(filepath.Separator)
	if write {
		return "Yes, allow all edits in " + dir + " during this session"
	}
	return "Yes, allow reading from " + dir + " during this session"
}
