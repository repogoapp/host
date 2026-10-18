// Package reminder keeps one-shot reminders, which agents set through
// RepoGo's MCP, and alerts the phones as each comes due. The rows live in
// state.db (store).
package reminder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/store"
)

// Limits on what a reminder may be, so an agent can't flood the phones.
const (
	maxText    = 200
	maxPerChat = 10
	soonest    = time.Minute
	latest     = 366 * 24 * time.Hour
	// late is how far past its time a reminder fires before its alert says
	// so, as after the machine slept through it.
	late = 2 * time.Minute
)

// ErrInvalid is a reminder this host won't keep: no text, a time out of
// range, or too many pending on one chat.
var ErrInvalid = errkind.New(errkind.Invalid, "reminder")

// Alert is what the phones are told when a reminder fires.
type Alert struct {
	ID     string
	Text   string
	ChatID string
	// How far past its time it fired; zero when on time.
	Late time.Duration
}

// Deps is everything the reminders touch. Every field is required.
type Deps struct {
	Store *store.Store
	Alert func(context.Context, Alert)
	Log   *slog.Logger
}

// New is what a caller sets: the text and when, as `in` (45m, 2h, 3d) or
// `due_at` (milliseconds), and the chat and project it's about.
type New struct {
	Text  string `json:"text" doc:"What the notification says, up to 200 characters"`
	In    string `json:"in,omitempty" doc:"How long from now, such as 45m, 2h or 3d; or give due_at"`
	DueAt int64  `json:"due_at,omitempty" doc:"When, in Unix milliseconds; or give in"`
	// The chat a tap opens; an agent's own chat when it leaves this out.
	ChatID string `json:"chat_id,omitempty" doc:"The chat a tap on the notification opens; this chat when left out"`
	Path   string `json:"path,omitempty" doc:"The project's folder"`
}

// Reminders keeps the reminders and fires each when it comes due.
type Reminders struct {
	d   Deps
	now func() time.Time
	// mu orders creates (and their per-chat count) and the tick's claims.
	mu sync.Mutex
}

// Open refuses Deps with a field unset rather than failing on first use.
func Open(d Deps) (*Reminders, error) {
	if d.Store == nil || d.Alert == nil || d.Log == nil {
		return nil, errors.New("reminder: Store, Alert and Log are required")
	}
	return &Reminders{d: d, now: time.Now}, nil
}

// List is every pending reminder, soonest first; under path's project only
// when path is set.
func (r *Reminders) List(path string) ([]store.Reminder, error) {
	all, err := r.d.Store.Reminders()
	if err != nil || path == "" {
		return all, err
	}
	out := []store.Reminder{}
	for _, row := range all {
		if row.Path == path {
			out = append(out, row)
		}
	}
	return out, nil
}

// Create checks n and saves it, set by setBy: "user", or the agent's chat.
func (r *Reminders) Create(n New, setBy string) (store.Reminder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	text := strings.TrimSpace(n.Text)
	if text == "" || utf8.RuneCountInString(text) > maxText {
		return store.Reminder{}, fmt.Errorf("%w: text is 1 to %d characters", ErrInvalid, maxText)
	}
	due, err := dueAt(n, now)
	if err != nil {
		return store.Reminder{}, err
	}
	if n.ChatID != "" {
		pending, err := r.d.Store.Reminders()
		if err != nil {
			return store.Reminder{}, err
		}
		count := 0
		for _, row := range pending {
			if row.ChatID == n.ChatID {
				count++
			}
		}
		if count >= maxPerChat {
			return store.Reminder{}, fmt.Errorf("%w: a chat has at most %d pending reminders", ErrInvalid, maxPerChat)
		}
	}
	return r.d.Store.AddReminder(store.Reminder{
		Text: text, DueAt: due.UnixMilli(), ChatID: n.ChatID, Path: n.Path, SetBy: setBy,
		CreatedAt: now.UnixMilli(),
	})
}

// Delete removes a pending reminder.
func (r *Reminders) Delete(id string) error {
	return r.d.Store.DeleteReminder(id)
}

// Tick fires every reminder due at or before now, each once: one the machine
// slept through fires now, its alert saying how late.
func (r *Reminders) Tick(ctx context.Context, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, err := r.d.Store.Reminders()
	if err != nil {
		r.d.Log.Error("reading reminders failed", "err", err)
		return
	}
	for _, row := range pending {
		if ctx.Err() != nil || time.UnixMilli(row.DueAt).After(now) {
			// Soonest first: the rest are later still.
			break
		}
		// Deleting first is the claim: a fired reminder never fires again.
		if err := r.d.Store.DeleteReminder(row.ID); err != nil {
			continue
		}
		alert := Alert{ID: row.ID, Text: row.Text, ChatID: row.ChatID}
		if behind := now.Sub(time.UnixMilli(row.DueAt)); behind > late {
			alert.Late = behind
		}
		r.d.Alert(ctx, alert)
		r.d.Log.Info("fired a reminder", "reminder", row.ID, "late", alert.Late)
	}
}

// dueAt is when n is due: exactly one of `in` and `due_at`, from a minute
// to a year out.
func dueAt(n New, now time.Time) (time.Time, error) {
	var due time.Time
	switch {
	case n.In != "" && n.DueAt != 0:
		return time.Time{}, fmt.Errorf("%w: give in or due_at, not both", ErrInvalid)
	case n.In != "":
		d, err := duration(n.In)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: in is a duration such as 45m, 2h or 3d", ErrInvalid)
		}
		due = now.Add(d)
	case n.DueAt != 0:
		due = time.UnixMilli(n.DueAt)
	default:
		return time.Time{}, fmt.Errorf("%w: give in or due_at", ErrInvalid)
	}
	if due.Before(now.Add(soonest)) || due.After(now.Add(latest)) {
		return time.Time{}, fmt.Errorf("%w: a reminder is from a minute to a year out", ErrInvalid)
	}
	return due, nil
}

// duration is Go's (45m, 1h30m) plus whole days (3d), which it lacks.
func duration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, errors.New("not a number of days")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
