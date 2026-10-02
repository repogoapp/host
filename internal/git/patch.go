package git

import (
	"context"
	"strconv"
	"strings"
)

// Patch is one change-set's contents: its files, or one file's hunks.
type Patch struct {
	Base string `json:"base"`
	Head string `json:"head,omitempty"`

	// Files is set when no single file was asked for.
	Files []FileChange `json:"files,omitempty"`

	// Text is the unified diff for one file. Passed through as git wrote it:
	// every tool that renders a diff already reads this format, and a parsed
	// representation would be a second grammar to agree on.
	Text string `json:"text,omitempty"`

	// Truncated means the patch was longer than a client can usefully show.
	Truncated bool `json:"truncated,omitempty"`
}

// maxPatchBytes bounds one file's diff. A generated lockfile or a vendored
// bundle is megabytes of hunks nobody scrolls; the cap keeps one bad file from
// making the relay carry it.
const maxPatchBytes = 512 << 10

// Patch returns what changed between two points, or inside one file of it.
// `head` empty means the working tree.
func (s *Service) Patch(ctx context.Context, path, base, head, file string) (Patch, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return Patch{}, err
	}
	if base == "" {
		base = "HEAD"
	}
	for _, rev := range []string{base, head} {
		if err := CheckRevision(rev); err != nil {
			return Patch{}, err
		}
	}
	out := Patch{Base: base, Head: head}

	args := []string{"diff"}
	if file == "" {
		// NUL-separated, or a rename comes back as one path string in git's
		// shorthand, `cmd/{old => new}/main.go`, which names no real file.
		args = append(args, "--numstat", "-z")
	} else {
		// No colour, no pager, and no rename detection surprises: the client
		// renders this, so it wants the plain form.
		args = append(args, "--no-color")
	}
	args = append(args, "--end-of-options", base)
	if head != "" {
		args = append(args, head)
	}
	if file != "" {
		args = append(args, "--", file)
	}

	raw, err := run(ctx, dir, args...)
	if err != nil {
		return out, err
	}

	if file == "" {
		out.Files = numstatFiles(raw)
		// Untracked files are listed too, or the list disagrees with the header
		// `workingSet` counted.
		if head == "" {
			untracked, _ := untrackedFiles(ctx, dir)
			out.Files = append(out.Files, untracked...)
		}
		return out, nil
	}

	if len(raw) > maxPatchBytes {
		raw = raw[:maxPatchBytes]
		out.Truncated = true
	}
	out.Text = raw

	// An untracked file has no diff against anything git knows, and most of a
	// working tree is untracked while a chat is running.
	if strings.TrimSpace(out.Text) == "" && head == "" {
		added, err := runDiff(ctx, dir, "diff", "--no-color", "--no-index", "--", devNull, file)
		if err == nil {
			out.Text = added
			if len(out.Text) > maxPatchBytes {
				out.Text = out.Text[:maxPatchBytes]
				out.Truncated = true
			}
		}
	}
	return out, nil
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
			change.Added = atoi(fields[0])
			change.Removed = atoi(fields[1])
		}
		out = append(out, change)
	}
	return out
}

// runDiff keeps the output of a diff that reported differences: non-empty
// stdout is the diff, git exited 1 to say the files differ. Empty stdout with
// an error is a real failure and still surfaces.
func runDiff(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := Run(ctx, dir, args...)
	if err != nil && strings.TrimSpace(out) != "" {
		return out, nil
	}
	return out, err
}

func atoi(value string) int {
	n, _ := strconv.Atoi(value)
	return n
}
