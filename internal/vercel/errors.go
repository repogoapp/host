package vercel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// ErrorWindow is how far back a card counts runtime errors.
const ErrorWindow = 15 * time.Minute

// RuntimeErrors is the 5xx responses a project served in the window: how
// many, the path that failed most, and when the first one was.
type RuntimeErrors struct {
	Count int
	Path  string
	At    time.Time
}

// ReadRuntimeErrors counts production 5xx responses over ErrorWindow. It asks
// by status, not level: a 5xx that never calls console.error is logged as info.
func ReadRuntimeErrors(ctx context.Context, teamID, projectID string) (RuntimeErrors, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return readErrorsWith(ctx, teamID, projectID, runCLI)
}

func readErrorsWith(ctx context.Context, teamID, projectID string, run func(context.Context, []string) ([]byte, error)) (RuntimeErrors, error) {
	out, err := run(ctx, []string{"logs", "--project", projectID, "--scope", teamID, "--environment", "production",
		"--status-code", "5xx", "--since", strconv.Itoa(int(ErrorWindow.Minutes())) + "m", "--json", "--non-interactive"})
	if err != nil {
		return RuntimeErrors{}, err
	}
	var errs RuntimeErrors
	byPath := map[string]int{}
	lines := bufio.NewScanner(bytes.NewReader(out))
	lines.Buffer(make([]byte, 64*1024), 1024*1024)
	for lines.Scan() {
		var entry struct {
			Path      string `json:"requestPath"`
			Status    int    `json:"responseStatusCode"`
			Timestamp int64  `json:"timestamp"`
		}
		if json.Unmarshal(lines.Bytes(), &entry) != nil || entry.Status < 500 {
			continue
		}
		errs.Count++
		byPath[entry.Path]++
		if at := time.UnixMilli(entry.Timestamp); errs.At.IsZero() || at.Before(errs.At) {
			errs.At = at
		}
	}
	for path, n := range byPath {
		if n > byPath[errs.Path] || (n == byPath[errs.Path] && path < errs.Path) {
			errs.Path = path
		}
	}
	return errs, nil
}
