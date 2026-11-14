package git

import (
	"context"
	"os/exec"
	"slices"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func head(t *testing.T, dir string) string {
	t.Helper()
	sha, err := resolve(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// Only the user's commits are reachable, and a lockfile's lines don't count.
func TestReachableIsYoursWithoutLockfileLines(t *testing.T) {
	dir, _ := repo(t)
	ctx := context.Background()
	first := head(t, dir)

	write(t, dir, "kept.txt", "one\nTWO\nthree\nfour\n")
	write(t, dir, "bun.lock", "a\nb\nc\nd\ne\n")
	commit(t, dir, "mine")
	mine := head(t, dir)

	write(t, dir, "theirs.txt", "x\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "-c", "user.email=someone@else.com", "commit", "-qm", "theirs")

	shas, err := Reachable(ctx, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(shas)
	want := []string{first, mine}
	slices.Sort(want)
	if !slices.Equal(shas, want) {
		t.Fatalf("reachable %v, want mine and first %v", shas, want)
	}
	read, err := ReadCommits(ctx, dir, []string{mine})
	if err != nil || len(read) != 1 {
		t.Fatalf("read %v, %v", read, err)
	}
	if read[0].SHA != mine || read[0].Added != 2 || read[0].Removed != 1 || read[0].AtMs == 0 {
		t.Fatalf("mine read as %+v, want +2 -1 (bun.lock skipped)", read[0])
	}
}

// A commit on a branch that isn't checked out counts; once amended away it
// is no longer reachable, and the stamp moves with it.
func TestReachableFollowsEveryBranch(t *testing.T) {
	dir, _ := repo(t)
	ctx := context.Background()
	gitIn(t, dir, "switch", "-q", "-c", "feature")
	write(t, dir, "feature.txt", "f\n")
	commit(t, dir, "feature work")
	feature := head(t, dir)
	gitIn(t, dir, "switch", "-q", "main")

	stamp := CommitStamp(ctx, dir)
	shas, _ := Reachable(ctx, dir, 0)
	if !slices.Contains(shas, feature) {
		t.Fatalf("feature commit %s not reachable from main's checkout: %v", feature, shas)
	}

	gitIn(t, dir, "switch", "-q", "feature")
	gitIn(t, dir, "commit", "-q", "--amend", "-m", "feature work, amended")
	shas, _ = Reachable(ctx, dir, 0)
	if slices.Contains(shas, feature) {
		t.Fatalf("amended-away commit %s still reachable", feature)
	}
	if CommitStamp(ctx, dir) == stamp {
		t.Fatal("stamp did not move with the branch")
	}
}

// A repository with no user.email has no commits that are yours.
func TestReachableWithoutEmailIsEmpty(t *testing.T) {
	dir, _ := repo(t)
	exec.Command("git", "-C", dir, "config", "--unset", "user.email").Run()
	t.Setenv("HOME", t.TempDir()) // no global email either
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	shas, err := Reachable(context.Background(), dir, 0)
	if err != nil || len(shas) != 0 {
		t.Fatalf("got %v, %v; want none", shas, err)
	}
}

func TestParseCommitsSkipsBinaries(t *testing.T) {
	out := "\x1eabc\x1f1700000000\n\n3\t1\tsrc/a.go\n-\t-\timage.png\n10\t0\tweb/package-lock.json\n"
	got := parseCommits(out)
	if len(got) != 1 || got[0].SHA != "abc" || got[0].AtMs != 1_700_000_000_000 || got[0].Added != 3 || got[0].Removed != 1 {
		t.Fatalf("got %+v", got)
	}
}
