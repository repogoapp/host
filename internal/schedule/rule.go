package schedule

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/store"
)

const (
	// A run's chat is titled "<title> · Oct 7, 9:00 AM", within chat's 120.
	titleMaxRunes  = 100
	promptMaxRunes = 32_000
	// A weekly time skipped by a spring-forward day is next due 14 days on.
	horizon = 15 * 24 * time.Hour
)

const (
	hourly = "hourly"
	daily  = "daily"
	weekly = "weekly"
)

// next is the schedule's first due time strictly after the minute holding
// after, or zero when there is none. A local time a spring-forward day skips
// does not exist, so it is never due.
func next(sc store.Schedule, after time.Time) time.Time {
	loc, err := time.LoadLocation(sc.Timezone)
	if err != nil {
		return time.Time{}
	}
	start := after.Truncate(time.Minute).Add(time.Minute)
	for candidate := start; candidate.Before(start.Add(horizon)); candidate = candidate.Add(time.Minute) {
		local := candidate.In(loc)
		if local.Minute() != sc.Minute {
			continue
		}
		if sc.Frequency == hourly {
			return candidate.UTC()
		}
		if local.Hour() != sc.Hour {
			continue
		}
		if sc.Frequency == weekly && int(local.Weekday())+1 != sc.Weekday {
			continue
		}
		if repeated(candidate, loc) {
			continue
		}
		return candidate.UTC()
	}
	return time.Time{}
}

// repeated reports whether t's wall time already happened earlier the same
// day, as on a fall-back day; a daily or weekly time runs on the first.
func repeated(t time.Time, loc *time.Location) bool {
	wall := t.In(loc).Format("2006-01-02 15:04")
	for _, back := range []time.Duration{30 * time.Minute, time.Hour} {
		if t.Add(-back).In(loc).Format("2006-01-02 15:04") == wall {
			return true
		}
	}
	return false
}

// title names a run's chat after its schedule and when it was due.
func title(sc store.Schedule, due time.Time) string {
	loc, err := time.LoadLocation(sc.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return sc.Title + " · " + due.In(loc).Format("Jan 2, 3:04 PM")
}

// check refuses a schedule that could never run as written.
func check(sc store.Schedule, agents []agent.Kind) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{errkind.ErrInvalid}, args...)...)
	}
	switch {
	case strings.TrimSpace(sc.Title) == "":
		return invalid("the title is empty")
	case utf8.RuneCountInString(sc.Title) > titleMaxRunes:
		return invalid("the title is over %d characters", titleMaxRunes)
	case strings.TrimSpace(sc.Prompt) == "":
		return invalid("the prompt is empty")
	case utf8.RuneCountInString(sc.Prompt) > promptMaxRunes:
		return invalid("the prompt is over %d characters", promptMaxRunes)
	case !slices.Contains(agents, agent.Kind(sc.Agent)):
		return invalid("this host has no agent %q", sc.Agent)
	case !slices.Contains([]string{hourly, daily, weekly}, sc.Frequency):
		return invalid("frequency %q is not hourly, daily or weekly", sc.Frequency)
	case sc.Hour < 0 || sc.Hour > 23:
		return invalid("hour %d is not 0 to 23", sc.Hour)
	case sc.Minute < 0 || sc.Minute > 59:
		return invalid("minute %d is not 0 to 59", sc.Minute)
	case sc.Weekday < 1 || sc.Weekday > 7:
		return invalid("weekday %d is not 1 to 7", sc.Weekday)
	}
	if _, err := time.LoadLocation(sc.Timezone); err != nil || sc.Timezone == "" {
		return invalid("unknown timezone %q", sc.Timezone)
	}
	return nil
}
