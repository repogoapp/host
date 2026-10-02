package git

import (
	"context"
	"strings"
)

// Ignored reports which of paths, relative to dir, git ignores; a folder is
// named with a trailing slash. Tracked files are never ignored, as in git
// itself. A dir outside any repository ignores nothing.
func (s *Service) Ignored(ctx context.Context, dir string, paths []string) (map[string]bool, error) {
	ignored := map[string]bool{}
	if len(paths) == 0 {
		return ignored, nil
	}
	dir, err := s.paths.Contain(dir)
	if err != nil {
		return nil, err
	}
	if _, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return ignored, nil
	}
	// -n prints every path, matched or not, so success is exit 0 and an empty
	// reply is a failure rather than "nothing ignored".
	out, err := runInput(ctx, dir, strings.Join(paths, "\x00")+"\x00",
		"check-ignore", "--stdin", "-z", "--verbose", "--non-matching")
	if err != nil {
		return nil, err
	}
	// Each path is four fields: source, line, pattern, path. A "!" pattern
	// matched to un-ignore it.
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+3 < len(fields); i += 4 {
		pattern, path := fields[i+2], fields[i+3]
		if pattern != "" && !strings.HasPrefix(pattern, "!") {
			ignored[path] = true
		}
	}
	return ignored, nil
}
