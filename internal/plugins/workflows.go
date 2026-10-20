package plugins

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// RunWorkflows starts the SDK's durable workers without a phone or a cron.
func (s *Service) RunWorkflows(ctx context.Context) {
	if !s.On() {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if s.hasWorkflows() {
			if _, err := s.running(ctx); err != nil && ctx.Err() == nil {
				s.cfg.Log.Warn("plugins: workflow recovery unavailable", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) hasWorkflows() bool {
	_, err := os.Stat(filepath.Join(s.cfg.DataDir, ".workflows", "catalog.db"))
	return err == nil
}
