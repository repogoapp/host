package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
// returns the project's row with the worktrees renamed with it (a clone names
// its repository's worktrees). Only a listed project can be named.
func (s *Store) Rename(path, name string) (Project, []Project, error) {
	all, err := s.projects(``)
	if err != nil {
		return Project{}, nil, err
	}
	if !slices.ContainsFunc(all, func(p Project) bool { return p.Path == path }) {
		return Project{}, nil, fmt.Errorf("%w: %q is not a listed project", ErrInvalid, path)
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
		return Project{}, nil, err
	}
	row, err := s.Project(path)
	if err != nil {
		return Project{}, nil, err
	}
	inherited, err := s.renamedSince(names(all), path)
	return row, inherited, err
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

// markedRow is a stored row with the user's name for it, before the name
// rule has run.
type markedRow struct {
	Project
	name *string
}

// nameRows gives each row its DisplayName. A worktree the user has not named
// is called what its repository's clone is, so every checkout of a repository
// reads as one project.
func (s *Store) nameRows(rows []markedRow) ([]Project, error) {
	var clones map[string]string
	if slices.ContainsFunc(rows, func(r markedRow) bool { return inheritsName(r.Project) }) {
		var err error
		if clones, err = s.cloneNames(); err != nil {
			return nil, err
		}
	}
	out := make([]Project, len(rows))
	for i, r := range rows {
		r.DisplayName = projectName(r.Path, r.RepoName, r.name)
		if clone, ok := clones[repoKey(r.Project)]; ok && r.name == nil && inheritsName(r.Project) {
			r.DisplayName = clone
		}
		out[i] = r.Project
	}
	return out, nil
}

// inheritsName is a worktree of a known repository, which takes its name from
// the repository's clone when one is listed.
func inheritsName(p Project) bool {
	return (p.Kind == ProjectWorktree || p.Kind == ProjectManaged) && p.RepoName != ""
}

func repoKey(p Project) string { return p.RepoOwner + "/" + p.RepoName }

// cloneNames is what each repository's clone is called: one the user named,
// else the one worked in last. Two clones of one repository keep their own
// names; only worktrees read this.
func (s *Store) cloneNames() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT p.path, p.repo_owner, p.repo_name, m.name
		FROM projects p LEFT JOIN state.project_marks m ON m.path = p.path
		WHERE p.kind = ? AND p.repo_name != ''
		ORDER BY m.name IS NULL, p.activity_at DESC, p.path`, ProjectClone)
	if err != nil {
		return nil, fmt.Errorf("store: clone names: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var clone Project
		var name *string
		if err := rows.Scan(&clone.Path, &clone.RepoOwner, &clone.RepoName, &name); err != nil {
			return nil, err
		}
		if _, ok := out[repoKey(clone)]; !ok {
			out[repoKey(clone)] = projectName(clone.Path, clone.RepoName, name)
		}
	}
	return out, rows.Err()
}

// names is each row's DisplayName by path, to compare against after an edit.
func names(rows []Project) map[string]string {
	out := make(map[string]string, len(rows))
	for _, p := range rows {
		out[p.Path] = p.DisplayName
	}
	return out
}

// renamedSince is the stored rows, other than skip, whose name differs from
// before: the worktrees an edit to their clone renamed.
func (s *Store) renamedSince(before map[string]string, skip ...string) ([]Project, error) {
	all, err := s.projects(``)
	if err != nil {
		return nil, err
	}
	out := []Project{}
	for _, p := range all {
		if was, ok := before[p.Path]; ok && was != p.DisplayName && !slices.Contains(skip, p.Path) {
			out = append(out, p)
		}
	}
	return out, nil
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
