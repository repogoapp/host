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
	// Set while pinned. Only being set is read: a pin from before marks had
	// times holds its old place in the order, which may be a fraction.
	PinnedAt *float64
	// "" is a cleared name, kept with its time.
	Name            *string
	NameUpdatedAt   *int64
	PinnedUpdatedAt *int64
}

func (m ProjectMarks) empty() bool {
	return m.PickedAt == nil && m.PinnedAt == nil && m.Name == nil &&
		m.NameUpdatedAt == nil && m.PinnedUpdatedAt == nil
}

// updateProjectMarks writes a project's state.project_marks row whole, or
// deletes it when nothing is set. Devices read a project's marks on their
// next projects.list, so nothing fans out.
func (s *Store) updateProjectMarks(path string, change func(*ProjectMarks) bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var marks ProjectMarks
	err = tx.QueryRow(`SELECT picked_at, pinned_at, name, name_updated_at, pinned_updated_at
		FROM state.project_marks WHERE path = ?`, path).
		Scan(&marks.PickedAt, &marks.PinnedAt, &marks.Name, &marks.NameUpdatedAt, &marks.PinnedUpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: read project marks: %w", err)
	}
	if !change(&marks) {
		return false, nil
	}

	if marks.empty() {
		_, err = tx.Exec(`DELETE FROM state.project_marks WHERE path = ?`, path)
	} else {
		_, err = tx.Exec(`INSERT INTO state.project_marks
			  (path, picked_at, pinned_at, name, name_updated_at, pinned_updated_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (path) DO UPDATE SET
			  picked_at         = excluded.picked_at,
			  pinned_at         = excluded.pinned_at,
			  name              = excluded.name,
			  name_updated_at   = excluded.name_updated_at,
			  pinned_updated_at = excluded.pinned_updated_at`,
			path, marks.PickedAt, marks.PinnedAt, marks.Name, marks.NameUpdatedAt, marks.PinnedUpdatedAt)
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

// Rename sets the user's name for a project as of updatedAt (ms), or clears it
// when blank, and returns the project's row and whether it was written. A
// rename older than the stored one is ignored: the device that sent it shows
// the newest name anyway. Only a listed project can be named.
func (s *Store) Rename(path, name string, updatedAt int64) (Project, bool, error) {
	if _, err := s.Project(path); err != nil {
		return Project{}, false, err
	}
	name = collapseSpace(name)
	written, err := s.updateProjectMarks(path, func(m *ProjectMarks) bool {
		if !newer(updatedAt, m.NameUpdatedAt) {
			return false
		}
		m.Name = &name
		m.NameUpdatedAt = &updatedAt
		return true
	})
	if err != nil {
		return Project{}, false, err
	}
	row, err := s.Project(path)
	return row, written, err
}

// SetPinned pins or unpins a project as of updatedAt (ms) and returns its row
// and whether it was written. A change older than the stored one is ignored.
// Only a listed project can be pinned.
func (s *Store) SetPinned(path string, pinned bool, updatedAt int64) (Project, bool, error) {
	if _, err := s.Project(path); err != nil {
		return Project{}, false, err
	}
	written, err := s.updateProjectMarks(path, func(m *ProjectMarks) bool {
		if !newer(updatedAt, m.PinnedUpdatedAt) {
			return false
		}
		if pinned {
			m.PinnedAt = ptr(float64(updatedAt))
		} else {
			m.PinnedAt = nil
		}
		m.PinnedUpdatedAt = &updatedAt
		return true
	})
	if err != nil {
		return Project{}, false, err
	}
	row, err := s.Project(path)
	return row, written, err
}

// newer is whether a change made at updatedAt beats the stored one. A mark
// set before marks had times (nil) loses to any change.
func newer(updatedAt int64, stored *int64) bool {
	return stored == nil || updatedAt > *stored
}

// ForgetMarks drops everything the user set on a project, for one deleted:
// a pick would keep the gone folder a root.
func (s *Store) ForgetMarks(path string) error {
	_, err := s.updateProjectMarks(path, func(m *ProjectMarks) bool {
		*m = ProjectMarks{}
		return true
	})
	return err
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

// projectName is the one rule for what a project is called: the user's name
// (not cleared); else the repository, spelled as the folder spells it when they match
// ignoring case (the host lowercases repo_name); else the folder.
func projectName(path, repoName string, name *string) string {
	if name != nil && *name != "" {
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
