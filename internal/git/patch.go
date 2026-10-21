package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Patch is one file's hunks. The file list is `Changes`, which carries each
// file's porcelain code; a second list here would disagree with it.
type Patch struct {
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

// Patch returns what changed in one file between two points. `head` empty
// means the working tree.
func (s *Service) Patch(ctx context.Context, path, base, head, file string) (Patch, error) {
	dir, err := s.paths.Contain(path)
	if err != nil {
		return Patch{}, err
	}
	if file == "" {
		return Patch{}, fmt.Errorf("%w: a file is required", ErrInvalidMutation)
	}
	// A repo with no commits yet diffs against the empty tree, as Changes does.
	if base == "" || base == "HEAD" {
		if base, err = diffBase(ctx, dir); err != nil {
			return Patch{}, err
		}
	}
	for _, rev := range []string{base, head} {
		if err := CheckRevision(rev); err != nil {
			return Patch{}, err
		}
	}
	// No colour, no pager, and no rename detection surprises: the client
	// renders this, so it wants the plain form.
	args := []string{"diff", "--no-color", "--end-of-options", base}
	if head != "" {
		args = append(args, head)
	}
	args = append(args, "--", file)

	text, err := run(ctx, dir, args...)
	if err != nil {
		return Patch{}, err
	}

	// An untracked file has no diff against anything git knows, and most of a
	// working tree is untracked while a chat is running.
	if strings.TrimSpace(text) == "" && head == "" {
		// --no-index reads the disk, not the index, so a symlinked folder could
		// lead it out of the project; the same containment every path gets.
		if _, err := s.paths.Contain(filepath.Join(dir, file)); err != nil {
			return Patch{}, err
		}
		if added, err := runDiff(ctx, dir, "diff", "--no-color", "--no-index", "--", devNull, file); err == nil {
			text = added
		}
	}
	if len(text) > maxPatchBytes {
		return Patch{Text: text[:maxPatchBytes], Truncated: true}, nil
	}
	return Patch{Text: text}, nil
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
