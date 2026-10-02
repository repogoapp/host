package git

import (
	"context"
	"strings"
)

// Listing is what git shows under a folder, relative to it. Files are tracked
// or untracked and not ignored, with an untracked nested repository as one
// entry ending in a slash. Ignored stops at each ignored folder, slash-ended.
type Listing struct {
	Files   []string
	Ignored []string
}

// List reads a folder with ls-files, which never enters an ignored folder.
// Ignored holds only paths matching ignoredPathspecs: a folder of ignored
// files would otherwise list every one of them.
func List(ctx context.Context, dir string, ignoredPathspecs []string) (Listing, error) {
	var ignored string
	var ignoredErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		args := append([]string{"ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory", "--"}, ignoredPathspecs...)
		ignored, ignoredErr = run(ctx, dir, args...)
	}()
	files, err := run(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	<-done
	if err != nil {
		return Listing{}, err
	}
	if ignoredErr != nil {
		return Listing{}, ignoredErr
	}
	return Listing{Files: splitNUL(files), Ignored: splitNUL(ignored)}, nil
}

func splitNUL(out string) []string {
	out = strings.TrimSuffix(out, "\x00")
	if out == "" {
		return []string{}
	}
	return strings.Split(out, "\x00")
}
