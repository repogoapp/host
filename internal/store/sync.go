package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/repogo/host/internal/jsonrpc"
)

// The chat sync surface: a `rev` per row from one counter, bumped only when
// content changed, and an `epoch` per cache file. A revision is never handed
// out twice, so a device's cursor stays valid across restarts. Rows go whole.

// maxProjects bounds projects.list. Membership is decided by the host, so two
// clients cannot disagree about a scope parameter.
const maxProjects = 200

// Project is the metadata and last observed diff for one folder.
type Project struct {
	Path      string `json:"path"`
	ChatCount int    `json:"chat_count"`

	// The repository this is a checkout of, empty for a folder that is not one.
	// Lowercased, because it is a grouping key and `Octocat/Code` and
	// `octocat/code` are one project.
	RepoOwner string `json:"repo_owner"`
	RepoName  string `json:"repo_name"`

	Kind ProjectKind `json:"kind"`
	// The project icon's content hash, "" when it has none: a device fetches
	// the icon (projects.detect_icon) only when this differs from the one it holds.
	IconHash      string `json:"icon_hash"`
	DiffAvailable bool   `json:"diff_available"`
	FilesChanged  int    `json:"files_changed"`
	Additions     int    `json:"additions"`
	Deletions     int    `json:"deletions"`

	// When this project was last worked in: its chats' newest activity_at, or
	// the folder's mtime. The client orders by this; a stored position would
	// bump every revision whenever one project moved.
	ActivityAt int64 `json:"activity_at"`

	// When the user last sent a prompt in any of its chats, 0 never. The
	// other sort key, beside ChatCount.
	LastMessageAt int64 `json:"last_message_at"`

	// What every surface calls the project: the user's name, else the
	// repository, else the folder (projectName). The raw name is not sent.
	DisplayName string `json:"display_name"`
	// When the user pinned it to the top of the list; nil is not pinned.
	PinnedAt *int64 `json:"pinned_at,omitempty"`

	// The host serving this row. Stamped on the way out, not stored: a device
	// mirrors more than one host, and two of them can hold the same path.
	HostID string `json:"host_id"`
}

// ProjectKind is what a folder is, which decides how it is shown and what may
// be done to it. projectsync reads it from git (a clone's `.git` is a folder, a
// worktree's a file) and this host's worktree folder, so nothing is recorded.
type ProjectKind string

const (
	// ProjectClone is a real clone: `.git` is a directory.
	ProjectClone ProjectKind = "clone"

	// ProjectManaged is a worktree this host made for a chat, the only kind that
	// is safe to reap.
	ProjectManaged ProjectKind = "managed"

	// ProjectWorktree is a linked worktree somebody made by hand. A legitimate
	// place to work; not ours to delete.
	ProjectWorktree ProjectKind = "worktree"

	// ProjectFolder has no git in it at all. "Open a folder and talk to an
	// agent" works before anyone runs `git init`.
	ProjectFolder ProjectKind = "folder"
)

// Activity is what the chat cache knows about a folder.
type Activity struct {
	Chats         int
	ActivityAt    int64
	LastMessageAt int64
}

// Epoch identifies this process's revision counter. Stable until the host restarts.
func (s *Store) Epoch() string { return s.epoch }

// openSync reads the cache's epoch, minting one for a new file, and seeds the
// counter past every revision handed out: the highest on a row, or the floor a
// deletion left, since a deleted row's revision no longer shows.
func (s *Store) openSync() error {
	var floor int64
	err := s.db.QueryRow(`SELECT epoch, rev_floor FROM sync_meta`).Scan(&s.epoch, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		var buf [8]byte
		rand.Read(buf[:])
		s.epoch = hex.EncodeToString(buf[:])
		_, err = s.db.Exec(`INSERT INTO sync_meta (epoch, rev_floor) VALUES (?, 0)`, s.epoch)
	}
	if err != nil {
		return fmt.Errorf("store: open sync: %w", err)
	}
	var high int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rev), 0) FROM sessions`).Scan(&high); err != nil {
		return err
	}
	s.rev.Store(max(high, floor))
	return nil
}

// keepRevFloor records the counter before rows are deleted, so a restart
// never hands out a revision a device already holds. Called in the delete's
// transaction.
func (s *Store) keepRevFloor(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE sync_meta SET rev_floor = MAX(rev_floor, ?)`, s.rev.Load())
	return err
}

// Activity is how many chats each folder holds, when it was last worked in
// (its chats' newest activity_at) and when the user last sent one a prompt,
// from one GROUP BY.
func (s *Store) Activity() (map[string]Activity, error) {
	rows, err := s.db.Query(
		`SELECT cwd, COUNT(*), MAX(activity_at), MAX(last_message_at) FROM sessions WHERE cwd != '' GROUP BY cwd`)
	if err != nil {
		return nil, fmt.Errorf("store: activity: %w", err)
	}
	defer rows.Close()

	out := map[string]Activity{}
	for rows.Next() {
		var cwd string
		var a Activity
		if err := rows.Scan(&cwd, &a.Chats, &a.ActivityAt, &a.LastMessageAt); err != nil {
			return nil, err
		}
		out[cwd] = a
	}
	return out, rows.Err()
}

// projectSelect reads project rows with the user's marks on them; callers
// append the WHERE and ORDER BY.
const projectSelect = `SELECT p.path, p.chat_count, p.activity_at, p.last_message_at, p.repo_owner, p.repo_name, p.kind,
	p.icon_hash, p.diff_available, p.files_changed, p.additions, p.deletions, m.name, m.pinned_at
	FROM projects p LEFT JOIN state.project_marks m ON m.path = p.path `

func (s *Store) projects(clauses string, args ...any) ([]Project, error) {
	rows, err := s.db.Query(projectSelect+clauses, args...)
	if err != nil {
		return nil, fmt.Errorf("store: projects: %w", err)
	}
	defer rows.Close()

	out := []Project{}
	for rows.Next() {
		var p Project
		var name *string
		if err := rows.Scan(&p.Path, &p.ChatCount, &p.ActivityAt, &p.LastMessageAt,
			&p.RepoOwner, &p.RepoName, &p.Kind, &p.IconHash, &p.DiffAvailable, &p.FilesChanged, &p.Additions, &p.Deletions,
			&name, &p.PinnedAt); err != nil {
			return nil, err
		}
		p.DisplayName = projectName(p.Path, p.RepoName, name)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectChange is what a SyncProjects pass moved: rows that read
// differently now, whole, and paths that are no longer projects.
type ProjectChange struct {
	Changed []Project
	Removed []string
}

func (c ProjectChange) Empty() bool { return len(c.Changed) == 0 && len(c.Removed) == 0 }

// SyncProjects makes the table match `rows` and reports what changed. An
// unchanged project is not rewritten, so a change is news to every device.
func (s *Store) SyncProjects(rows []Project) (ProjectChange, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	all, err := s.projects(``)
	if err != nil {
		return ProjectChange{}, err
	}
	existing := make(map[string]Project, len(all))
	for _, p := range all {
		existing[p.Path] = p
	}

	tx, err := s.db.Begin()
	if err != nil {
		return ProjectChange{}, err
	}
	defer tx.Rollback()

	var changed, removed []string
	for _, row := range rows {
		if was, ok := existing[row.Path]; ok {
			delete(existing, row.Path)
			// projectsync only resolves a repository for projects that have none, so a
			// sweep that did not look must not erase what an earlier one found.
			if row.RepoOwner == "" && was.RepoOwner != "" {
				row.RepoOwner, row.RepoName = was.RepoOwner, was.RepoName
			}
			if was.ChatCount == row.ChatCount && was.ActivityAt == row.ActivityAt && was.LastMessageAt == row.LastMessageAt &&
				was.RepoOwner == row.RepoOwner && was.RepoName == row.RepoName &&
				was.Kind == row.Kind && was.IconHash == row.IconHash {
				continue
			}
		}
		if _, err := tx.Exec(
			`INSERT INTO projects
			   (path, chat_count, activity_at, last_message_at, repo_owner, repo_name, kind, icon_hash)
			 VALUES (?,?,?,?,?,?,?,?)
			 ON CONFLICT(path) DO UPDATE SET
			   chat_count = excluded.chat_count,
			   activity_at = excluded.activity_at,
			   last_message_at = excluded.last_message_at,
			   repo_owner = excluded.repo_owner,
			   repo_name  = excluded.repo_name,
			   kind       = excluded.kind,
			   icon_hash  = excluded.icon_hash`,
			row.Path, row.ChatCount, row.ActivityAt, row.LastMessageAt,
			row.RepoOwner, row.RepoName, row.Kind, row.IconHash); err != nil {
			return ProjectChange{}, err
		}
		changed = append(changed, row.Path)
	}

	// Whatever `rows` did not mention is gone.
	for path := range existing {
		if _, err := tx.Exec(`DELETE FROM projects WHERE path = ?`, path); err != nil {
			return ProjectChange{}, err
		}
		removed = append(removed, path)
	}
	if len(changed) == 0 && len(removed) == 0 {
		return ProjectChange{}, nil
	}
	if err := tx.Commit(); err != nil {
		return ProjectChange{}, err
	}
	out := ProjectChange{Changed: []Project{}, Removed: removed}
	if len(changed) > 0 {
		// Read back whole, so the diff totals a watch measured go out too.
		if out.Changed, err = s.projects(`WHERE p.path IN (`+marks(len(changed))+`)`, anys(changed)...); err != nil {
			return ProjectChange{}, err
		}
	}
	return out, nil
}

// ProjectsAt is the stored rows for paths, in no order; a path with no row is
// left out.
func (s *Store) ProjectsAt(paths []string) ([]Project, error) {
	if len(paths) == 0 {
		return []Project{}, nil
	}
	return s.projects(`WHERE p.path IN (`+marks(len(paths))+`)`, anys(paths)...)
}

// KnownRepos is the set of project paths that already have a repository, so a
// pass can skip reading their origin.
func (s *Store) KnownRepos() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT path FROM projects WHERE repo_owner != ''`)
	if err != nil {
		return nil, fmt.Errorf("store: known repos: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out[path] = true
	}
	return out, rows.Err()
}

// Repo is the repository projectsync stored for a project; empty for a folder
// that is not a checkout or a project not yet swept.
func (s *Store) Repo(path string) (owner, name string, err error) {
	err = s.db.QueryRow(`SELECT repo_owner, repo_name FROM projects WHERE path = ?`, path).Scan(&owner, &name)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("store: repo: %w", err)
	}
	return owner, name, nil
}

// UpdateProjectDiff writes a project's totals and reports whether they changed.
func (s *Store) UpdateProjectDiff(path string, available bool, files, additions, deletions int) (bool, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	var oldAvailable bool
	var oldFiles, oldAdditions, oldDeletions int
	err := s.db.QueryRow(`SELECT diff_available, files_changed, additions, deletions
		FROM projects WHERE path = ?`, path).Scan(&oldAvailable, &oldFiles, &oldAdditions, &oldDeletions)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if oldAvailable == available && oldFiles == files && oldAdditions == additions && oldDeletions == deletions {
		return false, nil
	}
	_, err = s.db.Exec(`UPDATE projects SET diff_available = ?, files_changed = ?,
		additions = ?, deletions = ? WHERE path = ?`,
		available, files, additions, deletions, path)
	return err == nil, err
}

// maxSyncBytes bounds one SyncChats: half the message limit, so a cold start is one
// round trip for any set that fits, and `more` loops only for one that does not.
const maxSyncBytes = jsonrpc.MaxMessageBytes / 2

// SyncParams asks for the chat rows and deletions past a revision.
type SyncParams struct {
	Epoch string `json:"epoch"`
	Since int64  `json:"since"`
}

type SyncResult struct {
	Epoch  string   `json:"epoch"`
	Rev    int64    `json:"rev"`
	More   bool     `json:"more"`
	Upsert []Chat   `json:"upsert" wire:"array"`
	Delete []string `json:"delete" wire:"array"`
}

// ListProjects is the newest maxProjects projects and every pinned one, each
// stamped with host: last prompt first, else last activity, so a fresh folder
// is not sunk below every chatted one. Path breaks ties, so reads agree.
func (s *Store) ListProjects(host string) ([]Project, error) {
	projects, err := s.projects(`WHERE p.path IN (SELECT path FROM projects ORDER BY activity_at DESC LIMIT ?)
		OR m.pinned_at IS NOT NULL
		ORDER BY CASE WHEN p.last_message_at > 0 THEN p.last_message_at ELSE p.activity_at END DESC, p.path`, maxProjects)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		projects[i].HostID = host
	}
	return projects, nil
}

// SyncChats is the chat rows written and chats deleted past the cursor, each
// row stamped with host, in one revision order, so a capped answer is a
// prefix the cursor advances through.
func (s *Store) SyncChats(req SyncParams, host string) (SyncResult, error) {
	// An epoch mismatch means the revision counter restarted; everything is new.
	since := req.Since
	if req.Epoch != s.epoch {
		since = 0
	}
	// One transaction, so a deletion committed between the two reads can't
	// leave a row and its removal on different sides of the cursor.
	tx, err := s.db.Begin()
	if err != nil {
		return SyncResult{}, err
	}
	defer tx.Rollback()
	chats, err := readChats(tx, chatSelect+` WHERE s.rev > ? ORDER BY s.rev`, since)
	if err != nil {
		return SyncResult{}, err
	}
	// A chat made again after it was deleted is in sessions, past its deletion.
	deleted, err := deletedSince(tx, since)
	if err != nil {
		return SyncResult{}, err
	}

	// The cut falls on a byte budget; the first item always goes, however large.
	out := SyncResult{Epoch: s.epoch, Rev: since, Upsert: []Chat{}, Delete: []string{}}
	size, c, d := 0, 0, 0
	for c < len(chats) || d < len(deleted) {
		takeChat := d == len(deleted) || (c < len(chats) && chats[c].Rev < deleted[d].rev)
		var bytes int
		var rev int64
		if takeChat {
			chats[c].HostID = host
			b, err := json.Marshal(chats[c])
			if err != nil {
				return SyncResult{}, err
			}
			bytes, rev = len(b), chats[c].Rev
		} else {
			bytes, rev = len(deleted[d].id)+3, deleted[d].rev
		}
		if size+bytes > maxSyncBytes && (len(out.Upsert) > 0 || len(out.Delete) > 0) {
			out.More = true
			break
		}
		size += bytes
		out.Rev = rev
		if takeChat {
			out.Upsert = append(out.Upsert, chats[c])
			c++
		} else {
			out.Delete = append(out.Delete, deleted[d].id)
			d++
		}
	}
	return out, nil
}

type deletion struct {
	id  string
	rev int64
}

// deletedSince is every chat deleted past since and not made again, oldest
// first.
func deletedSince(db querier, since int64) ([]deletion, error) {
	rows, err := db.Query(`SELECT d.agent || ':' || d.session_id, d.rev FROM deleted_sessions d
		WHERE d.rev > ? AND NOT EXISTS (
			SELECT 1 FROM sessions s WHERE s.agent = d.agent AND s.session_id = d.session_id)
		ORDER BY d.rev`, since)
	if err != nil {
		return nil, fmt.Errorf("store: deleted chats: %w", err)
	}
	defer rows.Close()
	var out []deletion
	for rows.Next() {
		var d deletion
		if err := rows.Scan(&d.id, &d.rev); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
