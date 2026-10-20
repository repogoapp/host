package uploads

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/errkind"
)

const (
	phone   = "phone"
	other   = "other-phone"
	id      = "0b3c2d1e-4f5a-4b6c-8d7e-9f0a1b2c3d4e"
	project = "/work/shop"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "uploads"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func chunk(offset int64, data string, done bool) Chunk {
	return Chunk{Device: phone, Project: project, ID: id, Name: "receipt.pdf", Type: "application/pdf",
		Size: 10, Offset: offset, Data: []byte(data), Done: done}
}

func write(t *testing.T, s *Store, c Chunk) Progress {
	t.Helper()
	p, err := s.Write(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWriteInChunks(t *testing.T) {
	s := open(t)
	if p := write(t, s, chunk(0, "hello", false)); p != (Progress{Received: 5}) {
		t.Fatalf("first chunk: %+v", p)
	}
	// A repeat after a lost reply, and a gap, write nothing and say where to go on.
	if p := write(t, s, chunk(0, "hello", false)); p != (Progress{Received: 5}) {
		t.Fatalf("repeat: %+v", p)
	}
	if p := write(t, s, chunk(8, "ld", false)); p != (Progress{Received: 5}) {
		t.Fatalf("gap: %+v", p)
	}
	if p := write(t, s, chunk(5, "world", true)); p != (Progress{Received: 10, Complete: true}) {
		t.Fatalf("last chunk: %+v", p)
	}
	// The last chunk again, its reply lost, answers complete.
	if p := write(t, s, chunk(5, "world", true)); p != (Progress{Received: 10, Complete: true}) {
		t.Fatalf("last chunk again: %+v", p)
	}
	files, release, err := s.Hold(phone, project+"/", []string{id})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	b, err := os.ReadFile(files[0].Path)
	if err != nil || string(b) != "helloworld" {
		t.Fatalf("file = %q, %v", b, err)
	}
	if got := files[0]; got.Name != "receipt.pdf" || got.Type != "application/pdf" || got.Size != 10 || got.ID != id {
		t.Fatalf("file = %+v", got)
	}
}

func TestWriteRefuses(t *testing.T) {
	s := open(t)
	write(t, s, chunk(0, "hello", false))
	cases := map[string]Chunk{
		"not a uuid":    {Device: phone, Project: project, ID: "../x", Size: 1, Data: []byte("a")},
		"empty file":    {Device: phone, Project: project, ID: "1b3c2d1e-4f5a-4b6c-8d7e-9f0a1b2c3d4e", Size: 0},
		"too big":       {Device: phone, Project: project, ID: "1b3c2d1e-4f5a-4b6c-8d7e-9f0a1b2c3d4e", Size: MaxFileBytes + 1},
		"relative path": {Device: phone, Project: "shop", ID: "1b3c2d1e-4f5a-4b6c-8d7e-9f0a1b2c3d4e", Size: 1},
		"size changed":  func() Chunk { c := chunk(5, "world", false); c.Size = 11; return c }(),
		"past the end":  chunk(5, "world!", false),
		"ended early":   chunk(5, "wor", true),
	}
	for name, c := range cases {
		if _, err := s.Write(c); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	stranger := chunk(5, "world", false)
	stranger.Device = other
	if _, err := s.Write(stranger); !errors.Is(err, ErrNotFound) {
		t.Errorf("another device's upload: err = %v", err)
	}
}

func TestHold(t *testing.T) {
	s := open(t)
	write(t, s, chunk(0, "hello", false))
	if _, _, err := s.Hold(phone, project, []string{id}); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("unfinished: err = %v", err)
	}
	write(t, s, chunk(5, "world", true))
	if _, _, err := s.Hold(other, project, []string{id}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another device: err = %v", err)
	}
	if _, _, err := s.Hold(phone, "/work/other", []string{id}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another project: err = %v", err)
	}
	if _, _, err := s.Hold(phone, project, []string{id, id}); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("the same id twice: err = %v", err)
	}
	// A confirm's question and the confirmed call each hold the file.
	for range 2 {
		_, release, err := s.Hold(phone, project, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
}

func TestSweepAndRemove(t *testing.T) {
	s := open(t)
	write(t, s, chunk(0, "helloworld", true))
	_, release, err := s.Hold(phone, project, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(idle + time.Minute)
	s.Sweep(t.Context(), later)
	if err := s.Remove(phone, id); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("remove while held: err = %v", err)
	}
	release()
	s.Sweep(t.Context(), time.Now())
	_, release, err = s.Hold(phone, project, []string{id})
	if err != nil {
		t.Fatalf("a fresh upload survives the sweep: %v", err)
	}
	release()
	s.Sweep(t.Context(), later)
	if _, _, err := s.Hold(phone, project, []string{id}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an idle upload is swept: err = %v", err)
	}

	write(t, s, chunk(0, "hello", false))
	if err := s.Remove(other, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another device's remove: err = %v", err)
	}
	if err := s.Remove(phone, id); err != nil {
		t.Fatal(err)
	}
	if p := write(t, s, chunk(0, "hello", false)); p.Received != 5 {
		t.Fatalf("starts over after a remove: %+v", p)
	}
}

func TestDeviceBound(t *testing.T) {
	s := open(t)
	big := func(n byte) Chunk {
		return Chunk{Device: phone, Project: project, ID: "0b3c2d1e-4f5a-4b6c-8d7e-9f0a1b2c3d4" + string('0'+n),
			Name: "video.mov", Type: "video/quicktime", Size: MaxFileBytes}
	}
	for n := range byte(2) {
		write(t, s, big(n))
	}
	if _, err := s.Write(big(2)); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("a third gigabyte: err = %v", err)
	}
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "passwd",
		"a:b\x00.txt":      "a_b_.txt",
		"meta.json":        "attachment.pdf",
		"":                 "attachment.pdf",
	}
	for in, want := range cases {
		if got := CleanName(in, "application/pdf"); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
