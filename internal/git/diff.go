package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ChangeSet is the working tree's uncommitted edits against HEAD, totalled.
type ChangeSet struct {
	FilesChanged int `json:"files_changed"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
}

// Diff answers what is uncommitted in a project.
func (s *Service) Diff(ctx context.Context, path string) (ChangeSet, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return ChangeSet{}, err
	}
	if _, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return ChangeSet{}, nil // A plain folder has no diff, and that is not an error.
	}
	return workingSet(ctx, dir)
}

// WorkingDiff reads totals for the whole checkout, including when path is a subfolder.
func (s *Service) WorkingDiff(ctx context.Context, path string) (*ChangeSet, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return nil, err
	}
	root, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	set, err := workingSet(ctx, strings.TrimSpace(root))
	if err != nil {
		return nil, err
	}
	return &set, nil
}

// workingSet is everything uncommitted. `diff HEAD` covers staged and unstaged
// together; untracked files are counted separately because `git diff` cannot
// see them.
func workingSet(ctx context.Context, dir string) (ChangeSet, error) {
	var set ChangeSet
	base, err := diffBase(ctx, dir)
	if err != nil {
		return set, err
	}
	stats, err := run(ctx, dir, "diff", "--no-ext-diff", "--numstat", "-z", base, "--")
	if err != nil {
		return set, err
	}
	untracked, err := untrackedFiles(ctx, dir)
	if err != nil {
		return set, err
	}
	for _, change := range append(numstatFiles(stats), untracked...) {
		set.FilesChanged++
		set.Additions += change.Additions
		set.Deletions += change.Deletions
	}
	return set, nil
}

// numstatFiles parses `--numstat -z`: "added\tremoved\tpath" records. A rename
// leaves the path empty and puts old and new in the next two records; the new
// one exists, so it is listed. Binary files report "-" for both counts.
func numstatFiles(stats string) []FileChange {
	var out []FileChange
	records := strings.Split(stats, "\x00")
	for i := 0; i < len(records); i++ {
		fields := strings.SplitN(records[i], "\t", 3)
		if len(fields) != 3 {
			continue
		}
		path := fields[2]
		if path == "" {
			if i+2 >= len(records) {
				break
			}
			path = records[i+2]
			i += 2
		}
		change := FileChange{Path: path}
		if fields[0] == "-" || fields[1] == "-" {
			change.Binary = true
		} else {
			change.Additions = atoi(fields[0])
			change.Deletions = atoi(fields[1])
		}
		out = append(out, change)
	}
	return out
}

func atoi(value string) int {
	n, _ := strconv.Atoi(value)
	return n
}

// diffBase is what the working tree is compared with: HEAD, or on an unborn
// branch an empty tree, so staged files still count.
func diffBase(ctx context.Context, dir string) (string, error) {
	if _, err := resolve(ctx, dir, "HEAD"); err == nil {
		return "HEAD", nil
	}
	empty, err := run(ctx, dir, "hash-object", "-w", "-t", "tree", "--stdin")
	return strings.TrimSpace(empty), err
}

// untrackedFiles lists what git is not tracking, every line as an addition. Read
// from disk rather than `git add -N`, which would mutate the user's index.
func untrackedFiles(ctx context.Context, dir string) ([]FileChange, error) {
	list, err := run(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var out []FileChange
	for _, name := range strings.Split(list, "\x00") {
		if name == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lines, _ := countLines(filepath.Join(dir, name))
		out = append(out, FileChange{Path: name, Code: "??", Additions: lines})
	}
	return out, nil
}

const devNull = "/dev/null"

// countLines is how many lines a new file adds. Streamed in chunks so a large
// generated file costs a buffer, not its size; a missing trailing newline still
// counts the last line, as a diff would.
func countLines(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 1, nil // A symlink adds its target name, not the target's contents.
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("git: cannot count non-file %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, 32<<10)
	lines, seen := 0, false
	for {
		n, err := f.Read(buf)
		if bytes.IndexByte(buf[:n], 0) >= 0 {
			return 0, nil
		}
		for _, b := range buf[:n] {
			seen = true
			if b == '\n' {
				lines++
				seen = false
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if seen {
		lines++
	}
	return lines, nil
}
