package schedule

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chat"
	"github.com/repogo/host/internal/store"
)

var agents = []agent.Kind{"claude", "codex"}

type starts struct {
	mu   sync.Mutex
	got  []chat.Start
	fail error
}

func (s *starts) Start(where chat.Start, _ chat.Turn) (agent.TurnStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, where)
	return agent.TurnStatus{}, s.fail
}

type anyPath struct{}

func (anyPath) Contain(path string) (string, error) { return path, nil }

type fixture struct {
	scheduler *Scheduler
	db        *store.Store
	starts    *starts
	events    []Changed
	clock     time.Time
}

func newFixture(t *testing.T, at string) *fixture {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{db: db, starts: &starts{}}
	f.clock, _ = time.Parse(time.RFC3339, at)
	f.scheduler, err = New(Deps{
		Self: "host-1", Store: db, Chats: f.starts, Files: anyPath{}, Agents: agents,
		Changed: func(c Changed) { f.events = append(f.events, c) },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler.now = func() time.Time { return f.clock }
	return f
}

func (f *fixture) at(t *testing.T, at string) time.Time {
	t.Helper()
	f.clock, _ = time.Parse(time.RFC3339, at)
	return f.clock
}

func (f *fixture) tick(t *testing.T, at string) {
	t.Helper()
	f.scheduler.tick(t.Context(), f.at(t, at))
}

func dailyAtNine() store.Schedule {
	return store.Schedule{Title: "Review", Prompt: "Review the changes", Path: "/repo", Agent: "claude",
		Config:    agent.TurnConfig{PermissionMode: agent.PermissionAutoAcceptEdits},
		Frequency: daily, Hour: 9, Weekday: 1, Timezone: "UTC", Enabled: true}
}

func TestADueScheduleStartsOneChatOnce(t *testing.T) {
	f := newFixture(t, "2026-10-07T08:00:00Z")
	row, err := f.scheduler.Save(dailyAtNine())
	if err != nil {
		t.Fatal(err)
	}
	if row.ID == "" || row.HostID != "host-1" || row.NextRunAt != f.at(t, "2026-10-07T09:00:00Z").UnixMilli() {
		t.Fatalf("saved row = %+v", row)
	}

	f.tick(t, "2026-10-07T08:59:00Z")
	if len(f.starts.got) != 0 {
		t.Fatalf("started before it was due: %+v", f.starts.got)
	}
	f.tick(t, "2026-10-07T09:00:00Z")
	f.tick(t, "2026-10-07T09:01:00Z")
	if len(f.starts.got) != 1 {
		t.Fatalf("starts = %d, want 1", len(f.starts.got))
	}
	if got := f.starts.got[0]; got.Path != "/repo" || got.Agent != "claude" || got.Title != "Review · Oct 7, 9:00 AM" {
		t.Fatalf("start = %+v", got)
	}
	last := f.events[len(f.events)-1].Schedules[0]
	if last.NextRunAt != f.at(t, "2026-10-08T09:00:00Z").UnixMilli() {
		t.Fatalf("next run after the run = %d", last.NextRunAt)
	}
}

func TestARestartWithinTwoMinutesRunsTheTimeAndLaterSkipsIt(t *testing.T) {
	f := newFixture(t, "2026-10-07T08:00:00Z")
	if _, err := f.scheduler.Save(dailyAtNine()); err != nil {
		t.Fatal(err)
	}
	f.tick(t, "2026-10-07T09:02:00Z")
	if len(f.starts.got) != 1 {
		t.Fatalf("two minutes late: starts = %d, want 1", len(f.starts.got))
	}

	g := newFixture(t, "2026-10-07T08:00:00Z")
	if _, err := g.scheduler.Save(dailyAtNine()); err != nil {
		t.Fatal(err)
	}
	g.tick(t, "2026-10-07T09:03:00Z")
	g.tick(t, "2026-10-10T08:30:00Z")
	if len(g.starts.got) != 0 {
		t.Fatalf("missed times ran: %+v", g.starts.got)
	}
	g.tick(t, "2026-10-10T09:00:00Z")
	if len(g.starts.got) != 1 {
		t.Fatalf("the next time after a skip: starts = %d, want 1", len(g.starts.got))
	}
}

func TestAnEditOrDisableAfterTheReadLosesTheClaim(t *testing.T) {
	f := newFixture(t, "2026-10-07T08:00:00Z")
	row, err := f.scheduler.Save(dailyAtNine())
	if err != nil {
		t.Fatal(err)
	}
	read, err := f.db.Schedules()
	if err != nil {
		t.Fatal(err)
	}
	due := f.at(t, "2026-10-07T09:00:00Z")

	edited := row.Schedule
	edited.Prompt = "Something else"
	f.clock = f.clock.Add(time.Second)
	if _, err := f.scheduler.Save(edited); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.db.ClaimScheduleRun(row.ID, read[0].SavedAt, due); err != nil || ok {
		t.Fatalf("claim after an edit = %v, %v; want lost", ok, err)
	}

	read, _ = f.db.Schedules()
	edited.Enabled = false
	f.clock = f.clock.Add(time.Second)
	if _, err := f.scheduler.Save(edited); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.db.ClaimScheduleRun(row.ID, read[0].SavedAt, due); ok {
		t.Fatal("a disabled schedule was claimed")
	}
	f.tick(t, "2026-10-07T09:00:00Z")
	if len(f.starts.got) != 0 {
		t.Fatalf("a disabled schedule started: %+v", f.starts.got)
	}
}

func TestAnEditDoesNotFireATimeAlreadyPassed(t *testing.T) {
	f := newFixture(t, "2026-10-07T09:00:30Z")
	if _, err := f.scheduler.Save(dailyAtNine()); err != nil {
		t.Fatal(err)
	}
	f.tick(t, "2026-10-07T09:01:00Z")
	if len(f.starts.got) != 0 {
		t.Fatalf("a time before the save ran: %+v", f.starts.got)
	}
}

func TestAFailedStartIsNotRetried(t *testing.T) {
	f := newFixture(t, "2026-10-07T08:00:00Z")
	f.starts.fail = errors.New("agent missing")
	if _, err := f.scheduler.Save(dailyAtNine()); err != nil {
		t.Fatal(err)
	}
	f.tick(t, "2026-10-07T09:00:00Z")
	f.tick(t, "2026-10-07T09:01:00Z")
	if len(f.starts.got) != 1 {
		t.Fatalf("starts = %d, want 1", len(f.starts.got))
	}
}

func TestDeleteAndListAnnounce(t *testing.T) {
	f := newFixture(t, "2026-10-07T08:00:00Z")
	row, err := f.scheduler.Save(dailyAtNine())
	if err != nil {
		t.Fatal(err)
	}
	off := row.Schedule
	off.Enabled = false
	if row, err = f.scheduler.Save(off); err != nil || row.NextRunAt != 0 {
		t.Fatalf("a disabled row = %+v, %v; want no next run", row, err)
	}
	if err := f.scheduler.Delete(row.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.Delete(row.ID); !errors.Is(err, store.ErrNoSchedule) {
		t.Fatalf("second delete = %v", err)
	}
	if len(f.events) != 3 || len(f.events[2].Schedules) != 0 {
		t.Fatalf("events = %+v", f.events)
	}
	list, err := f.scheduler.List()
	if err != nil || len(list.Schedules) != 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
}
