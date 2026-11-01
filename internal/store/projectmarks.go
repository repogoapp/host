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
	// The column pinned_at: Project.PinOrder.
	PinOrder *float64
	Name     *string
}

func (m ProjectMarks) empty() bool {
	return m.PickedAt == nil && m.PinOrder == nil && m.Name == nil
}

// updateProjectMarks writes a project's state.project_marks row whole, or
// deletes it when nothing is set; Pin writes pinned_at alone. Devices read a project's marks on their
// next projects.list, so nothing fans out.
func (s *Store) updateProjectMarks(path string, change func(*ProjectMarks) bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var marks ProjectMarks
	err = tx.QueryRow(`SELECT picked_at, pinned_at, name FROM state.project_marks WHERE path = ?`,
		path).Scan(&marks.PickedAt, &marks.PinOrder, &marks.Name)
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
			path, marks.PickedAt, marks.PinOrder, marks.Name)
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

// Pin pins a listed project, or moves a pinned one, just below the pin `after`
// ("" or unpinned: the top), at its neighbours' midpoint; with no room left it
// renumbers every pin 0, 1, 2…. Returns the rows it moved, path's first.
func (s *Store) Pin(path, after string) ([]Project, error) {
	if _, err := s.Project(path); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	pins, err := readPins(tx)
	if err != nil {
		return nil, err
	}
	pins = slices.DeleteFunc(pins, func(p pin) bool { return p.path == path })
	at := 0
	if i := slices.IndexFunc(pins, func(p pin) bool { return p.path == after }); after != "" && i >= 0 {
		at = i + 1
	}

	var above, below *float64
	if at > 0 {
		above = &pins[at-1].order
	}
	if at < len(pins) {
		below = &pins[at].order
	}
	moved := []string{path}
	if order, ok := pinBetween(above, below); ok {
		err = writePin(tx, path, order)
	} else {
		var others []string
		others, err = renumberPins(tx, slices.Insert(pins, at, pin{path: path}), path)
		moved = append(moved, others...)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	rows := make([]Project, 0, len(moved))
	for _, p := range moved {
		row, err := s.Project(p)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Unpin takes a project off the pinned list and returns its row.
func (s *Store) Unpin(path string) (Project, error) {
	if _, err := s.Project(path); err != nil {
		return Project{}, err
	}
	if _, err := s.updateProjectMarks(path, func(m *ProjectMarks) bool {
		m.PinOrder = nil
		return true
	}); err != nil {
		return Project{}, err
	}
	return s.Project(path)
}

// pin is one pinned project's place.
type pin struct {
	path  string
	order float64
}

// readPins is every pin, top first.
func readPins(tx *sql.Tx) ([]pin, error) {
	rows, err := tx.Query(`SELECT path, pinned_at FROM state.project_marks
		WHERE pinned_at IS NOT NULL ORDER BY pinned_at, path`)
	if err != nil {
		return nil, fmt.Errorf("store: read pins: %w", err)
	}
	defer rows.Close()
	var pins []pin
	for rows.Next() {
		var p pin
		if err := rows.Scan(&p.path, &p.order); err != nil {
			return nil, err
		}
		pins = append(pins, p)
	}
	return pins, rows.Err()
}

// writePin sets one project's pin order, keeping its other marks.
func writePin(tx *sql.Tx, path string, order float64) error {
	_, err := tx.Exec(`INSERT INTO state.project_marks (path, pinned_at) VALUES (?, ?)
		ON CONFLICT (path) DO UPDATE SET pinned_at = excluded.pinned_at`, path, order)
	if err != nil {
		return fmt.Errorf("store: write pin: %w", err)
	}
	return nil
}

// renumberPins writes 0, 1, 2… down pins, top first, skipping one already at
// its number; path, just placed, is always written. Returns the others written.
func renumberPins(tx *sql.Tx, pins []pin, path string) ([]string, error) {
	var others []string
	for i, p := range pins {
		if p.path == path {
			if err := writePin(tx, path, float64(i)); err != nil {
				return nil, err
			}
			continue
		}
		if p.order == float64(i) {
			continue
		}
		if err := writePin(tx, p.path, float64(i)); err != nil {
			return nil, err
		}
		others = append(others, p.path)
	}
	return others, nil
}

// pinBetween is a pin order between above and below (nil at either end of the
// list), or false when there is no number left between them.
func pinBetween(above, below *float64) (float64, bool) {
	switch {
	case above == nil && below == nil:
		return 0, true
	case above == nil:
		return *below - 1, *below-1 < *below
	case below == nil:
		return *above + 1, *above+1 > *above
	}
	mid := *above + (*below-*above)/2
	return mid, *above < mid && mid < *below
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
