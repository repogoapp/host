package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ProjectMarks is what the user set on a project, one field per column of
// state.project_marks. Nil is not set.
type ProjectMarks struct {
	PickedAt *int64
	PinnedAt *int64
	Name     *string
}

func (m ProjectMarks) empty() bool {
	return m.PickedAt == nil && m.PinnedAt == nil && m.Name == nil
}

// updateProjectMarks is the one writer of state.project_marks, written whole
// or deleted when nothing is set. Devices read a project's marks on their
// next projects.list, so nothing fans out.
func (s *Store) updateProjectMarks(path string, change func(*ProjectMarks) bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var marks ProjectMarks
	err = tx.QueryRow(`SELECT picked_at, pinned_at, name FROM state.project_marks WHERE path = ?`,
		path).Scan(&marks.PickedAt, &marks.PinnedAt, &marks.Name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: read project marks: %w", err)
	}
	if !change(&marks) {
		return false, nil
	}

	if marks.empty() {
		_, err = tx.Exec(`DELETE FROM state.project_marks WHERE path = ?`, path)
	} else {
		_, err = tx.Exec(`INSERT INTO state.project_marks (path, picked_at, pinned_at, name) VALUES (?, ?, ?, ?)
			ON CONFLICT (path) DO UPDATE SET
			  picked_at = excluded.picked_at,
			  pinned_at = excluded.pinned_at,
			  name      = excluded.name`,
			path, marks.PickedAt, marks.PinnedAt, marks.Name)
	}
	if err != nil {
		return false, fmt.Errorf("store: write project marks: %w", err)
	}
	return true, tx.Commit()
}

// Pick makes a folder the user chose in the folder picker a root, or moves it
// to the top of the recent ones as of at. The caller has already resolved and vetted it.
func (s *Store) Pick(path string, at time.Time) error {
	_, err := s.updateProjectMarks(path, func(m *ProjectMarks) bool {
		m.PickedAt = ptr(at.UnixMilli())
		return true
	})
	return err
}

// Rename sets the user's name for a project, or clears it when blank, and
// returns the project's row. Only a listed project can be named.
func (s *Store) Rename(path, name string) (Project, error) {
	if _, err := s.Project(path); err != nil {
		return Project{}, err
	}
	name = collapseSpace(name)
	if _, err := s.updateProjectMarks(path, func(m *ProjectMarks) bool {
		if name == "" {
			m.Name = nil
		} else {
			m.Name = &name
		}
		return true
	}); err != nil {
		return Project{}, err
	}
	return s.Project(path)
}

// SetPinned pins a project to the top of the list, or unpins it, and returns
// the project's row. Only a listed project can be pinned.
func (s *Store) SetPinned(path string, pinned bool, at time.Time) (Project, error) {
	if _, err := s.Project(path); err != nil {
		return Project{}, err
	}
	if _, err := s.updateProjectMarks(path, func(m *ProjectMarks) bool {
		if pinned {
			m.PinnedAt = ptr(at.UnixMilli())
		} else {
			m.PinnedAt = nil
		}
		return true
	}); err != nil {
		return Project{}, err
	}
	return s.Project(path)
}

// Project is one listed project's row, with the user's marks on it.
func (s *Store) Project(path string) (Project, error) {
	rows, err := s.projects(`WHERE p.path = ?`, path)
	if err != nil {
		return Project{}, err
	}
	if len(rows) == 0 {
		return Project{}, fmt.Errorf("%w: %q is not a listed project", ErrInvalid, path)
	}
	return rows[0], nil
}

// ProjectName is what a folder is called wherever it is shown, listed or not
// (a chat can run in a folder no sweep has listed yet).
func (s *Store) ProjectName(path string) string {
	if row, err := s.Project(path); err == nil {
		return row.DisplayName
	}
	return filepath.Base(path)
}

// projectName is the one rule for what a project is called: the user's name;
// else the repository, spelled as the folder spells it when they match
// ignoring case (the host lowercases repo_name); else the folder.
func projectName(path, repoName string, name *string) string {
	if name != nil {
		return *name
	}
	folder := filepath.Base(path)
	switch {
	case repoName == "":
		return folder
	case strings.EqualFold(folder, repoName):
		return folder
	default:
		return repoName
	}
}
