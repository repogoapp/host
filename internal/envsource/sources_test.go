package envsource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSources is a store confined to a temporary home, with its bindings file
// outside it, as ~/.repogo would be.
func newSources(t *testing.T) (*Sources, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(t.TempDir(), "env-sources.json"), home)
	if err != nil {
		t.Fatal(err)
	}
	return s, home
}

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetNormalizesAndPersistsPrivately(t *testing.T) {
	s, home := newSources(t)
	env := write(t, filepath.Join(home, "app", ".env"), "KEY=value\n")

	src, err := s.Set("  My Secrets!! ", env)
	if err != nil {
		t.Fatal(err)
	}
	if src.Handle != "my-secrets" || src.Path != env || !src.Exists {
		t.Fatalf("source = %+v", src)
	}
	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("bindings file mode = %o, want 600", mode)
	}

	reopened, err := Open(s.path, home)
	if err != nil {
		t.Fatal(err)
	}
	if list := reopened.List(); len(list) != 1 || list[0].Handle != "my-secrets" {
		t.Fatalf("reopened list = %+v", list)
	}
	// Bindings name a file; its values never go in the bindings file.
	if b, _ := os.ReadFile(s.path); strings.Contains(string(b), "value") {
		t.Fatalf("bindings file holds a value: %s", b)
	}
}

func TestSetRefusesWhatIsNotADotenvUnderHome(t *testing.T) {
	s, home := newSources(t)
	outside := write(t, filepath.Join(t.TempDir(), ".env"), "KEY=v\n")
	link := filepath.Join(home, "link.env")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	big := write(t, filepath.Join(home, "big.env"), "KEY="+strings.Repeat("x", MaxBytes)+"\n")
	binary := write(t, filepath.Join(home, "bin.env"), "KEY=\x00\n")
	latin := write(t, filepath.Join(home, "latin.env"), "KEY=\xff\n")

	cases := map[string]struct {
		path string
		want error
	}{
		"empty handle":   {write(t, filepath.Join(home, "ok.env"), "A=1"), ErrInvalid},
		"relative":       {"app/.env", ErrInvalid},
		"outside home":   {outside, ErrOutsideHome},
		"link out":       {link, ErrOutsideHome},
		"missing":        {filepath.Join(home, "nope.env"), ErrInvalid},
		"folder":         {home, ErrInvalid},
		"too large":      {big, ErrInvalid},
		"nul byte":       {binary, ErrInvalid},
		"not utf-8 text": {latin, ErrInvalid},
	}
	for name, c := range cases {
		handle := "h"
		if name == "empty handle" {
			handle = "!!"
		}
		if _, err := s.Set(handle, c.path); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("a refused binding was kept: %+v", list)
	}
}

func TestReadReportsEachHandle(t *testing.T) {
	s, home := newSources(t)
	good := write(t, filepath.Join(home, "good.env"), "export A=1\nB='two'\n")
	gone := write(t, filepath.Join(home, "gone.env"), "C=3\n")
	turned := write(t, filepath.Join(home, "turned.env"), "D=4\n")
	for handle, path := range map[string]string{"good": good, "gone": gone, "turned": turned} {
		if _, err := s.Set(handle, path); err != nil {
			t.Fatal(err)
		}
	}
	// Both change after binding; read checks again.
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	write(t, turned, "D=\x00")

	got := s.Read([]string{"Good", "gone", "turned", "unknown"})
	if r := got["Good"]; !r.OK || r.Vars["A"] != "1" || r.Vars["B"] != "two" {
		t.Errorf("Good = %+v", r)
	}
	for handle, want := range map[string]string{"gone": Missing, "turned": Binary, "unknown": NotBound} {
		if r := got[handle]; r.OK || r.Error != want || r.Vars == nil {
			t.Errorf("%s = %+v, want error %s and empty vars", handle, r, want)
		}
	}
}

func TestRemove(t *testing.T) {
	s, home := newSources(t)
	if _, err := s.Set("a", write(t, filepath.Join(home, "a.env"), "A=1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove = %v, want not found", err)
	}
}
