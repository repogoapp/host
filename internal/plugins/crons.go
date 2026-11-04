package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// What runs unattended is what the user lists in one file of their own,
// ~/.repogo/crons.json: a repo can't schedule itself. Each minute the host
// gives the plugins folder the file's whole set of schedules.

// reconcileEvery is how soon an edit to the file, or a restarted plugins server, takes effect.
const reconcileEvery = time.Minute

// CronEntry is one line of crons.json, naming an environment plugin. A project,
// an absolute path or one starting ~/, is the run's ctx.root; none runs it at
// the home folder.
type CronEntry struct {
	Project  string `json:"project,omitempty"`
	Plugin   string `json:"plugin"`
	Method   string `json:"method"`
	Schedule string `json:"schedule"`
	Timezone string `json:"timezone,omitempty"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// Cron is one entry and how it's going, as the plugins tool shows it. Index is
// its place in the file, -1 for the file itself.
type Cron struct {
	Index    int    `json:"index"`
	Project  string `json:"project,omitempty"`
	Plugin   string `json:"plugin"`
	Method   string `json:"method"`
	Schedule string `json:"schedule"`
	Timezone string `json:"timezone,omitempty"`
	Enabled  bool   `json:"enabled"`
	// Root is the project's checkout the run gets as ctx.root; empty for none.
	Root           string `json:"root,omitempty"`
	Error          string `json:"error,omitempty"`
	NextRunAt      *int64 `json:"nextRunAt,omitempty"`
	LastRunAt      *int64 `json:"lastRunAt,omitempty"`
	LastError      string `json:"lastError,omitempty"`
	LastDurationMs *int64 `json:"lastDurationMs,omitempty"`
	Running        bool   `json:"running,omitempty"`
	Runs           int    `json:"runs,omitempty"`
}

// cronStatus is what a Worker answers for each schedule it was given.
type cronStatus struct {
	Error          *string `json:"error"`
	Timezone       string  `json:"timezone"`
	NextRunAt      *int64  `json:"nextRunAt"`
	LastRunAt      *int64  `json:"lastRunAt"`
	LastError      *string `json:"lastError"`
	LastDurationMs *int64  `json:"lastDurationMs"`
	Running        bool    `json:"running"`
	Runs           int     `json:"runs"`
}

type cronWire struct {
	Root     string `json:"root,omitempty"`
	Plugin   string `json:"plugin"`
	Method   string `json:"method"`
	Schedule string `json:"schedule"`
	Timezone string `json:"timezone,omitempty"`
}

type cronsParams struct {
	Dir     string     `json:"dir"`
	Plugins []string   `json:"plugins"`
	Crons   []cronWire `json:"crons"`
}

var (
	pluginNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	methodNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// RunCrons keeps the schedules in step with crons.json until ctx ends.
func (s *Service) RunCrons(ctx context.Context) {
	if !s.On() || s.cfg.Crons == "" {
		return
	}
	tick := time.NewTicker(reconcileEvery)
	defer tick.Stop()
	for {
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Crons is every entry in crons.json and how it went at the last reconcile.
func (s *Service) Crons() []Cron {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(make([]Cron, 0, len(s.crons)), s.crons...)
}

// setWait bounds the Worker's reply, so one that never answers can't hold up
// the next pass, a removal above all.
const setWait = 30 * time.Second

// reconcile gives the plugins folder the file's schedules, an empty set once
// it names none. A file that can't be read pauses them all, so a broken edit
// can't keep a removed job running.
func (s *Service) reconcile(ctx context.Context) {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()
	crons, params, at := s.planCrons()

	s.mu.Lock()
	held := s.cronsHeld
	s.mu.Unlock()
	// With nothing to set or clear, the plugins server needn't start.
	if len(params.Crons) > 0 || held {
		statuses, failure := s.setCrons(ctx, params)
		// An empty set the Worker never acknowledged is sent again next pass.
		held = len(params.Crons) > 0 || failure != ""
		if failure != "" && len(params.Crons) == 0 {
			crons = append(crons, Cron{Index: -1, Error: "schedules removed from crons.json may still run: " + failure})
		}
		for n, i := range at {
			switch {
			case failure != "":
				crons[i].Error = failure
			case n < len(statuses):
				crons[i].apply(statuses[n])
			}
		}
	}

	s.mu.Lock()
	s.cronsHeld = held
	s.crons = crons
	s.mu.Unlock()
}

// planCrons reads the file into its lines, and the schedules its valid enabled
// ones make, with where each schedule's line sits in the file.
func (s *Service) planCrons() ([]Cron, cronsParams, []int) {
	params := cronsParams{Dir: realOrSame(s.cfg.Global), Plugins: []string{}, Crons: []cronWire{}}
	entries, err := readCronFile(s.cfg.Crons)
	if err != nil {
		return []Cron{{Index: -1, Error: fmt.Sprintf("crons.json can't be read, so every schedule is paused: %v", err)}}, params, nil
	}
	crons := make([]Cron, len(entries))
	var at []int
	seen := map[string]bool{}
	for i, e := range entries {
		c := s.resolveCron(i, e)
		// Two lines for one method would let its runs overlap; a disabled one runs nothing.
		key := c.Root + "|" + e.Plugin + "|" + e.Method
		if c.Error == "" && c.Enabled && seen[key] {
			c.Error = "the same plugin method is scheduled twice for one project; keep one entry"
		}
		crons[i] = c
		if c.Error != "" || !c.Enabled {
			continue
		}
		seen[key] = true
		if !slices.Contains(params.Plugins, e.Plugin) {
			params.Plugins = append(params.Plugins, e.Plugin)
		}
		params.Crons = append(params.Crons, cronWire{Root: c.Root, Plugin: e.Plugin, Method: e.Method, Schedule: e.Schedule, Timezone: e.Timezone})
		at = append(at, i)
	}
	return crons, params, at
}

// setCrons sends the folder its schedules, then reloads it if its code changed:
// in that order, so a restart imports the plugins just named, not the old set.
func (s *Service) setCrons(ctx context.Context, params cronsParams) ([]cronStatus, string) {
	ctx, cancel := context.WithTimeout(ctx, setWait)
	defer cancel()
	srv, err := s.running(ctx)
	if err != nil {
		return nil, err.Error()
	}
	result, pluginErr, err := srv.request(ctx, "crons", params)
	switch {
	case err != nil:
		return nil, err.Error()
	case pluginErr != nil:
		return nil, pluginErr.Message
	}
	if len(params.Crons) > 0 {
		if _, err := s.ready(ctx, Target{Dir: params.Dir}); err != nil {
			return nil, err.Error()
		}
	}
	var statuses []cronStatus
	if err := json.Unmarshal(result, &statuses); err != nil {
		return nil, fmt.Sprintf("plugins: unreadable schedules reply: %v", err)
	}
	return statuses, ""
}

// resolveCron checks an entry's names, its project and its plugin, before
// any of its code loads; the Worker checks the method and the schedule.
func (s *Service) resolveCron(i int, e CronEntry) Cron {
	c := Cron{Index: i, Project: e.Project, Plugin: e.Plugin, Method: e.Method, Schedule: e.Schedule, Timezone: e.Timezone, Enabled: e.Enabled == nil || *e.Enabled}
	switch {
	case !pluginNamePattern.MatchString(e.Plugin):
		c.Error = "plugin is a plugin's folder name: lowercase letters, digits and hyphens"
		return c
	case !methodNamePattern.MatchString(e.Method):
		c.Error = "method is one of the plugin's method names: letters, digits, _ and -"
		return c
	case strings.TrimSpace(e.Schedule) == "":
		c.Error = "schedule is a cron, such as */5 * * * *"
		return c
	}
	if s.cfg.Global == "" {
		c.Error = "this host has no environment plugins folder"
		return c
	}
	root, err := cronRoot(e.Project)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	c.Root = root
	if _, err := os.Stat(filepath.Join(s.cfg.Global, e.Plugin, "app.ts")); err != nil {
		c.Error = fmt.Sprintf("there's no plugin %s in %s", e.Plugin, s.cfg.Global)
	}
	return c
}

// cronRoot is the checkout a line's project names, by its real path so two
// spellings of one folder are one; "" for a line without one.
func cronRoot(project string) (string, error) {
	if project == "" {
		return "", nil
	}
	path := project
	if rest, ok := strings.CutPrefix(project, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("project is an absolute path, or one starting ~/")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("project %s doesn't exist", project)
	}
	return checkout(real), nil
}

// realOrSame is a path with its symlinks resolved, or as it is when it doesn't exist yet.
func realOrSame(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// readCronFile is the file's entries; a missing file is none.
func readCronFile(path string) ([]CronEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var entries []CronEntry
	if err := decoder.Decode(&entries); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the file goes on after its array")
	}
	return entries, nil
}

func (c *Cron) apply(status cronStatus) {
	if status.Error != nil {
		c.Error = *status.Error
	}
	if status.LastError != nil {
		c.LastError = *status.LastError
	}
	c.Timezone = status.Timezone
	c.NextRunAt, c.LastRunAt, c.LastDurationMs = status.NextRunAt, status.LastRunAt, status.LastDurationMs
	c.Running, c.Runs = status.Running, status.Runs
}
