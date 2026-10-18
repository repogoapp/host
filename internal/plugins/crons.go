package plugins

import (
	"context"
	"path/filepath"
	"time"
)

// A plugin's crons run inside its folder's Worker, which the plugins server
// starts the first time it is asked about the folder. Keep asks about every
// folder with plugins, so their crons run without a card or an agent calling.

// keepEvery is how often Keep asks again: it starts a server that stopped, and
// reloads a folder whose code changed, within this long.
const keepEvery = time.Minute

// Keep holds every folder with plugins open until ctx ends: the environment's,
// and each of projects() that has a .repogo/plugins folder.
func (s *Service) Keep(ctx context.Context, projects func() []string) {
	if !s.On() {
		return
	}
	tick := time.NewTicker(keepEvery)
	defer tick.Stop()
	for {
		s.keep(ctx, projects())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// keep asks each folder for its manifest once, which opens its Worker and
// reloads it if its code changed.
func (s *Service) keep(ctx context.Context, projects []string) {
	var targets []Target
	if isDir(s.cfg.Global) {
		targets = append(targets, Target{Dir: s.cfg.Global})
	}
	for _, path := range projects {
		if dir := filepath.Join(path, filepath.FromSlash(Folder)); isDir(dir) {
			targets = append(targets, Target{Dir: dir})
		}
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		if answer, err := s.ask(ctx, t, "list", t); err != nil || answer.Error != nil {
			s.cfg.Log.Debug("plugins: couldn't keep a folder open", "dir", t.Dir, "err", err, "plugin_error", answer.Error)
		}
	}
}
