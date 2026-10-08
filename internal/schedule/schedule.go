// Package schedule runs the user's schedules: each is a prompt this host
// starts as a new chat at a set time. The rows live in state.db (store).
package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chat"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/store"
)

// late is how far behind a due time may be and still run; anything later,
// as when the machine slept through it, is skipped.
const late = 2 * time.Minute

// Starter starts a chat: chat.Service, narrowed to what a run needs.
type Starter interface {
	Start(where chat.Start, t chat.Turn) (agent.TurnStatus, error)
}

// Deps is everything the scheduler touches. Every field is required.
type Deps struct {
	Self   device.ID
	Store  *store.Store
	Chats  Starter
	Files  files.Container
	Agents []agent.Kind
	// Changed sends the list to every paired device.
	Changed func(Changed)
	Log     *slog.Logger
}

// Scheduler keeps the schedules and starts each run when it is due.
type Scheduler struct {
	d   Deps
	now func() time.Time
	// mu orders saves, deletes and the tick's claims, and the events each sends.
	mu sync.Mutex
}

// New refuses Deps with a field unset rather than failing on first use.
func New(d Deps) (*Scheduler, error) {
	var missing []string
	for _, f := range []struct {
		name  string
		unset bool
	}{
		{"Self", d.Self == ""}, {"Store", d.Store == nil}, {"Chats", d.Chats == nil}, {"Files", d.Files == nil},
		{"Agents", len(d.Agents) == 0}, {"Changed", d.Changed == nil}, {"Log", d.Log == nil},
	} {
		if f.unset {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("schedule: missing %s", strings.Join(missing, ", "))
	}
	return &Scheduler{d: d, now: time.Now}, nil
}

// List is every schedule on this host with its next run.
func (s *Scheduler) List() (Changed, error) {
	saved, err := s.d.Store.Schedules()
	if err != nil {
		return Changed{}, err
	}
	now := s.now()
	out := Changed{Schedules: make([]Row, 0, len(saved))}
	for _, sc := range saved {
		out.Schedules = append(out.Schedules, s.row(sc, now))
	}
	return out, nil
}

// Save checks and keeps a schedule, making it when its id is empty.
func (s *Scheduler) Save(sc store.Schedule) (Row, error) {
	if err := check(sc, s.d.Agents); err != nil {
		return Row{}, err
	}
	path, err := s.d.Files.Contain(sc.Path)
	if err != nil {
		return Row{}, err
	}
	sc.Path = path

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	saved, err := s.d.Store.SaveSchedule(sc, now)
	if err != nil {
		return Row{}, err
	}
	s.announce()
	return s.row(store.SavedSchedule{Schedule: saved, SavedAt: now}, now), nil
}

// Delete removes a schedule; a run it already started goes on as a chat.
func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.d.Store.DeleteSchedule(id); err != nil {
		return err
	}
	s.announce()
	return nil
}

// Run ticks at start and then on every minute, the only times a schedule is
// due, until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		s.tick(ctx, s.now())
		now := s.now()
		timer := time.NewTimer(now.Truncate(time.Minute).Add(time.Minute).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// tick starts every schedule due within the last two minutes, once.
func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.d.Store.Schedules()
	if err != nil {
		s.d.Log.Error("reading schedules failed", "err", err)
		return
	}
	ran := false
	for _, sc := range saved {
		if ctx.Err() != nil {
			break
		}
		if !sc.Enabled {
			continue
		}
		// Searching from two minutes back skips every older time unrun.
		due := next(sc.Schedule, latest(sc.SavedAt, sc.LastDueAt, now.Add(-late-time.Millisecond)))
		if due.IsZero() || due.After(now) {
			continue
		}
		claimed, err := s.d.Store.ClaimScheduleRun(sc.ID, sc.SavedAt, due)
		if err != nil {
			s.d.Log.Error("claiming a schedule run failed", "schedule", sc.ID, "err", err)
			continue
		}
		if !claimed {
			continue
		}
		ran = true
		_, err = s.d.Chats.Start(
			chat.Start{Path: sc.Path, Agent: sc.Agent, Title: title(sc.Schedule, due)},
			chat.Turn{Prompt: sc.Prompt, Config: sc.Config},
		)
		if err != nil {
			s.d.Log.Error("starting a scheduled chat failed", "schedule", sc.ID, "err", err)
			continue
		}
		s.d.Log.Info("started a scheduled chat", "schedule", sc.ID, "due", due)
	}
	if ran {
		s.announce()
	}
}

// announce sends the list as it now stands; the caller holds mu, so events
// go out in the order the writes landed.
func (s *Scheduler) announce() {
	list, err := s.List()
	if err != nil {
		s.d.Log.Error("reading schedules failed", "err", err)
		return
	}
	s.d.Changed(list)
}

func (s *Scheduler) row(sc store.SavedSchedule, now time.Time) Row {
	out := Row{Schedule: sc.Schedule, HostID: string(s.d.Self)}
	if sc.Enabled {
		if due := next(sc.Schedule, latest(sc.SavedAt, sc.LastDueAt, now)); !due.IsZero() {
			out.NextRunAt = due.UnixMilli()
		}
	}
	return out
}

func latest(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}
