// Package gitmeta collects pull-request cost-attribution metadata from local git,
// for Goei's cost-per-PR view. It is opt-in (budgetclaw prs on) and content-free: it
// reads only branch names, merge/squash commit subjects (for the PR number), commit
// counts, and numstat diff sizes. It never reads commit message bodies, code, or diffs
// content. Every git invocation is read-only, time-bounded, and non-fatal: a directory
// that is not a git repo, or a machine without git, simply yields nothing.
//
// The join back to spend is by (project, branch): budgetclaw already sends spend keyed
// by project (basename of the working directory) and git branch, so a PR record tagged
// with the same (project, branch) lets Goei sum that branch's local spend as the PR's
// cost.
package gitmeta

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PRRecord is one pull request (or in-flight branch) with the metadata needed to
// attribute cost to it. State is "merged" (a merge commit named the branch and PR),
// "squashed" (a squash-merge commit on the base branch carried the PR number, but the
// head branch is gone so cost cannot be joined by branch), or "open" (a local branch
// with spend that is not yet merged).
type PRRecord struct {
	Project   string `json:"project"`
	Branch    string `json:"branch,omitempty"`
	PR        int    `json:"pr,omitempty"`
	Base      string `json:"base,omitempty"`
	State     string `json:"state"`
	MergedAt  string `json:"mergedAt,omitempty"`
	Commits   int    `json:"commits"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// SpendKey is one (project, branch) that had spend, with a working directory to locate
// its repository. Mirrors db.RepoSpendKey without the storage dependency.
type SpendKey struct {
	Project string
	Branch  string
	CWD     string
}

const gitTimeout = 5 * time.Second

// maxPRsPerRepo caps how many merged / squashed PRs are parsed per repo per sync, so a
// very active monorepo cannot spawn an unbounded number of git stat subprocesses.
const maxPRsPerRepo = 200

// mergeSubject matches GitHub's default merge-commit subject, capturing the PR number
// and the head branch: "Merge pull request #4312 from owner/feature/checkout".
var mergeSubject = regexp.MustCompile(`^Merge pull request #(\d+) from [^/]+/(.+)$`)

// squashSubject matches the PR number GitHub appends to a squash-merge commit subject:
// "Some title (#4312)". Anchored to the end so a "(#N)" mid-message is ignored.
var squashSubject = regexp.MustCompile(`\(#(\d+)\)\s*$`)

// git runs a read-only git command in dir with a timeout, returning trimmed stdout.
// GIT_OPTIONAL_LOCKS=0 avoids taking index locks that could contend with the user's
// own git. Any error yields ("", err) and the caller treats it as no data.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	full := append([]string{"-C", dir}, args...)
	// The binary is the fixed literal "git" and every argument is a structured value
	// (not a shell string): callers pass validated refs/paths and prefix untrusted
	// revisions with --end-of-options, and no shell is ever involved.
	cmd := exec.CommandContext(cctx, "git", full...) // #nosec G204 -- fixed "git" binary, structured args, no shell
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// projectBranch is one (project, branch) spend key within a resolved repository.
type projectBranch struct{ project, branch string }

// Collect returns PR records for the repositories the spend keys point at. since bounds
// how far back merged/squashed PRs are considered, keeping git work proportional to the
// sync window.
func Collect(ctx context.Context, keys []SpendKey, since time.Time) []PRRecord {
	// Group spend keys by repository root, remembering each (project, branch).
	repos := make(map[string][]projectBranch)
	for _, k := range keys {
		if k.Branch == "" || k.CWD == "" {
			continue
		}
		root, err := git(ctx, k.CWD, "rev-parse", "--show-toplevel")
		if err != nil || root == "" {
			continue
		}
		repos[root] = append(repos[root], projectBranch{k.Project, k.Branch})
	}

	var records []PRRecord
	for root, pairs := range repos {
		records = append(records, collectRepo(ctx, root, pairs, since)...)
	}
	return records
}

func collectRepo(ctx context.Context, root string, pairs []projectBranch, since time.Time) []PRRecord {
	base := defaultBranch(ctx, root)
	if base == "" {
		return nil
	}
	sinceArg := "--since=" + since.UTC().Format(time.RFC3339)

	merged := mergedPRs(ctx, root, base, sinceArg) // head branch -> prInfo
	squashed := squashedPRs(ctx, root, base, sinceArg)

	var out []PRRecord
	seenBranch := make(map[string]bool)

	for _, p := range pairs {
		if seenBranch[p.branch] {
			continue
		}
		seenBranch[p.branch] = true

		if info, ok := merged[p.branch]; ok {
			add, del := mergeStats(ctx, root, info.sha)
			commits := mergeCommits(ctx, root, info.sha)
			out = append(out, PRRecord{
				Project: p.project, Branch: p.branch, PR: info.pr, Base: base,
				State: "merged", MergedAt: info.mergedAt,
				Commits: commits, Additions: add, Deletions: del,
			})
			continue
		}

		// Not merged via a merge commit. If the branch still exists locally and differs
		// from base, it is in flight: report cost-so-far (Goei joins the spend); the PR
		// number is not known until it merges.
		if branchExists(ctx, root, p.branch) && p.branch != base {
			add, del := rangeStats(ctx, root, base, p.branch)
			commits := rangeCommits(ctx, root, base, p.branch)
			if commits == 0 && add == 0 && del == 0 {
				continue // nothing distinct from base; skip noise
			}
			out = append(out, PRRecord{
				Project: p.project, Branch: p.branch, Base: base,
				State: "open", Commits: commits, Additions: add, Deletions: del,
			})
		}
		// Otherwise the branch is gone and was not a merge-commit PR: most likely
		// squash-merged, whose head branch git no longer records. Those PRs are reported
		// below (branch-less), so cost cannot be joined by branch - reported honestly.
	}

	// Squash-merged PRs: the number and diff size are known, the head branch is not.
	// Reported branch-less so Goei can show the PR and its size even though it cannot
	// attribute a per-branch cost to it.
	repoProject := filepath.Base(root)
	for _, s := range squashed {
		out = append(out, PRRecord{
			Project: repoProject, PR: s.pr, Base: base,
			State: "squashed", MergedAt: s.mergedAt,
			Commits: 1, Additions: s.add, Deletions: s.del,
		})
	}
	return out
}

// defaultBranch resolves the repo's base branch: the remote HEAD if set, else main or
// master if present. Returns "" when none can be determined. The remote HEAD symref is
// attacker-influenceable (a malicious/compromised git server can advertise a default
// ref that begins with "-", e.g. "--output=..."), and base flows into git subprocesses
// as a revision argument, so an unsafe value is rejected here rather than trusted.
// Every base sink additionally passes "--end-of-options" before the revision as belt
// and suspenders.
func defaultBranch(ctx context.Context, root string) string {
	if ref, err := git(ctx, root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		b := strings.TrimPrefix(ref, "origin/")
		if isSafeRef(b) {
			return b
		}
	}
	for _, cand := range []string{"main", "master"} {
		if _, err := git(ctx, root, "rev-parse", "--verify", "--quiet", cand); err == nil {
			return cand
		}
	}
	return ""
}

// isSafeRef rejects ref names that could be mistaken for a git option or a second range
// operator when used as a bare revision argument: a leading "-", embedded "..", or any
// whitespace/control character.
func isSafeRef(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

type prInfo struct {
	pr       int
	sha      string
	mergedAt string
}

// mergedPRs parses merge commits on the base branch's first-parent history into a map
// from head branch name to its PR info.
func mergedPRs(ctx context.Context, root, base, sinceArg string) map[string]prInfo {
	out := make(map[string]prInfo)
	// %x1f is a unit separator: sha, subject, committer ISO date. --end-of-options guards
	// base (a revision) from being parsed as an option.
	log, err := git(ctx, root, "log", "--first-parent", "--merges", sinceArg,
		"--format=%H%x1f%s%x1f%cI", "--end-of-options", base)
	if err != nil || log == "" {
		return out
	}
	for _, line := range strings.Split(log, "\n") {
		parts := strings.Split(line, "\x1f")
		if len(parts) != 3 {
			continue
		}
		m := mergeSubject.FindStringSubmatch(parts[1])
		if m == nil {
			continue
		}
		pr, _ := strconv.Atoi(m[1])
		head := strings.TrimSpace(m[2])
		if head == "" {
			continue
		}
		// First (newest) wins if a branch name was reused across PRs.
		if _, seen := out[head]; !seen {
			out[head] = prInfo{pr: pr, sha: parts[0], mergedAt: parts[2]}
		}
		if len(out) >= maxPRsPerRepo {
			break
		}
	}
	return out
}

type squashInfo struct {
	pr       int
	mergedAt string
	add, del int
}

// squashedPRs parses squash-merge commits (non-merge commits on base whose subject ends
// with "(#N)") into a slice of PR info with diff sizes. Unlike merged/open records,
// squash commits carry no head branch, so they cannot be scoped to the caller's own
// spend branches. They are scoped to the LOCAL git author instead, so opting in never
// transmits a teammate's PR metadata; with no local identity, none are collected.
func squashedPRs(ctx context.Context, root, base, sinceArg string) []squashInfo {
	localEmail, err := git(ctx, root, "config", "user.email")
	localEmail = strings.ToLower(strings.TrimSpace(localEmail))
	if err != nil || localEmail == "" {
		return nil
	}

	log, err := git(ctx, root, "log", "--first-parent", "--no-merges", sinceArg,
		"--format=%H%x1f%s%x1f%cI%x1f%ae", "--end-of-options", base)
	if err != nil || log == "" {
		return nil
	}
	var out []squashInfo
	for _, line := range strings.Split(log, "\n") {
		parts := strings.Split(line, "\x1f")
		if len(parts) != 4 {
			continue
		}
		// Only this developer's own squash-merged PRs.
		if strings.ToLower(strings.TrimSpace(parts[3])) != localEmail {
			continue
		}
		m := squashSubject.FindStringSubmatch(parts[1])
		if m == nil {
			continue
		}
		pr, _ := strconv.Atoi(m[1])
		add, del := commitStats(ctx, root, parts[0])
		out = append(out, squashInfo{pr: pr, mergedAt: parts[2], add: add, del: del})
		if len(out) >= maxPRsPerRepo {
			break
		}
	}
	return out
}

func branchExists(ctx context.Context, root, branch string) bool {
	_, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// mergeStats sums additions/deletions of a merge commit against its first parent (the
// net change the PR brought onto the base branch).
func mergeStats(ctx context.Context, root, sha string) (int, int) {
	out, err := git(ctx, root, "show", "--first-parent", "--numstat", "--format=", sha)
	if err != nil {
		return 0, 0
	}
	return sumNumstat(out)
}

func mergeCommits(ctx context.Context, root, sha string) int {
	// Commits the branch brought: first-parent..second-parent of the merge.
	out, err := git(ctx, root, "rev-list", "--count", sha+"^1.."+sha+"^2")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

func commitStats(ctx context.Context, root, sha string) (int, int) {
	out, err := git(ctx, root, "show", "--numstat", "--format=", sha)
	if err != nil {
		return 0, 0
	}
	return sumNumstat(out)
}

func rangeStats(ctx context.Context, root, base, branch string) (int, int) {
	out, err := git(ctx, root, "diff", "--numstat", "--end-of-options", base+"..."+branch)
	if err != nil {
		return 0, 0
	}
	return sumNumstat(out)
}

func rangeCommits(ctx context.Context, root, base, branch string) int {
	out, err := git(ctx, root, "rev-list", "--count", "--end-of-options", base+".."+branch)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// sumNumstat totals the added/deleted columns of `git --numstat` output. Binary files
// show "-\t-" and contribute nothing.
func sumNumstat(s string) (int, int) {
	var add, del int
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if a, err := strconv.Atoi(fields[0]); err == nil {
			add += a
		}
		if d, err := strconv.Atoi(fields[1]); err == nil {
			del += d
		}
	}
	return add, del
}
