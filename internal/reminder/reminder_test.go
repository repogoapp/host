package reminder

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/store"
)

type fixture struct {
	reminders *Reminders
	alerts    []Alert
	clock     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{clock: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	f.reminders, err = Open(Deps{
		Store: db, Alert: func(_ context.Context, a Alert) { f.alerts = append(f.alerts, a) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.reminders.now = func() time.Time { return f.clock }
	return f
}

func (f *fixture) create(t *testing.T, n New) store.Reminder {
	t.Helper()
	r, err := f.reminders.Create(n, "claude:chat-1")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) tick(t *testing.T, after time.Duration) {
	t.Helper()
	f.reminders.Tick(t.Context(), f.clock.Add(after))
}

func TestAReminderFiresOnceWhenDue(t *testing.T) {
	f := newFixture(t)
	r := f.create(t, New{Text: " Check the deploy ", In: "45m", ChatID: "claude:chat-1"})
	if r.Text != "Check the deploy" || r.DueAt != f.clock.Add(45*time.Minute).UnixMilli() || r.SetBy != "claude:chat-1" {
		t.Fatalf("saved %+v", r)
	}
	f.tick(t, 44*time.Minute)
	if len(f.alerts) != 0 {
		t.Fatalf("fired early: %+v", f.alerts)
	}
	f.tick(t, 45*time.Minute)
	f.tick(t, 46*time.Minute)
	if len(f.alerts) != 1 || f.alerts[0].Text != "Check the deploy" || f.alerts[0].ChatID != "claude:chat-1" || f.alerts[0].Late != 0 {
		t.Fatalf("alerts %+v", f.alerts)
	}
	if left, _ := f.reminders.List(""); len(left) != 0 {
		t.Fatalf("a fired reminder stays: %+v", left)
	}
}

func TestOneTheMachineSleptThroughFiresLateAndSaysSo(t *testing.T) {
	f := newFixture(t)
	f.create(t, New{Text: "Stand-up", In: "1h"})
	f.tick(t, time.Hour+12*time.Minute)
	if len(f.alerts) != 1 || f.alerts[0].Late != 12*time.Minute {
		t.Fatalf("alerts %+v", f.alerts)
	}
}

func TestATickFiresEveryDueReminderAndNoLaterOne(t *testing.T) {
	f := newFixture(t)
	f.create(t, New{Text: "b", In: "2h"})
	f.create(t, New{Text: "a", In: "1h"})
	f.create(t, New{Text: "c", In: "3h"})
	f.tick(t, 2*time.Hour)
	if len(f.alerts) != 2 || f.alerts[0].Text != "a" || f.alerts[1].Text != "b" {
		t.Fatalf("alerts %+v", f.alerts)
	}
	left, _ := f.reminders.List("")
	if len(left) != 1 || left[0].Text != "c" {
		t.Fatalf("left %+v", left)
	}
}

func TestADeletedReminderNeverFires(t *testing.T) {
	f := newFixture(t)
	r := f.create(t, New{Text: "x", In: "5m"})
	if err := f.reminders.Delete(r.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.reminders.Delete(r.ID); !errors.Is(err, store.ErrNoReminder) {
		t.Fatalf("deleting twice: %v", err)
	}
	f.tick(t, time.Hour)
	if len(f.alerts) != 0 {
		t.Fatalf("alerts %+v", f.alerts)
	}
}

func TestWhenIsInOrDueAtFromAMinuteToAYear(t *testing.T) {
	f := newFixture(t)
	if r := f.create(t, New{Text: "x", In: "3d"}); r.DueAt != f.clock.Add(72*time.Hour).UnixMilli() {
		t.Fatalf("3d is %v", time.UnixMilli(r.DueAt))
	}
	at := f.clock.Add(90 * time.Minute).UnixMilli()
	if r := f.create(t, New{Text: "x", DueAt: at}); r.DueAt != at {
		t.Fatalf("due_at %v", r.DueAt)
	}
	for _, n := range []New{
		{Text: "x"},
		{Text: "x", In: "1h", DueAt: at},
		{Text: "x", In: "soon"},
		{Text: "x", In: "0d"},
		{Text: "x", In: "30s"},
		{Text: "x", In: "-1h"},
		{Text: "x", In: "400d"},
		{Text: "x", DueAt: f.clock.Add(-time.Hour).UnixMilli()},
	} {
		if _, err := f.reminders.Create(n, "user"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", n, err)
		}
	}
}

func TestTextAndPendingPerChatAreBounded(t *testing.T) {
	f := newFixture(t)
	for _, text := range []string{"", "   ", strings.Repeat("a", 201)} {
		if _, err := f.reminders.Create(New{Text: text, In: "1h"}, "user"); !errors.Is(err, ErrInvalid) {
			t.Errorf("text %q: %v", text, err)
		}
	}
	for range 10 {
		f.create(t, New{Text: "x", In: "1h", ChatID: "claude:a"})
	}
	if _, err := f.reminders.Create(New{Text: "x", In: "1h", ChatID: "claude:a"}, "user"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an 11th on one chat: %v", err)
	}
	f.create(t, New{Text: "x", In: "1h", ChatID: "claude:b"})
}

func TestListNarrowsToAProject(t *testing.T) {
	f := newFixture(t)
	f.create(t, New{Text: "a", In: "1h", Path: "/p/web"})
	f.create(t, New{Text: "b", In: "2h", Path: "/p/api"})
	rows, err := f.reminders.List("/p/web")
	if err != nil || len(rows) != 1 || rows[0].Text != "a" {
		t.Fatalf("rows %+v, %v", rows, err)
	}
	if all, _ := f.reminders.List(""); len(all) != 2 {
		t.Fatalf("all %+v", all)
	}
}
