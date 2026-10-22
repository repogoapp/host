package connect

import (
	"context"
	"slices"
	"strings"

	"github.com/repogoapp/host/internal/mcp"
)

// An MCP server, through the MCP service's own probe and sign-in: an API key
// is a secret step, OAuth a browser step whose redirect comes back through
// Callback, after a client ID when the server won't register RepoGo.

// mcpStatus is connected when an account on url is on for the project; one
// connected but off here is missing, and starting switches it on.
func (s *Service) mcpStatus(project, url string, c *Connection) {
	c.Status, c.Destination = Missing, "MCP servers for this project"
	all, on := s.accounts(url, project)
	switch {
	case len(on) > 0:
		c.Status, c.Destination = Connected, labels(on)
	case len(all) > 0:
		c.Destination = all[0].Label
	}
}

// startMCP connects a server. One already connected opens as connected,
// unless account asks to sign it in again or to add another account.
func (s *Service) startMCP(ctx context.Context, f *flow, label, note, help, account string) error {
	f.Destination = "MCP servers for this project"
	all, on := s.accounts(f.target.URL, f.project)
	var p mcp.Probe
	var err error
	switch {
	case account == "again":
		// The one connection this project uses, or the only one there is.
		mine := on
		if len(mine) == 0 {
			mine = all
		}
		if len(mine) != 1 {
			return errRefused.Errorf("pick which account to sign in again in MCP settings")
		}
		f.Destination = mine[0].Label
		p, err = s.d.MCP.ProbeAgain(ctx, mine[0].ID, f.project)
	case account == "another":
		p, err = s.d.MCP.ProbeAnother(ctx, f.target.URL, label, f.project)
	case account != "":
		return ErrInvalid.Errorf("account must be again or another")
	case len(on) > 0:
		f.Destination = labels(on)
		f.end(Succeeded, "", s.d.Now())
		return nil
	case len(all) == 1:
		f.Destination = all[0].Label
		if err := s.d.MCP.SetEnabled(all[0].ID, f.project, true); err != nil {
			return err
		}
		f.end(Succeeded, "", s.d.Now())
		return nil
	case len(all) > 1:
		// Which account's credentials a project uses is the user's to pick.
		return errRefused.Errorf("%s are connected; turn one on for this project in MCP settings", labels(all))
	default:
		p, err = s.d.MCP.Probe(ctx, f.target.URL, label, f.project)
	}
	if err != nil {
		return err
	}
	f.probeID = p.ProbeID
	switch {
	case p.Auth == mcp.AuthAPIKey:
		f.Step = &Step{ID: "api_key", Kind: StepSecret, Label: "API key", Note: note, HelpURL: help}
	case p.Auth == mcp.AuthOAuth && p.NeedsManualClient && !f.loopback:
		f.Step = clientIDStep(p.RegistrationError, help, f.loopback)
	case p.Auth == mcp.AuthOAuth:
		// A loopback-only server refuses the app redirect at probe time but
		// registers again with its own; a client ID is asked for only then.
		if f.Step, err = s.signIn(ctx, f, ""); err != nil && p.NeedsManualClient {
			f.Step, err = clientIDStep(p.RegistrationError, help, f.loopback), nil
		}
		return err
	default:
		if _, err := s.d.MCP.Connect(p.ProbeID, ""); err != nil {
			return err
		}
		f.end(Succeeded, "", s.d.Now())
	}
	return nil
}

func (s *Service) submitMCP(ctx context.Context, f flow, value string) (*Step, error) {
	switch f.Step.ID {
	case "api_key":
		// Checked here: the MCP service spends its probe before refusing an empty key.
		if strings.TrimSpace(value) == "" {
			return nil, ErrInvalid.Errorf("paste the API key")
		}
		_, err := s.d.MCP.Connect(f.probeID, value)
		return nil, err
	case "client_id":
		return s.signIn(ctx, &f, strings.TrimSpace(value))
	}
	return nil, ErrInvalid.Errorf("step %q takes no value", f.Step.ID)
}

// disconnectMCP switches the server off for the flow's project, keeping its
// sign-in. One also on for a folder above the project would stay on here, so
// it is refused rather than reported off.
func (s *Service) disconnectMCP(f flow) error {
	_, on := s.accounts(f.target.URL, f.project)
	switch {
	case len(on) == 0:
		return nil
	case len(on) > 1:
		return errRefused.Errorf("%s are all on for this project; turn one off in MCP settings", labels(on))
	}
	srv := on[0]
	root := mcp.Root(f.project)
	above := slices.ContainsFunc(srv.Projects, func(p string) bool {
		return p != root && onFor(mcp.Server{Projects: []string{p}}, f.project)
	})
	if above {
		return errRefused.Errorf("%s is on for a folder that holds this project; turn it off in MCP settings", srv.Label)
	}
	return s.d.MCP.SetEnabled(srv.ID, f.project, false)
}

// signIn starts the server's OAuth and keeps the state its redirect brings
// back on the live flow, for Callback to check; a claim makes this the one
// submit acting on it.
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
	return &Step{ID: stepSignIn, Kind: StepBrowser, URL: page, Redirect: redirect}, nil
}

func clientIDStep(why, help string, loopback bool) *Step {
	redirect := mcp.NativeRedirect
	if loopback {
		redirect = mcp.LoopbackRedirect
	}
	note := "This server won't register RepoGo. Create an OAuth client in its settings, with " + redirect +
		" as its redirect URI, and paste its client ID."
	if why != "" {
		note = why + ". " + note
	}
	return &Step{ID: "client_id", Kind: StepPaste, Label: "Client ID", Note: note, HelpURL: help}
}

// accounts is every connection to url, matched as the MCP service stores it,
// and those of them switched on for project.
func (s *Service) accounts(url, project string) (all, on []mcp.Server) {
	for _, srv := range s.d.MCP.List("").Servers {
		if srv.URL != url {
			continue
		}
		all = append(all, srv)
		if onFor(srv, project) {
			on = append(on, srv)
		}
	}
	return all, on
}

// labels names connections as the phone shows them: "Linear, Linear (work)".
func labels(servers []mcp.Server) string {
	names := make([]string, len(servers))
	for i, srv := range servers {
		names[i] = srv.Label
	}
	return strings.Join(names, ", ")
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
