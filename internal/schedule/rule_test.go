package schedule

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/store"
)

func TestNext(t *testing.T) {
	cases := []struct {
		name, frequency, zone, after, want string
		hour, minute, weekday              int
	}{
		{"hourly", hourly, "UTC", "2026-09-07T09:15:00Z", "2026-09-07T10:15:00Z", 0, 15, 1},
		{"daily", daily, "America/New_York", "2026-09-07T12:59:00Z", "2026-09-07T13:00:00Z", 9, 0, 1},
		{"weekly", weekly, "UTC", "2026-09-07T09:00:00Z", "2026-09-14T09:00:00Z", 9, 0, 2},
		{"a spring-forward gap is skipped", daily, "America/New_York", "2026-03-08T06:59:00Z", "2026-03-09T06:30:00Z", 2, 30, 1},
		{"a fall-back time runs once", daily, "America/New_York", "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z", 1, 30, 1},
		// 2:30 does not exist on Sunday March 8, so the next is two weeks out.
		{"a weekly time in the gap waits two weeks", weekly, "America/New_York", "2026-03-01T07:30:00Z", "2026-03-15T06:30:00Z", 2, 30, 1},
		{"hourly runs in both fall-back hours", hourly, "America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", 0, 30, 1},
		{"the minute holding after is not due", daily, "UTC", "2026-09-07T09:00:30Z", "2026-09-08T09:00:00Z", 9, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := store.Schedule{Frequency: tc.frequency, Timezone: tc.zone, Hour: tc.hour, Minute: tc.minute, Weekday: tc.weekday}
			after, _ := time.Parse(time.RFC3339, tc.after)
			if got := next(sc, after).Format(time.RFC3339); got != tc.want {
				t.Fatalf("next = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCheckRefusesWhatCouldNeverRun(t *testing.T) {
	good := store.Schedule{Title: "Review", Prompt: "Review the changes", Path: "/repo", Agent: "claude",
		Frequency: daily, Hour: 9, Weekday: 2, Timezone: "America/New_York", Enabled: true}
	if err := check(good, agents); err != nil {
		t.Fatalf("a good schedule was refused: %v", err)
	}
	for name, change := range map[string]func(*store.Schedule){
		"empty title":     func(s *store.Schedule) { s.Title = " " },
		"long title":      func(s *store.Schedule) { s.Title = string(make([]rune, titleMaxRunes+1)) },
		"empty prompt":    func(s *store.Schedule) { s.Prompt = "" },
		"unknown agent":   func(s *store.Schedule) { s.Agent = "other" },
		"unknown cadence": func(s *store.Schedule) { s.Frequency = "monthly" },
		"hour":            func(s *store.Schedule) { s.Hour = 24 },
		"minute":          func(s *store.Schedule) { s.Minute = -1 },
		"weekday":         func(s *store.Schedule) { s.Weekday = 0 },
		"timezone":        func(s *store.Schedule) { s.Timezone = "Mars/Base" },
		"no timezone":     func(s *store.Schedule) { s.Timezone = "" },
	} {
		sc := good
		change(&sc)
		if check(sc, agents) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
