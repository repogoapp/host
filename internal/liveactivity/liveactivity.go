// Package liveactivity keeps a Live Activity on each paired phone for every
// running Claude turn, from its hooks, through the relay's `push.send`. The
// payload carries the work itself (prompt, tools, questions); the relay stores none.
package liveactivity

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/push"
	"github.com/repogo/host/internal/relay"
)

const (
	// Tool calls come several a second; the lock screen gets the latest at
	// most this often. Starts, approvals, questions and endings go at once.
	updateInterval = 5 * time.Second

	// iOS ends a Live Activity after eight hours; its token is dead after that.
	maxActivityAge = 8 * time.Hour

	// The newest tool calls the activity lists, as many as the widget shows.
	recentLimit = 8

	// A settled chat's state is kept this long for a late event, then dropped.
	settledRetention = time.Hour
)

type Driver struct {
	// Labels a chat's agent in the subtitle: "Claude is working".
	agentName func(agent.Kind) string
	// Labels its project as every surface does: the user's name for it, else
	// the repository, else the folder.
	projectName func(cwd string) string
	store     *device.Store
	send      push.Sender
	log       *slog.Logger
	now       func() time.Time
	// Runs a delivery off the notice loop, which must never wait on the relay.
	spawn func(func())

	// Held from snapshot to send, so a later push always carries later state.
	sendMu sync.Mutex

	mu    sync.Mutex
	chats map[string]*chat // host chat id "<agent>:<session>"
}

// New builds the driver that pushes Live Activity updates through send for the
// tokens in store; name labels an agent kind, as in "Claude is working".
func New(store *device.Store, send push.Sender, log *slog.Logger, name func(agent.Kind) string,
	projectName func(cwd string) string) *Driver {
	return &Driver{agentName: name, projectName: projectName, store: store, send: send, log: log, now: time.Now,
		spawn: func(f func()) { go f() }, chats: map[string]*chat{}}
}

// chat is one chat's activity: its current turn, as the lock screen shows it.
type chat struct {
	id        string
	agent     agent.Kind
	cwd       string
	title     string
	status    string // running | completed | failed
	startedAt time.Time
	endedAt   time.Time

	current   *event
	recent    []event
	files     map[string]bool
	additions int
	deletions int

	approval *approval
	question string

	// Peers this turn's activity was started on, so a turn starts at most
	// one activity per phone while the phone reports its token.
	started map[device.ID]bool

	lastPush time.Time
	timer    *time.Timer
}

type approval struct {
	title   string
	turnID  string
	callID  string
	options map[string]string // approval option kind → option id
}

// Run feeds hook notices to the activities until ctx ends.
func (d *Driver) Run(ctx context.Context, bridge *notify.Bridge) {
	bridge.Follow(ctx, func(n notify.Notice) { d.Observe(ctx, n) })
}

// Observe folds one hook notice into its chat's activity and pushes the
// result. Only Claude reports tool calls; Codex's one hook ends a turn.
func (d *Driver) Observe(ctx context.Context, n notify.Notice) {
	id := n.ChatID()
	d.mu.Lock()
	d.prune()
	c := d.chats[id]
	urgent := true
	switch {
	case n.Starts():
		c = d.begin(id, n)
		c.title = headline(n.Prompt, "Agent")

	case n.Ends():
		if c == nil || c.status != "running" {
			d.mu.Unlock()
			return
		}
		c.status = "completed"
		if n.Failed() {
			c.status = "failed"
		}
		c.endedAt = n.At
		c.current, c.approval, c.question = nil, nil, ""

	case n.AsksQuestion():
		c = d.running(id, n)
		c.question = n.Question()

	case n.ToolStarting():
		c = d.running(id, n)
		lbl := labelFor(n.ToolName, n.ToolInput)
		c.upsert(eventFor(n.ToolUseID, lbl, lbl.Title))
		urgent = false

	case n.ToolFinished():
		c = d.running(id, n)
		lbl := labelFor(n.ToolName, n.ToolInput)
		title := completedTitle(lbl.Title)
		if n.ToolFailed() {
			title = lbl.Error
		}
		ev := eventFor(n.ToolUseID, lbl, title)
		if !n.ToolFailed() {
			ev.Additions, ev.Deletions = lineChanges(n.ToolName, n.ToolInput)
			if ev.Additions+ev.Deletions > 0 {
				c.files[cmp.Or(toolPath(n.ToolInput), n.ToolUseID)] = true
				c.additions += ev.Additions
				c.deletions += ev.Deletions
			}
		}
		c.upsert(ev)
		// The tool ran, so whatever was asked of the user has been answered.
		urgent = c.approval != nil || c.question != ""
		c.approval, c.question = nil, ""

	case n.Status == agent.ChatAwaitingApproval:
		c = d.running(id, n)
		if c.approval == nil {
			c.approval = &approval{title: cmp.Or(n.Message, "Permission required")}
		}
		// PermissionRequest names the tool; the Notification that follows it
		// only has Claude's sentence, so it must not replace the label.
		if n.ToolName != "" {
			c.approval.title = labelFor(n.ToolName, n.ToolInput).Title
		}

	case n.Status == agent.ChatAwaitingUser:
		c = d.running(id, n)
		c.question = cmp.Or(n.Question(), n.Message, "Question")

	default:
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	d.flush(ctx, c, urgent)
}

// Approval is told of each prompt this host puts to devices, and nil when it
// is withdrawn; its ids are what the lock screen's buttons answer with.
func (d *Driver) Approval(ctx context.Context, chatID, turnID string, a *agent.Approval) {
	d.mu.Lock()
	c := d.chats[chatID]
	if c == nil || c.status != "running" {
		d.mu.Unlock()
		return
	}
	switch {
	case a == nil:
		c.approval = nil
	case len(a.Questions) > 0:
		c.question = cmp.Or(strings.TrimSpace(a.Questions[0].Text), "Question")
	default:
		options := map[string]string{}
		for _, o := range a.Options {
			if o.Kind != "" && options[o.Kind] == "" {
				options[o.Kind] = o.OptionID
			}
		}
		title := cmp.Or(a.Title, "Permission required")
		if c.approval != nil && c.approval.title != "" {
			// The hook's label names the tool the way the rest of the list does.
			title = c.approval.title
		}
		c.approval = &approval{title: title, turnID: turnID, callID: a.CallID, options: options}
	}
	d.mu.Unlock()
	d.flush(ctx, c, true)
}

// begin resets a chat's activity for a new turn. Caller holds d.mu.
func (d *Driver) begin(id string, n notify.Notice) *chat {
	c := d.chats[id]
	if c != nil && c.timer != nil {
		c.timer.Stop()
	}
	c = &chat{
		id: id, agent: n.Agent, cwd: n.Cwd, status: "running", startedAt: n.At,
		files: map[string]bool{}, started: map[device.ID]bool{},
	}
	d.chats[id] = c
	return c
}

// running is the chat's activity for an event inside a turn, begun here when
// the turn's start was missed, as when the host restarted mid-turn. Caller
// holds d.mu.
func (d *Driver) running(id string, n notify.Notice) *chat {
	if c := d.chats[id]; c != nil && c.status == "running" {
		return c
	}
	c := d.begin(id, n)
	c.title = "Agent"
	return c
}

// prune drops chats settled long ago. Caller holds d.mu.
func (d *Driver) prune() {
	cutoff := d.now().Add(-settledRetention)
	for id, c := range d.chats {
		if c.status != "running" && c.endedAt.Before(cutoff) {
			delete(d.chats, id)
		}
	}
}

func (c *chat) upsert(ev event) {
	c.current = &ev
	for i := range c.recent {
		if c.recent[i].ID == ev.ID {
			c.recent[i] = ev
			return
		}
	}
	c.recent = append(c.recent, ev)
	if len(c.recent) > recentLimit {
		c.recent = c.recent[len(c.recent)-recentLimit:]
	}
}

// flush pushes the chat's activity now, or, for an update that can wait, no
// sooner than updateInterval after the last one.
func (d *Driver) flush(ctx context.Context, c *chat, urgent bool) {
	if !urgent {
		d.mu.Lock()
		wait := c.lastPush.Add(updateInterval).Sub(d.now())
		if wait > 0 {
			if c.timer == nil {
				c.timer = time.AfterFunc(wait, func() { d.push(context.WithoutCancel(ctx), c) })
			}
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()
	}
	d.spawn(func() { d.push(ctx, c) })
}

// push sends the chat's activity as it stands to every paired phone: an
// update where the phone has the activity, a start where it does not yet.
func (d *Driver) push(ctx context.Context, c *chat) {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	d.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if d.chats[c.id] != c {
		// A newer turn has the chat now.
		d.mu.Unlock()
		return
	}
	c.lastPush = d.now()
	hostID := d.store.Identity().ID
	key := c.id + "@" + string(hostID)
	state := c.contentState(d.now(), d.agentName(c.agent), d.projectLabel(c.cwd))
	attributes := map[string]any{
		"name":        state.Title,
		"chatId":      key,
		"workspaceId": projectID(string(hostID), c.cwd),
	}
	running := c.status == "running"
	var starts []device.Peer
	var updates []device.Peer
	for _, peer := range d.store.Peers() {
		if act, ok := peer.Activities[key]; ok && d.now().Sub(time.UnixMilli(act.At)) < maxActivityAge {
			updates = append(updates, peer)
			continue
		}
		if running && peer.PushToStart != nil && !c.started[peer.ID] {
			c.started[peer.ID] = true
			starts = append(starts, peer)
		}
	}
	d.mu.Unlock()

	now := d.now().Unix()
	for _, peer := range updates {
		act := peer.Activities[key]
		payload, _ := json.Marshal(map[string]any{"aps": map[string]any{
			"timestamp": now, "event": "update", "content-state": state,
		}})
		if d.deliver(ctx, act, payload) {
			continue
		}
		// Apple says the activity is gone: dismissed, or ended by iOS.
		if err := d.store.ForgetPush(peer.ID, key); err != nil {
			d.log.Warn("liveactivity: could not forget token", "device", peer.ID, "err", err)
		}
	}
	// iOS drops a start without an alert, and only issues the activity an
	// update token when the start asks for one (input-push-token).
	alert := map[string]any{"title": state.Title, "body": d.agentName(c.agent) + " is working"}
	for _, peer := range starts {
		payload, _ := json.Marshal(map[string]any{"aps": map[string]any{
			"timestamp": now, "event": "start", "content-state": state,
			"attributes-type": "LiveActivityAttributes", "attributes": attributes,
			"alert": alert, "input-push-token": 1,
		}})
		if d.deliver(ctx, *peer.PushToStart, payload) {
			continue
		}
		if err := d.store.ForgetPushToStart(peer.ID); err != nil {
			d.log.Warn("liveactivity: could not forget start token", "device", peer.ID, "err", err)
		}
	}
}

// deliver sends one Live Activity push; false means the token is dead. Always
// high priority: Apple holds low-priority updates while the phone is locked.
func (d *Driver) deliver(ctx context.Context, to device.PushTarget, payload []byte) bool {
	return !push.Send(ctx, d.send, d.log, relay.PushRequest{
		Token: to.Token, Environment: to.Environment, Payload: payload,
		PushType: relay.PushTypeLiveActivity, Priority: 10,
	})
}

// contentState is the activity as the widget decodes it
// (`LiveActivityAttributes.ContentState`). Caller holds d.mu.
func (c *chat) contentState(now time.Time, name, project string) contentState {
	s := contentState{
		CurrentEvent:            c.current,
		StartTimeInMilliseconds: float64(c.startedAt.UnixMilli()),
		Status:                  c.status,
		Additions:               c.additions,
		Deletions:               c.deletions,
		FileCount:               len(c.files),
		RecentEvents:            append([]event{}, c.recent...),
		ProjectLabel:            project,
		Subtitle:                name + " is working",
		Title:                   c.title,
		AgentID:                 string(c.agent),
	}
	if c.status != "running" {
		// The timer stops at the turn's length instead of ticking on.
		s.DurationMs = max(c.endedAt.Sub(c.startedAt).Milliseconds(), 0)
	}
	if a := c.approval; a != nil {
		s.AwaitingApproval = true
		s.ApprovalTitle = a.title
		s.ApprovalID, s.TurnID, s.ApprovalOptions = a.callID, a.turnID, a.options
		s.ApprovalAgent = string(c.agent)
		s.PendingApprovalCount = 1
	}
	if c.question != "" {
		s.AwaitingUserInput = true
		s.UserInputQuestion = c.question
		s.UserInputAgent = string(c.agent)
		// While the agent waits on the user, the question is the headline.
		s.Title = headline(c.question, c.title)
	}
	return s
}

type contentState struct {
	CurrentEvent            *event  `json:"currentEvent"`
	StartTimeInMilliseconds float64 `json:"startTimeInMilliseconds,omitempty"`
	DurationMs              int64   `json:"durationMs,omitempty"`
	Status                  string  `json:"status"`
	Additions               int     `json:"additions,omitempty"`
	Deletions               int     `json:"deletions,omitempty"`
	FileCount               int     `json:"fileCount,omitempty"`
	RecentEvents            []event `json:"recentEvents"`
	ProjectLabel            string  `json:"workspaceLabel,omitempty"`
	Subtitle                string  `json:"subtitle,omitempty"`
	Title                   string  `json:"title,omitempty"`
	AgentID                 string  `json:"agentId,omitempty"`

	AwaitingApproval     bool              `json:"awaitingApproval,omitempty"`
	ApprovalID           string            `json:"approvalId,omitempty"`
	ApprovalTitle        string            `json:"approvalTitle,omitempty"`
	ApprovalAgent        string            `json:"approvalAgent,omitempty"`
	TurnID               string            `json:"turnId,omitempty"`
	ApprovalOptions      map[string]string `json:"approvalOptions,omitempty"`
	PendingApprovalCount int               `json:"pendingApprovalCount,omitempty"`

	AwaitingUserInput bool   `json:"awaitingUserInput,omitempty"`
	UserInputQuestion string `json:"userInputQuestion,omitempty"`
	UserInputAgent    string `json:"userInputAgent,omitempty"`
}

// event is one row of the activity's tool list (`EventInfo`).
type event struct {
	ID       string    `json:"id"`
	PartType string    `json:"partType"`
	Icon     eventIcon `json:"icon"`
	Label    struct {
		Title    string `json:"title"`
		Subtitle string `json:"subtitle"`
	} `json:"label"`
	Timeline struct {
		Active    string `json:"active"`
		Completed string `json:"completed"`
	} `json:"timeline"`
	Additions int `json:"additions,omitempty"`
	Deletions int `json:"deletions,omitempty"`
}

type eventIcon struct {
	LiveActivity icon `json:"liveActivity"`
}

func eventFor(id string, lbl label, title string) event {
	var ev event
	ev.ID, ev.PartType = id, "PART_TYPE_TOOL"
	ev.Icon.LiveActivity = lbl.Icon
	ev.Label.Title, ev.Label.Subtitle = title, lbl.Subtitle
	ev.Timeline.Active, ev.Timeline.Completed = lbl.Active, lbl.Completed
	return ev
}

func toolPath(raw json.RawMessage) string {
	var in struct {
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(raw, &in)
	return in.FilePath
}

// headline is a one-line title, as v1 cut the prompt.
func headline(s, fallback string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return fallback
	}
	if r := []rune(s); len(r) > 220 {
		return strings.TrimSpace(string(r[:220])) + "…"
	}
	return s
}

func (d *Driver) projectLabel(cwd string) string {
	if cwd == "" {
		return ""
	}
	return d.projectName(cwd)
}

// projectID is the app's `ProjectRef.fileKey` for a folder on this
// host, which keys the project icon the app mirrors for the widget.
func projectID(hostID, cwd string) string {
	if cwd == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(hostID + "\x00" + cwd))
	return hex.EncodeToString(sum[:])
}
