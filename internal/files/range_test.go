package files_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/files"
)

func TestReadRangeReadsAFilePastTheReadLimitInPieces(t *testing.T) {
	svc, root, _ := project(t)
	big := filepath.Join(root, "movie.mp4")
	want := bytes.Repeat([]byte("0123456789"), files.MaxFileBytes/10+1000)
	if err := os.WriteFile(big, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Read(big); !errors.Is(err, files.ErrTooLarge) {
		t.Fatalf("whole read: %v, want ErrTooLarge", err)
	}

	var got []byte
	for offset := int64(0); ; {
		piece, err := svc.ReadRange(big, files.Scope{}, offset, 1<<20)
		if err != nil {
			t.Fatalf("range at %d: %v", offset, err)
		}
		if piece.Size != int64(len(want)) {
			t.Fatalf("size = %d, want %d", piece.Size, len(want))
		}
		if len(piece.Content) == 0 {
			break
		}
		got = append(got, piece.Content...)
		offset += int64(len(piece.Content))
	}
	if !bytes.Equal(got, want) {
		t.Errorf("pieces joined to %d bytes, want the file's %d", len(got), len(want))
	}
}

func TestReadRangeCapsOnePiece(t *testing.T) {
	svc, root, _ := project(t)
	big := filepath.Join(root, "big.bin")
	if err := os.WriteFile(big, make([]byte, files.MaxRangeBytes+100), 0o644); err != nil {
		t.Fatal(err)
	}
	piece, err := svc.ReadRange(big, files.Scope{}, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(piece.Content) != files.MaxRangeBytes {
		t.Errorf("piece = %d bytes, want the cap %d", len(piece.Content), files.MaxRangeBytes)
	}
}

func TestReadRangeKeepsReadsContainment(t *testing.T) {
	svc, root, secret := project(t)
	if _, err := svc.ReadRange(secret, files.Scope{}, 0, 10); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("range outside the roots: %v, want ErrOutsideRoots", err)
	}
	if _, err := svc.ReadRange(filepath.Join(root, "src"), files.Scope{}, 0, 10); !errors.Is(err, files.ErrIsDirectory) {
		t.Errorf("range of a folder: %v, want ErrIsDirectory", err)
	}
	if _, err := svc.ReadRange(filepath.Join(root, "src", "main.go"), files.Scope{}, -1, 10); !errors.Is(err, files.ErrInvalidOperation) {
		t.Errorf("negative offset: %v, want ErrInvalidOperation", err)
	}
}
