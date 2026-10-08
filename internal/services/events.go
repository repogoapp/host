package services

import "github.com/repogo/host/internal/emit"

func init() {
	emit.Register(Status{})
}

// Status is a project's services, sent to every device on each transition.
type Status struct {
	Path     string `json:"path"`
	Root     string `json:"root"`
	Revision int64  `json:"revision"`
	// IdleDeadline is when the idle countdown stops the services, RFC 3339.
	IdleDeadline string    `json:"idle_deadline,omitempty"`
	Services     []Service `json:"services" wire:"array"`
}

func (Status) Method() string { return "services.status" }

// Service is one entry of environment.json and whether it is running now.
type Service struct {
	Name      string `json:"name"`
	Type      string `json:"type"` // "service" | "setup"
	Cmd       string `json:"cmd"`
	Target    string `json:"target"`
	Expose    bool   `json:"expose"`
	URL       string `json:"url"`
	Running   bool   `json:"running"`
	SessionID string `json:"session_id"`
	AutoStart bool   `json:"auto_start"`
	IdleStop  string `json:"idle_stop"`
}
