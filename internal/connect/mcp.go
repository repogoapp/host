package connect

import (
	"context"
	"strings"

	"github.com/repogoapp/host/internal/mcp"
)

// An MCP server, through the MCP service's own probe and sign-in: an API key
// is a secret step, OAuth a browser step whose redirect comes back through
// Callback, after a client ID when the server won't register RepoGo.

// mcpStatus is connected when a server at url is on for the project; one
// connected but off here is missing, and starting switches it on.
func (s *Service) mcpStatus(project, url string, c *Connection) {
	c.Status, c.Destination = Missing, "MCP servers for this project"
	if srv, ok := s.server(url); ok {
		c.Destination = srv.Label
		if onFor(srv, project) {
			c.Status = Connected
		}
	}
}

func (s *Service) startMCP(ctx context.Context, f *flow, label, note, help string) error {
	f.Destination = "MCP servers for this project"
	if srv, ok := s.server(f.target.URL); ok {
		f.Destination = srv.Label
		if !onFor(srv, f.project) {
			if err := s.d.MCP.SetEnabled(srv.ID, f.project, true); err != nil {
				return err
			}
		}
		f.end(Succeeded, "", s.d.Now())
		return nil
	}
	probe, err := s.d.MCP.Probe(ctx, f.target.URL, label, f.project)
	if err != nil {
		return err
	}
	f.probeID = probe.ProbeID
	switch {
	case probe.Auth == mcp.AuthAPIKey:
		f.Step = &Step{ID: "api_key", Kind: StepSecret, Label: "API key", Note: note, HelpURL: help}
	case probe.Auth == mcp.AuthOAuth && probe.NeedsManualClient:
		f.Step = clientIDStep(probe.RegistrationError, help)
	case probe.Auth == mcp.AuthOAuth:
		f.Step, err = s.signIn(ctx, f, "")
		return err
	default:
		if _, err := s.d.MCP.Connect(probe.ProbeID, ""); err != nil {
			return err
		}
		f.end(Succeeded, "", s.d.Now())
	}
	return nil
}

func (s *Service) submitMCP(ctx context.Context, f flow, value string) (*Step, error) {
	switch f.Step.ID {
	case "api_key":
		_, err := s.d.MCP.Connect(f.probeID, value)
		return nil, err
	case "client_id":
		return s.signIn(ctx, &f, strings.TrimSpace(value))
	}
	return nil, ErrInvalid.Errorf("step %q takes no value", f.Step.ID)
}

// signIn starts the server's OAuth and keeps the state its redirect brings
// back on the live flow, so Callback can find it.
func (s *Service) signIn(ctx context.Context, f *flow, clientID string) (*Step, error) {
	page, state, err := s.d.MCP.OAuthStart(ctx, f.probeID, clientID, f.loopback)
	if err != nil {
		return nil, err
	}
	redirect := RedirectApp
	if f.loopback {
		redirect = RedirectLoopback
	}
	s.mu.Lock()
	if live := s.flows[f.ID]; live != nil {
		live.state = state
	}
	s.mu.Unlock()
	f.state = state
	return &Step{ID: "sign_in", Kind: StepBrowser, URL: page, Redirect: redirect}, nil
}

func clientIDStep(why, help string) *Step {
	note := "This server won't register RepoGo. Create an OAuth client in its settings and paste its client ID."
	if why != "" {
		note = why + ". " + note
	}
	return &Step{ID: "client_id", Kind: StepPaste, Label: "Client ID", Note: note, HelpURL: help}
}

// server is the connected server at url, matched as the MCP service stores it.
func (s *Service) server(url string) (mcp.Server, bool) {
	for _, srv := range s.d.MCP.List("").Servers {
		if srv.URL == url {
			return srv, true
		}
	}
	return mcp.Server{}, false
}

// onFor is whether a server is switched on for project: for its root, or
// for a folder holding it.
func onFor(srv mcp.Server, project string) bool {
	if project == "" {
		return false
	}
	root := mcp.Root(project)
	for _, p := range srv.Projects {
		if root == p || strings.HasPrefix(root, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}
