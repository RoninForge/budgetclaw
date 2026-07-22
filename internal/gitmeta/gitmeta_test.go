package gitmeta

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.co",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.co",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// buildRepo constructs a repo with: a merge-commit PR (#12, branch feature/foo), a
// squash PR (#34, branch deleted), and an open branch (open/baz).
func buildRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "t@e.co")
	run(t, dir, "config", "user.name", "t")
	write(t, dir, "a.txt", "a\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "init")

	// Merge-commit PR #12 on feature/foo (two commits).
	run(t, dir, "checkout", "-q", "-b", "feature/foo")
	write(t, dir, "foo.txt", "foo\nfoo2\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "foo 1")
	write(t, dir, "foo.txt", "foo\nfoo2\nfoo3\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "foo 2")
	run(t, dir, "checkout", "-q", "main")
	run(t, dir, "merge", "-q", "--no-ff", "feature/foo", "-m", "Merge pull request #12 from owner/feature/foo")

	// Squash PR #34: merge --squash then a single commit whose subject ends "(#34)";
	// then delete the head branch so it looks like a real squash-merge.
	run(t, dir, "checkout", "-q", "-b", "bar")
	write(t, dir, "bar.txt", "bar\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "bar work")
	run(t, dir, "checkout", "-q", "main")
	run(t, dir, "merge", "-q", "--squash", "bar")
	run(t, dir, "commit", "-q", "-m", "Add bar thing (#34)")
	run(t, dir, "branch", "-q", "-D", "bar")

	// Open branch with a commit, not merged.
	run(t, dir, "checkout", "-q", "-b", "open/baz")
	write(t, dir, "baz.txt", "baz\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "baz wip")
	run(t, dir, "checkout", "-q", "main")
	return dir
}

func TestCollect(t *testing.T) {
	dir := buildRepo(t)
	keys := []SpendKey{
		{Project: "proj", Branch: "feature/foo", CWD: dir},
		{Project: "proj", Branch: "open/baz", CWD: dir},
		{Project: "proj", Branch: "bar", CWD: dir}, // squash-merged, branch gone
	}
	since := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := Collect(context.Background(), keys, since)

	byBranch := map[string]PRRecord{}
	var squashed []PRRecord
	for _, r := range recs {
		if r.State == "squashed" {
			squashed = append(squashed, r)
			continue
		}
		byBranch[r.Branch] = r
	}

	// Merged PR
	foo, ok := byBranch["feature/foo"]
	if !ok {
		t.Fatalf("feature/foo not collected; got %+v", recs)
	}
	if foo.State != "merged" || foo.PR != 12 || foo.Base != "main" {
		t.Errorf("merged PR wrong: %+v", foo)
	}
	if foo.Commits != 2 {
		t.Errorf("merged commits = %d, want 2", foo.Commits)
	}
	if foo.Additions == 0 {
		t.Errorf("merged additions should be > 0: %+v", foo)
	}

	// Open branch
	baz, ok := byBranch["open/baz"]
	if !ok {
		t.Fatalf("open/baz not collected; got %+v", recs)
	}
	if baz.State != "open" || baz.PR != 0 {
		t.Errorf("open branch wrong: %+v", baz)
	}
	if baz.Commits != 1 {
		t.Errorf("open commits = %d, want 1", baz.Commits)
	}

	// Squash PR (branch-less)
	if len(squashed) != 1 {
		t.Fatalf("want 1 squashed PR, got %d: %+v", len(squashed), squashed)
	}
	if squashed[0].PR != 34 || squashed[0].Branch != "" {
		t.Errorf("squashed PR wrong: %+v", squashed[0])
	}

	// The deleted 'bar' branch must NOT appear as an open/merged record.
	if _, present := byBranch["bar"]; present {
		t.Errorf("gone squash branch 'bar' should not be reported as a branch record")
	}
}

func TestIsSafeRef(t *testing.T) {
	unsafe := []string{"-x", "--output=/tmp/pwned", "-", "a..b", "a b", "a\tb", "", "x\x00y"}
	for _, s := range unsafe {
		if isSafeRef(s) {
			t.Errorf("isSafeRef(%q) = true, want false", s)
		}
	}
	safe := []string{"main", "master", "develop", "feature/foo", "release-1.2"}
	for _, s := range safe {
		if !isSafeRef(s) {
			t.Errorf("isSafeRef(%q) = false, want true", s)
		}
	}
}

// A malicious remote can advertise a default-branch symref that begins with "-", which
// would be an arbitrary-file-write primitive if base were passed to git unguarded (e.g.
// "--output=<file>"). Collect must not honor it: base is rejected and falls back to main,
// and no file is written.
func TestCollectRejectsHostileDefaultBranch(t *testing.T) {
	dir := buildRepo(t)
	sentinel := filepath.Join(t.TempDir(), "pwned")
	// Point origin/HEAD at a ref whose name is a git option. Best-effort: if this git
	// refuses to store it, the isSafeRef unit test above still guards the code path.
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD",
		"refs/remotes/origin/--output="+sentinel)
	cmd.Dir = dir
	_ = cmd.Run()

	recs := Collect(context.Background(),
		[]SpendKey{{Project: "proj", Branch: "feature/foo", CWD: dir}},
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("arg-injection: a file was written via a hostile default-branch ref")
	}
	// base fell back to main, so the merged PR is still found normally.
	var foundMerged bool
	for _, r := range recs {
		if r.Branch == "feature/foo" && r.State == "merged" {
			foundMerged = true
		}
	}
	if !foundMerged {
		t.Errorf("expected the merged PR to still be collected via the main fallback; got %+v", recs)
	}
}

func TestCollectNonGitDirIsSafe(t *testing.T) {
	dir := t.TempDir() // not a git repo
	recs := Collect(context.Background(), []SpendKey{{Project: "p", Branch: "b", CWD: dir}},
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(recs) != 0 {
		t.Fatalf("a non-git dir must yield no records, got %+v", recs)
	}
}
