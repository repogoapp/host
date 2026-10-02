package repogomcp

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/errkind"
)

func init() {
	emit.Register(Request{})
}

// ErrNotFound answers a respond for a request that has ended, or that went to
// another device.
var ErrNotFound = errkind.New(errkind.NotFound, "no such browser request")

const (
	// How long past its own timeout an action may take on the phone: the
	// phone spends timeout_ms in the page, so the wait must outlive it.
	answerGrace = 5 * time.Second
	// How long a phone that was not connected has to come back: its user has
	// to see the alert and open the app.
	wakeWait = time.Minute
)

// Request is one browser action for the phone that sent the turn, and only
// that phone: it is the one whose preview the user is looking at.
type Request struct {
	RequestID string `json:"request_id"`
	TurnID    string `json:"turn_id"`
	ChatID    string `json:"chat_id"`
	// The turn's working tree; with this host's id, the phone's tab bucket.
	Cwd       string `json:"cwd"`
	ExpiresAt string `json:"expires_at"` // RFC 3339
	Action    Action `json:"action"`
}

func (Request) Method() string { return "browser.request" }

// Action is v1's browser.ActionRequest, checked and trimmed to what its
// action uses.
type Action struct {
	Action          string   `json:"action"`
	BrowserID       string   `json:"browser_id"`
	URL             string   `json:"url"`
	Script          string   `json:"script"`
	Format          string   `json:"format"`
	Locator         string   `json:"locator"`
	Text            string   `json:"text"`
	Clear           bool     `json:"clear"`
	Key             string   `json:"key"`
	Modifiers       []string `json:"modifiers"`
	DeltaX          float64  `json:"delta_x"`
	DeltaY          float64  `json:"delta_y"`
	WaitText        string   `json:"wait_text"`
	WaitURLIncludes string   `json:"wait_url_includes"`
	X               float64  `json:"x"`
	Y               float64  `json:"y"`
	Value           string   `json:"value"`
	IncludeConsole  bool     `json:"include_console"`
	IncludeNetwork  bool     `json:"include_network"`
	SettleMS        int64    `json:"settle_ms"`
	SinceMS         int64    `json:"since_ms"`
	LogLevel        string   `json:"log_level"`
	URLContains     string   `json:"url_contains"`
	Limit           int      `json:"limit"`
	IncludeElements bool     `json:"include_elements"`
	// Also a screenshot, for snapshot.
	IncludeScreenshot bool  `json:"include_screenshot"`
	ChangedOnly       bool  `json:"changed_only"`
	TimeoutMS         int64 `json:"timeout_ms"`
}

// Result is what the phone found, v1's browser.ActionResponse.
type Result struct {
	OK                  bool                 `json:"ok"`
	ErrorText           string               `json:"error_text,omitempty"`
	URL                 string               `json:"url,omitempty"`
	Title               string               `json:"title,omitempty"`
	ResultJSON          string               `json:"result_json,omitempty"`
	Snapshot            string               `json:"snapshot,omitempty"`
	SnapshotKind        string               `json:"snapshot_kind,omitempty"`
	Navigated           bool                 `json:"navigated,omitempty"`
	RecordingID         string               `json:"recording_id,omitempty"`
	Target              *ElementState        `json:"target,omitempty"`
	Tabs                []Tab                `json:"tabs,omitempty"`
	ConsoleDelta        string               `json:"console_delta,omitempty"`
	NetworkDelta        string               `json:"network_delta,omitempty"`
	ConsoleCount        int                  `json:"console_count,omitempty"`
	NetworkCount        int                  `json:"network_count,omitempty"`
	InteractiveElements []InteractiveElement `json:"interactive_elements,omitempty"`
	Screenshot          *Screenshot          `json:"screenshot,omitempty"`
}

// ElementState is the acted-on element after a semantic action settled.
type ElementState struct {
	Role     string `json:"role"`
	Name     string `json:"name,omitempty"`
	Value    string `json:"value,omitempty"`
	Checked  string `json:"checked,omitempty"`
	Selected string `json:"selected,omitempty"`
	Expanded string `json:"expanded,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	Focused  bool   `json:"focused,omitempty"`
	Locator  string `json:"locator,omitempty"`
}

type Tab struct {
	ID     string `json:"id"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title,omitempty"`
	Active bool   `json:"active,omitempty"`
}

type InteractiveElement struct {
	Role     string  `json:"role"`
	Name     string  `json:"name,omitempty"`
	Value    string  `json:"value,omitempty"`
	Locator  string  `json:"locator,omitempty"`
	Selector string  `json:"selector,omitempty"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Width    float64 `json:"width"`
	Height   float64 `json:"height"`
}

type Screenshot struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

type BrowserConfig struct {
	// Phone reports a paired device: a turn from the host itself has none.
	Phone func(device.ID) bool
	// Send delivers a request to its device and reports whether it went out.
	Send func(device.ID, Request) bool
	// Alert wakes a device that was not connected.
	Alert func(device.ID, Request)
}

// Browser brokers actions between a turn and the phone that sent it: pushed
// to that phone, answered with browser.respond, or fetched with
// browser.pending by a phone that was away when it was sent.
type Browser struct {
	cfg BrowserConfig

	mu      sync.Mutex
	pending map[string]*call
}

type call struct {
	to  device.ID
	req Request
	// Buffered: Respond hands over the answer without waiting on this side.
	answer chan Result
}

func NewBrowser(cfg BrowserConfig) *Browser {
	return &Browser{cfg: cfg, pending: map[string]*call{}}
}

// errNoPhone and errNoAnswer are the agent's to read, in v1's words where v1 had some.
const (
	errNoPhone  = "RepoGo: this turn did not come from a phone, so there is no preview to control. Send it from the RepoGo app."
	errNoAnswer = "RepoGo: the phone did not answer. Open RepoGo on it, keep the preview open, and try again."
	errNoWake   = "RepoGo: the phone that sent this turn is not connected, and did not wake. Open RepoGo on it and try again."
)

// Do sends one action to the turn's phone and waits for its answer, the
// action's own timeout, or the turn.
func (b *Browser) Do(t agent.RunningTurn, a Action) (Result, string) {
	if t.Device == "" || !b.cfg.Phone(t.Device) {
		return Result{}, errNoPhone
	}
	answer := time.Duration(a.TimeoutMS)*time.Millisecond + answerGrace
	c := &call{
		to: t.Device,
		req: Request{
			RequestID: uuid.NewString(), TurnID: t.TurnID, ChatID: t.ChatID, Cwd: t.Cwd, Action: a,
			// Long enough for a phone that has to be woken: past it, pending drops it.
			ExpiresAt: time.Now().Add(answer + wakeWait).UTC().Format(time.RFC3339),
		},
		answer: make(chan Result, 1),
	}
	// Filed before it is sent, so an answer that beats Send finds it.
	b.mu.Lock()
	b.pending[c.req.RequestID] = c
	b.mu.Unlock()
	defer b.take(c.to, c.req.RequestID)

	wait := answer
	delivered := b.cfg.Send(t.Device, c.req)
	if !delivered {
		wait += wakeWait
		b.cfg.Alert(t.Device, c.req)
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case r := <-c.answer:
		return r, ""
	case <-timer.C:
		if delivered {
			return Result{}, errNoAnswer
		}
		return Result{}, errNoWake
	case <-t.Ctx.Done():
		return Result{}, "RepoGo: the turn was stopped"
	}
}

// Pending is every request still waiting for d, oldest first.
func (b *Browser) Pending(d device.ID) []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []Request{}
	for _, c := range b.pending {
		if c.to == d {
			out = append(out, c.req)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt < out[j].ExpiresAt })
	return out
}

// Respond answers a request, from the device it went to only.
func (b *Browser) Respond(d device.ID, id string, r Result) error {
	c, ok := b.take(d, id)
	if !ok {
		return ErrNotFound
	}
	c.answer <- r
	return nil
}

func (b *Browser) take(d device.ID, id string) (*call, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.pending[id]
	if !ok || c.to != d {
		return nil, false
	}
	delete(b.pending, id)
	return c, true
}
