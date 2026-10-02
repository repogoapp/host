package builds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/repogo/host/internal/apphome"
)

func (s *Service) Publication(id string) (Publication, error) {
	if !idPattern.MatchString(id) {
		return Publication{}, fmt.Errorf("%w: publication %q", ErrNotFound, id)
	}
	var p Publication
	found, err := apphome.ReadJSON(filepath.Join(s.publicationDir(id), "publish.json"), &p)
	if err != nil {
		return Publication{}, err
	}
	if !found {
		return Publication{}, fmt.Errorf("%w: publication %s", ErrNotFound, id)
	}
	return p, nil
}

// Publications lists an app's uploads, newest first; no project lists every app's.
func (s *Service) Publications(project, path, target string) ([]Publication, error) {
	if project != "" {
		contained, err := s.cfg.Paths.Contain(project)
		if err != nil {
			return nil, err
		}
		project = contained
	}
	entries, err := os.ReadDir(filepath.Join(s.cfg.Dir, "publications"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out := []Publication{}
	for _, e := range entries {
		if !e.IsDir() || !idPattern.MatchString(e.Name()) {
			continue
		}
		p, err := s.Publication(e.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if project == "" || p.Project == project && p.Path == path && p.Target == target {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

func (s *Service) PublishLog(id string, offset int64) (LogChunk, error) {
	p, err := s.Publication(id)
	if err != nil {
		return LogChunk{}, err
	}
	return readLog(filepath.Join(s.publicationDir(id), "publish.log"), offset, p.Status != StatusPublishing)
}

func (s *Service) CancelPublish(id string) error {
	s.mu.Lock()
	run, ok := s.publishing[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: publication %s is not running", ErrNotFound, id)
	}
	run.cancel(errCanceled)
	return nil
}

func (s *Service) publicationDir(id string) string {
	return filepath.Join(s.cfg.Dir, "publications", id)
}

func (s *Service) savePublication(p Publication) error {
	return apphome.WriteJSON(filepath.Join(s.publicationDir(p.ID), "publish.json"), p, 0o600)
}

func (s *Service) finishPublication(p *Publication, status, code, message string) {
	p.Status, p.Phase, p.ErrorCode, p.ErrorMessage = status, "", code, message
	p.FinishedAt = time.Now().UnixMilli()
	if err := s.savePublication(*p); err != nil {
		s.cfg.Log.Warn("builds: saving publication failed", "publication", p.ID, "err", err)
	}
}

func (s *Service) settlePublications() error {
	all, err := s.Publications("", "", "")
	if err != nil {
		return err
	}
	for _, p := range all {
		if p.Status != StatusPublishing {
			continue
		}
		status, code, message := StatusFailed, "host_stopped", "the host stopped before upload"
		if p.Phase == "upload" {
			status, code, message = StatusUnknown, "upload_uncertain", "the host stopped during upload; check App Store Connect before retrying"
		}
		p.Status, p.Phase, p.ErrorCode, p.ErrorMessage = status, "", code, message
		p.FinishedAt = time.Now().UnixMilli()
		if err := s.savePublication(p); err != nil {
			return err
		}
	}
	return nil
}
