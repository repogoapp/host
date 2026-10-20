package connect

import (
	"net/url"
	"time"
)

// A flow's states: waiting for the user, then one ending.
const (
	Waiting   = "waiting"
	Succeeded = "succeeded"
	Failed    = "failed"
	Cancelled = "cancelled"
	Expired   = "expired"
)

// How the phone draws a step.
const (
	// StepSecret is a field the phone draws without echoing what is typed.
	StepSecret = "secret"
	// StepPaste is a plain field, for a code a page shows or a client ID.
	StepPaste = "paste"
	// StepBrowser is a page to open; Redirect says how it ends.
	StepBrowser = "browser"
)

// How a browser step ends, as Step.Redirect.
const (
	// RedirectNone: the page finishes on its own, as a device code does.
	RedirectNone = ""
	// RedirectLocalhost: the page sends the browser to a CLI's localhost
	// server; the phone catches it and forwards it to this machine.
	RedirectLocalhost = "localhost"
	// RedirectApp: the page sends the browser back to the app, which hands
	// the code to connect.callback.
	RedirectApp = "app"
	// RedirectLoopback: as RedirectApp, through the phone's own localhost,
	// for a server that only accepts loopback redirects.
	RedirectLoopback = "loopback"
)

// maxNote bounds the explanation a caller adds to a step.
const maxNote = 500

// StartParams is what to connect and the words a caller may add. Where a value
// goes is never the caller's to word: the host resolves Scope itself.
type StartParams struct {
	Project string `json:"project" doc:"The project the card or agent is in"`
	Target  string `json:"target" doc:"What to connect: env:<NAME>, mcp:<url> or cli:<tool>"`
	Scope   string `json:"scope,omitempty" doc:"Which file an env key goes in: plugin:<name>, plugins, source:<handle> or project:<.env file>"`
	Label   string `json:"label,omitempty" doc:"The name an MCP server is listed and called by"`
	Note    string `json:"note,omitempty" doc:"A line on where to find the value"`
	HelpURL string `json:"help_url,omitempty" doc:"An https page where the value is found"`
	// Loopback is the phone's own knowledge of a server that only accepts
	// localhost redirects, from its presets.
	Loopback bool `json:"loopback,omitempty" doc:"The MCP server only accepts a localhost redirect"`
}

// Flow is one attempt to connect a target, as the phone draws it.
type Flow struct {
	ID     string `json:"id"`
	Target string `json:"target"`
	Scope  string `json:"scope"`
	// State is waiting, succeeded, failed, cancelled or expired.
	State string `json:"state"`
	// Destination is where the value goes, as the host words it.
	Destination string `json:"destination"`
	// Step is what the phone shows while the flow waits; nil once it ends.
	Step   *Step  `json:"step,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Step is one thing the phone asks of the user.
type Step struct {
	ID string `json:"id"`
	// Kind is how the phone draws it: secret, paste or browser.
	Kind string `json:"kind"`
	// Label names a field: API key, Client ID, Code.
	Label string `json:"label,omitempty"`
	// Name is the env key a secret is saved as.
	Name    string `json:"name,omitempty"`
	Note    string `json:"note,omitempty"`
	HelpURL string `json:"help_url,omitempty"`
	// URL is the page a browser step opens, or the one a paste step's code is on.
	URL string `json:"url,omitempty"`
	// UserCode is typed on a browser step's page: a device code.
	UserCode string `json:"user_code,omitempty"`
	// Redirect is how a browser step ends: "", localhost, app or loopback.
	Redirect string `json:"redirect,omitempty"`
	// Replaces says a value is set already, so the phone asks before replacing it.
	Replaces bool `json:"replaces"`
}

// flow is a Flow with what only the host keeps.
type flow struct {
	Flow
	project string
	target  target
	started time.Time
	ended   time.Time
	// For an MCP server: its probe, whether it redirects through the phone's
	// localhost, and the state its OAuth redirect carries back.
	probeID  string
	loopback bool
	state    string
	// claimed is set once a callback took the flow, so a second can't.
	claimed bool
}

func (f *flow) end(state, reason string, at time.Time) {
	f.State, f.Reason, f.Step, f.ended = state, reason, nil, at
}

// helpURL keeps a help link only when it is an https page.
func helpURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", ErrInvalid.Errorf("a help link must be an https page")
	}
	return u.String(), nil
}
