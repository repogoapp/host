// Package uploads stages files a phone sends in chunks, for a plugin call to
// read: one folder per upload under the host's state, owned by the device and
// project that wrote it, and swept once nothing has touched it for a day.
package uploads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/errkind"
)

const (
	MaxFileBytes   = 1 << 30
	maxDeviceBytes = 2 << 30
	maxTotalBytes  = 4 << 30
	// MaxPerCall is the most files one plugin call may take.
	MaxPerCall = 10
	// idle is how long an upload no call holds is kept after its last touch.
	idle     = 24 * time.Hour
	metaName = "meta.json"
)

var (
	ErrNotFound = errkind.New(errkind.NotFound, "uploads: no such upload")
	ErrInvalid  = errkind.New(errkind.Invalid, "uploads")
)

// The phone names each upload with a UUID, so a folder name is never a path.
var uploadID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// File is a finished upload as a plugin reads it.
type File struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	Path string `json:"path"`
}

// Chunk is one write: where it starts, its bytes, and whether it ends the file.
// Name, Type and Size are the whole file's and must not change between chunks.
type Chunk struct {
	Device  device.ID
	Project string
	ID      string
	Name    string
	Type    string
	Size    int64
	Offset  int64
	Data    []byte
	Done    bool
}

// Progress is what the host holds of an upload: the phone sends the next
// chunk from Received, which also resumes a transfer cut off midway.
type Progress struct {
	Received int64 `json:"received"`
	Complete bool  `json:"complete"`
}

// meta is an upload's meta.json; the file's size on disk is how much arrived.
type meta struct {
	Device   device.ID `json:"device"`
	Project  string    `json:"project"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Complete bool      `json:"complete"`
	Touched  int64     `json:"touched"`
}

// Store is the staging folder. One lock covers every upload: a chunk is a
// megabyte, written in a moment.
type Store struct {
	dir  string
	root *os.Root
	log  *slog.Logger

	mu     sync.Mutex
	leases map[string]int
}

func Open(dir string, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, root: root, log: log, leases: map[string]int{}}, nil
}

func (s *Store) Close() { _ = s.root.Close() }

// Write appends a chunk that starts where the file ends. One that starts
// anywhere else writes nothing and answers what's held: a repeat after a lost
// reply, or a gap, both resume from Received.
func (s *Store) Write(c Chunk) (Progress, error) {
	if !uploadID.MatchString(c.ID) {
		return Progress{}, ErrInvalid.Errorf("an upload id is a UUID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.meta(c.ID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if m, err = s.start(c); err != nil {
			return Progress{}, err
		}
	case err != nil:
		return Progress{}, err
	case m.Device != c.Device:
		return Progress{}, ErrNotFound
	case m.Project != filepath.Clean(c.Project) || m.Name != CleanName(c.Name, c.Type) || m.Type != mimeType(c.Type) || m.Size != c.Size:
		return Progress{}, ErrInvalid.Errorf("an upload's project, name, type and size stay as its first chunk said")
	}
	if m.Complete {
		return Progress{Received: m.Size, Complete: true}, nil
	}
	received, err := s.received(c.ID, m.Name)
	if err != nil {
		return Progress{}, err
	}
	if c.Offset != received {
		return Progress{Received: received}, nil
	}
	if received+int64(len(c.Data)) > m.Size {
		return Progress{}, ErrInvalid.Errorf("the chunk runs past the file's %d bytes", m.Size)
	}
	if len(c.Data) > 0 {
		f, err := s.root.OpenFile(filepath.Join(c.ID, m.Name), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return Progress{}, err
		}
		_, err = f.Write(c.Data)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return Progress{}, err
		}
		received += int64(len(c.Data))
	}
	if c.Done {
		if received != m.Size {
			return Progress{}, ErrInvalid.Errorf("the upload ended at %d of its %d bytes", received, m.Size)
		}
		m.Complete = true
	}
	m.Touched = time.Now().UnixMilli()
	if err := s.saveMeta(c.ID, m); err != nil {
		return Progress{}, err
	}
	return Progress{Received: received, Complete: m.Complete}, nil
}

// start makes an upload's folder on its first chunk, once it fits the bounds.
func (s *Store) start(c Chunk) (meta, error) {
	if c.Size <= 0 || c.Size > MaxFileBytes {
		return meta{}, ErrInvalid.Errorf("a file is 1 byte to %d MiB", MaxFileBytes>>20)
	}
	if !filepath.IsAbs(c.Project) {
		return meta{}, ErrInvalid.Errorf("project must be an absolute path")
	}
	mine, total := s.usage(c.Device)
	if mine+c.Size > maxDeviceBytes || total+c.Size > maxTotalBytes {
		return meta{}, ErrInvalid.Errorf("the environment holds too many unsent uploads; try again once they finish")
	}
	m := meta{
		Device: c.Device, Project: filepath.Clean(c.Project), Name: CleanName(c.Name, c.Type),
		Type: mimeType(c.Type), Size: c.Size, Touched: time.Now().UnixMilli(),
	}
	if err := s.root.Mkdir(c.ID, 0o700); err != nil {
		return meta{}, err
	}
	f, err := s.root.OpenFile(filepath.Join(c.ID, m.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return meta{}, err
	}
	if err := f.Close(); err != nil {
		return meta{}, err
	}
	return m, s.saveMeta(c.ID, m)
}

// Remove deletes one of the device's uploads, as a cancel does.
func (s *Store) Remove(d device.ID, id string) error {
	if !uploadID.MatchString(id) {
		return ErrInvalid.Errorf("an upload id is a UUID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.meta(id)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && m.Device != d) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if s.leases[id] > 0 {
		return ErrInvalid.Errorf("a plugin call is reading the upload")
	}
	return s.root.RemoveAll(id)
}

// Hold resolves the device's finished uploads for a call in project and keeps
// them from the sweep until release runs. A confirm's question and the
// confirmed call each hold them, so neither consumes them.
func (s *Store) Hold(d device.ID, project string, ids []string) (files []File, release func(), err error) {
	if len(ids) > MaxPerCall {
		return nil, nil, ErrInvalid.Errorf("at most %d files per call", MaxPerCall)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range ids {
		if !uploadID.MatchString(id) || seen[id] {
			return nil, nil, ErrInvalid.Errorf("upload ids are distinct UUIDs")
		}
		seen[id] = true
		m, err := s.meta(id)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && (m.Device != d || m.Project != filepath.Clean(project))) {
			return nil, nil, ErrNotFound
		}
		if err != nil {
			return nil, nil, err
		}
		if !m.Complete {
			return nil, nil, ErrInvalid.Errorf("%s is still uploading", m.Name)
		}
		files = append(files, File{ID: id, Name: m.Name, Type: m.Type, Size: m.Size, Path: filepath.Join(s.dir, id, m.Name)})
	}
	for _, id := range ids {
		s.leases[id]++
	}
	var once sync.Once
	return files, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, id := range ids {
				if s.leases[id]--; s.leases[id] <= 0 {
					delete(s.leases, id)
				}
				if m, err := s.meta(id); err == nil {
					m.Touched = time.Now().UnixMilli()
					_ = s.saveMeta(id, m)
				}
			}
		})
	}, nil
}

// Sweep removes uploads idle for a day that no call holds, finished or not;
// a plugin copies what it keeps.
func (s *Store) Sweep(_ context.Context, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.ids() {
		if s.leases[id] > 0 {
			continue
		}
		m, err := s.meta(id)
		touched := time.UnixMilli(m.Touched)
		if err != nil {
			// A folder without readable meta is a start cut off; its own time says how old.
			info, statErr := s.root.Stat(id)
			if statErr != nil {
				continue
			}
			touched = info.ModTime()
		}
		if now.Sub(touched) < idle {
			continue
		}
		if err := s.root.RemoveAll(id); err != nil {
			s.log.Debug("uploads: sweep failed", "id", id, "err", err)
		}
	}
}

// usage is the bytes staged for d and for every device, by declared size.
func (s *Store) usage(d device.ID) (mine, total int64) {
	for _, id := range s.ids() {
		m, err := s.meta(id)
		if err != nil {
			continue
		}
		total += m.Size
		if m.Device == d {
			mine += m.Size
		}
	}
	return mine, total
}

func (s *Store) ids() []string {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && uploadID.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func (s *Store) meta(id string) (meta, error) {
	var m meta
	b, err := s.root.ReadFile(filepath.Join(id, metaName))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("uploads: %s: %w", id, err)
	}
	return m, nil
}

func (s *Store) saveMeta(id string, m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.root.WriteFile(filepath.Join(id, metaName), b, 0o600)
}

// received is how much of the file is on disk; a link in its place is refused.
func (s *Store) received(id, name string) (int64, error) {
	info, err := s.root.Lstat(filepath.Join(id, name))
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, ErrInvalid.Errorf("the upload isn't a regular file")
	}
	return info.Size(), nil
}

func mimeType(t string) string {
	t = strings.TrimSpace(t)
	if t == "" || !strings.Contains(t, "/") || len(t) > 255 {
		return "application/octet-stream"
	}
	return t
}

// CleanName keeps the name the user saw, minus anything that would move the
// file out of its directory or make a shell choke on the path.
func CleanName(name, mimeType string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == metaName {
		name = "attachment" + extensionFor(mimeType)
	}
	return name
}

func extensionFor(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "video/quicktime":
		return ".mov"
	case "video/mp4":
		return ".mp4"
	}
	return ""
}
