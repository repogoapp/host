package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

// counting is a provider of the files in dir that counts its Lists.
type counting struct {
	dir     string
	lists   int
	refresh bool
}

func (c *counting) Kind() agent.Kind { return agent.KindClaude }

func (c *counting) ParseLine([]byte) ([]agent.Event, string) { return nil, "" }

func (c *counting) List() ([]Meta, error) {
	c.lists++
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		info, _ := e.Info()
		out = append(out, c.meta(filepath.Join(c.dir, e.Name()), info))
	}
	return out, nil
}

func (c *counting) meta(path string, info os.FileInfo) Meta {
	return Meta{ID: info.Name(), Agent: agent.KindClaude, Cwd: "/srv/demo", Path: path,
		UpdatedAt: info.ModTime(), SizeBytes: info.Size()}
}

// refreshing is counting with a Refresh, as Claude has.
type refreshing struct{ *counting }

func (r refreshing) Refresh(m Meta) (Meta, bool) {
	info, err := os.Stat(m.Path)
	if err != nil {
		return Meta{}, false
	}
	return r.meta(m.Path, info), true
}

// A session the last List saw is found from its own file, fresh, with no
// List; one the List never saw, or whose file is gone, lists again.
func TestFindReadsAKnownSessionWithoutListing(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "1")
	p := &counting{dir: dir}
	s := NewStore(refreshing{p})
	s.List()

	write("a", "12")
	m, _, err := s.Find("a")
	if err != nil || p.lists != 1 || m.SizeBytes != 2 {
		t.Fatalf("Find(a) = size %d, %v, after %d lists; want the file's new size and no list", m.SizeBytes, err, p.lists)
	}

	write("b", "1")
	s.listedAt = time.Time{} // as if the last list were long ago
	if _, _, err := s.Find("b"); err != nil || p.lists != 2 {
		t.Fatalf("Find(b) = %v after %d lists; want it found by one more list", err, p.lists)
	}

	os.Remove(filepath.Join(dir, "a"))
	s.listedAt = time.Time{}
	if _, _, err := s.Find("a"); err != ErrNotFound || p.lists != 3 {
		t.Fatalf("Find(gone) = %v after %d lists; want not found by one more list", err, p.lists)
	}
}

// A miss right after a List does not list again: a chat asked for every
// 500 ms before its first line is written would list on every ask.
func TestFindMissesWithoutRelistingRightAway(t *testing.T) {
	p := &counting{dir: t.TempDir()}
	s := NewStore(refreshing{p})
	s.List()
	for range 5 {
		if _, _, err := s.Find("nope"); err != ErrNotFound {
			t.Fatal(err)
		}
	}
	if p.lists != 1 {
		t.Errorf("%d lists for five misses straight after one, want 1", p.lists)
	}
}

// An agent without Refresh is found by listing, as before.
func TestFindListsForAnAgentWithoutRefresh(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("1"), 0o644)
	p := &counting{dir: dir}
	s := NewStore(p)
	s.List()
	s.listedAt = time.Time{}
	if _, _, err := s.Find("a"); err != nil || p.lists != 2 {
		t.Fatalf("Find(a) = %v after %d lists; want found by listing", err, p.lists)
	}
}
