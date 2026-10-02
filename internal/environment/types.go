// Package environment runs a project's committed environment.json: named
// services and run-once setup steps as managed terminal sessions, in a
// dependsOn graph gated by readyWhen, with ENVIRONMENT_* peer variables and
// envFrom secrets. Services start at boot for the clones this host made,
// after a fresh clone, and while a device has the project open; they stop on
// an idle countdown once none does.
package environment

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/repogo/host/internal/envsource"
)

type readyWhen struct {
	Port      int    `json:"port"`      // TCP connect to 127.0.0.1:port succeeds
	HTTP      string `json:"http"`      // HTTP GET returns < 500
	TimeoutMs int    `json:"timeoutMs"` // give up after this many ms (default 120000), then proceed
}

type serviceConfig struct {
	Name string `json:"name"` // stable slug; the managed id and the <NAME> in env vars
	// "service" = long-running (default). "setup" = run-once-and-exit;
	// dependents wait for its exit.
	Type      string            `json:"type"`
	Cmd       string            `json:"cmd"`
	Cwd       string            `json:"cwd"` // relative to repo root
	Target    string            `json:"-"`   // port ("3000") or host ("web.localhost"[:port])
	Expose    bool              `json:"expose"`
	Ready     *readyWhen        `json:"readyWhen"`
	Env       map[string]string `json:"env"`
	EnvFrom   []string          `json:"envFrom"`
	DependsOn []string          `json:"dependsOn"`
	AutoStart bool              `json:"-"`        // default true
	IdleStop  string            `json:"idleStop"` // "now" | "never" | "" (stopped when the idle countdown expires)
}

// UnmarshalJSON reads the fields with a default or a second spelling: target
// is a string or a number with `port` as its alias, and autoStart defaults on.
func (s *serviceConfig) UnmarshalJSON(raw []byte) error {
	type plain serviceConfig
	in := struct {
		*plain
		Target    json.RawMessage `json:"target"`
		Port      json.RawMessage `json:"port"`
		AutoStart *bool           `json:"autoStart"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	s.Target = parseTarget(in.Target, in.Port)
	s.AutoStart = in.AutoStart == nil || *in.AutoStart
	return nil
}

// envKey is the <NAME> token in ENVIRONMENT_*_<NAME> env vars.
func envKey(name string) string {
	return strings.ReplaceAll(strings.ToUpper(envsource.NormalizeHandle(name)), "-", "_")
}

// parseEnvironmentConfig is raw environment.json's runnable services.
// Forward-compatible: unknown fields are ignored, and services without a
// name or command are dropped (the rest still run). Nil when none remain.
func parseEnvironmentConfig(raw []byte) []serviceConfig {
	var doc struct {
		Services []serviceConfig `json:"services"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var services []serviceConfig
	for _, service := range doc.Services {
		service.Name = envsource.NormalizeHandle(service.Name)
		if service.Name == "" || service.Cmd == "" || seen[service.Name] {
			continue
		}
		seen[service.Name] = true
		if service.Type != "setup" {
			service.Type = "service"
		}
		if r := service.Ready; r != nil && r.Port == 0 && r.HTTP == "" {
			service.Ready = nil
		}
		services = append(services, service)
	}
	return services
}

// parseTarget accepts a string or number, with `port` as an alias.
func parseTarget(target, port json.RawMessage) string {
	for _, raw := range []json.RawMessage{target, port} {
		if len(raw) == 0 {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
			continue
		}
		var number json.Number
		if json.Unmarshal(raw, &number) == nil && number.String() != "" {
			return number.String()
		}
	}
	return ""
}

// targetPort is the local port a target serves on: "3000" or
// "web.localhost:3000"; 0 for a bare host name.
func targetPort(target string) int {
	if i := strings.LastIndexByte(target, ':'); i >= 0 {
		target = target[i+1:]
	}
	port, err := strconv.Atoi(target)
	if err != nil {
		return 0
	}
	return port
}
