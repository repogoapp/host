package shipping

import (
	"context"
	"slices"
	"testing"
	"time"
)

// fakeCommits serves fixed commits per folder and counts the reads.
type fakeCommits struct {
	dirs    []string
	stamps  map[string]string
	commits map[string][]Commit // what each folder's branches hold
	reads   map[string][][]string
}

func (f *fakeCommits) Checkouts() ([]string, error) { return f.dirs, nil }

func (f *fakeCommits) Stamp(_ context.Context, dir string) string { return f.stamps[dir] }

func (f *fakeCommits) Reachable(_ context.Context, dir string, _ int64) ([]string, error) {
	var shas []string
	for _, c := range f.commits[dir] {
		shas = append(shas, c.SHA)
	}
	return shas, nil
}

func (f *fakeCommits) Read(_ context.Context, dir string, shas []string) ([]Commit, error) {
	f.reads[dir] = append(f.reads[dir], shas)
	var out []Commit
	for _, c := range f.commits[dir] {
		if slices.Contains(shas, c.SHA) {
			out = append(out, c)
		}
	}
	return out, nil
}

func shasOf(commits []Commit) []string {
	out := make([]string, len(commits))
	for i, c := range commits {
		out[i] = c.SHA
	}
	return out
}

// Two checkouts of one repo hold the same commit; History counts it once,
// and a project's scope sees only its own folder's.
func TestCommitsCountOnceAndFollowScope(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	shared := Commit{SHA: "aaa", AtMs: now.Add(-time.Hour).UnixMilli(), Added: 5, Removed: 2}
	src := &fakeCommits{
		dirs:   []string{"/code/app", "/code/app-copy", "/code/other"},
		stamps: map[string]string{"/code/app": "1", "/code/app-copy": "1", "/code/other": "1"},
		commits: map[string][]Commit{
			"/code/app":      {shared},
			"/code/app-copy": {shared},
			"/code/other":    {{SHA: "bbb", AtMs: now.Add(-2 * time.Hour).UnixMilli(), Added: 1}},
		},
		reads: map[string][][]string{},
	}
	l.ReadCommits(src)
	if err := l.sweepCommits(ctx, now); err != nil {
		t.Fatal(err)
	}

	since, until := now.Add(-24*time.Hour).UnixMilli(), now.UnixMilli()
	all, err := l.History(ctx, since, until, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Commits) != 2 || all.Commits[0].SHA != "bbb" || all.Commits[1] != shared {
		t.Fatalf("all: %+v, want bbb then aaa once", all.Commits)
	}
	app, err := l.History(ctx, since, until, Folders("/code/app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Commits) != 1 || app.Commits[0].SHA != "aaa" {
		t.Fatalf("app: %+v, want aaa", app.Commits)
	}
}

// A folder is read only when its stamp moves; then only new commits are read,
// an old one newly on a branch is added whatever its date, and one no branch
// holds any more is removed.
func TestCommitsStayInStepWithBranches(t *testing.T) {
	l := openTestLedger(t, t.TempDir(), rates{})
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	recent := Commit{SHA: "aaa", AtMs: now.Add(-time.Hour).UnixMilli()}
	amended := Commit{SHA: "bbb", AtMs: now.Add(-30 * time.Minute).UnixMilli()}
	src := &fakeCommits{
		dirs:    []string{"/code/app"},
		stamps:  map[string]string{"/code/app": "1"},
		commits: map[string][]Commit{"/code/app": {recent, amended}},
		reads:   map[string][][]string{},
	}
	l.ReadCommits(src)
	for range 2 {
		if err := l.sweepCommits(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(src.reads["/code/app"]) != 1 {
		t.Fatalf("read %d times with an unmoved stamp, want once", len(src.reads["/code/app"]))
	}

	// bbb is amended into ccc, and a pull brings in a month-old commit.
	pulled := Commit{SHA: "old", AtMs: now.Add(-30 * 24 * time.Hour).UnixMilli()}
	replacement := Commit{SHA: "ccc", AtMs: amended.AtMs}
	src.commits["/code/app"] = []Commit{recent, replacement, pulled}
	src.stamps["/code/app"] = "2"
	if err := l.sweepCommits(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := src.reads["/code/app"][1]; slices.Contains(got, "aaa") {
		t.Fatalf("second read %v re-read a stored commit", got)
	}
	h, err := l.History(ctx, now.Add(-60*24*time.Hour).UnixMilli(), now.UnixMilli(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if got := shasOf(h.Commits); !slices.Equal(got, []string{"old", "aaa", "ccc"}) {
		t.Fatalf("commits %v, want old, aaa, ccc (bbb gone)", got)
	}
}
