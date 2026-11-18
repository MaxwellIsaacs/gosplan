package wt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=master"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a repo on master with a.txt (three lines) and returns it and
// the base SHA.
func newRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	writeFile(t, dir, "a.txt", "one\ntwo\nthree\n")
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-q", "-m", "base")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

// branchWith creates a branch from base with one file changed and returns its SHA.
func branchWith(t *testing.T, repo, base, name, file, content string) string {
	t.Helper()
	git(t, repo, "checkout", "-q", "-b", name, base)
	writeFile(t, repo, file, content)
	git(t, repo, "add", file)
	git(t, repo, "commit", "-q", "-m", name)
	sha := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "-q", "master")
	return sha
}

func TestWtPrepareMergesDeps(t *testing.T) {
	repo, base := newRepo(t)
	d1 := branchWith(t, repo, base, "d1", "d1.txt", "x\n")
	d2 := branchWith(t, repo, base, "d2", "d2.txt", "y\n")
	root := t.TempDir()
	p, err := Git{}.Prepare(context.Background(), core.PrepareRequest{
		Project: repo, Run: "r1", Task: "t1", BaseSHA: base, DepHeads: []string{d1, d2}, Root: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Branch != "gosplan/r1/t1" || p.Dir != filepath.Join(root, "r1", "t1") {
		t.Fatalf("prepared = %+v", p)
	}
	for _, f := range []string{"d1.txt", "d2.txt", "a.txt"} {
		if _, err := os.Stat(filepath.Join(p.Dir, f)); err != nil {
			t.Errorf("missing %s in worktree", f)
		}
	}
	if head := git(t, p.Dir, "rev-parse", "HEAD"); p.BaseSHA != head || p.BaseSHA == base {
		t.Errorf("BaseSHA %s, worktree HEAD %s, run base %s", p.BaseSHA, head, base)
	}
	if got := git(t, repo, "rev-parse", "master"); got != base {
		t.Errorf("master moved to %s", got)
	}
	if got := git(t, repo, "symbolic-ref", "--short", "HEAD"); got != "master" {
		t.Errorf("project checked-out branch is %s", got)
	}
	if st := git(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("project tree dirty: %s", st)
	}

	// Resume: the same request reuses the live worktree.
	again, err := Git{}.Prepare(context.Background(), core.PrepareRequest{
		Project: repo, Run: "r1", Task: "t1", BaseSHA: base, DepHeads: []string{d1, d2}, Root: root,
	})
	if err != nil || again.BaseSHA != p.BaseSHA {
		t.Fatalf("resume = %+v, %v", again, err)
	}
	// Branch exists but worktree is gone: error, not a silent reuse.
	if err := (Git{}).Remove(context.Background(), repo, p.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := (Git{}).Prepare(context.Background(), core.PrepareRequest{
		Project: repo, Run: "r1", Task: "t1", BaseSHA: base, Root: root,
	}); err == nil {
		t.Error("expected error when branch exists without worktree")
	}
}

func TestWtPrepareConflict(t *testing.T) {
	repo, base := newRepo(t)
	d1 := branchWith(t, repo, base, "d1", "a.txt", "one\nTWO-A\nthree\n")
	d2 := branchWith(t, repo, base, "d2", "a.txt", "one\nTWO-B\nthree\n")
	root := t.TempDir()
	_, err := Git{}.Prepare(context.Background(), core.PrepareRequest{
		Project: repo, Run: "r1", Task: "t1", BaseSHA: base, DepHeads: []string{d1, d2}, Root: root,
	})
	if !errors.Is(err, core.ErrMergeConflict) {
		t.Fatalf("err = %v, want ErrMergeConflict", err)
	}
	if !strings.Contains(err.Error(), d2) {
		t.Errorf("error does not name the conflicting head %s: %v", d2, err)
	}
	if _, serr := os.Stat(filepath.Join(root, "r1", "t1")); !os.IsNotExist(serr) {
		t.Errorf("worktree dir left behind: %v", serr)
	}
	if out := git(t, repo, "branch", "--list", "gosplan/r1/t1"); out != "" {
		t.Errorf("branch left behind: %q", out)
	}
	if out := git(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, "r1/t1") {
		t.Errorf("worktree still registered:\n%s", out)
	}
}

func TestWtHeadFullSHA(t *testing.T) {
	repo, base := newRepo(t)
	feat := branchWith(t, repo, base, "feat", "f.txt", "f\n")
	got, err := Git{}.Head(context.Background(), repo, "feat")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(got) || got != feat {
		t.Errorf("Head = %q, want %s", got, feat)
	}
	if _, err := (Git{}).Head(context.Background(), repo, "nope"); err == nil {
		t.Error("missing branch: want error")
	}
	// A tag with the same name must not satisfy a branch lookup.
	git(t, repo, "tag", "onlytag", base)
	if _, err := (Git{}).Head(context.Background(), repo, "onlytag"); err == nil {
		t.Error("tag resolved as a branch")
	}
	// A remote-tracking ref must not either.
	git(t, repo, "update-ref", "refs/remotes/origin/remoteonly", base)
	for _, name := range []string{"remoteonly", "origin/remoteonly"} {
		if _, err := (Git{}).Head(context.Background(), repo, name); err == nil {
			t.Errorf("remote ref %q resolved as a branch", name)
		}
	}
	// Tag shadowing a branch name: the branch wins.
	git(t, repo, "tag", "shadow", base)
	git(t, repo, "branch", "shadow", feat)
	if got, err := (Git{}).Head(context.Background(), repo, "shadow"); err != nil || got != feat {
		t.Errorf("shadowed Head = %q, %v; want %s", got, err, feat)
	}
}

func TestWtIntegrateOrderAndNoMaster(t *testing.T) {
	repo, base := newRepo(t)
	h1 := branchWith(t, repo, base, "h1", "h1.txt", "1\n")
	h2 := branchWith(t, repo, base, "h2", "h2.txt", "2\n")
	h3 := branchWith(t, repo, base, "h3", "h3.txt", "3\n")
	ctx := context.Background()
	tip, err := Git{}.Integrate(ctx, repo, "gosplan/r1/integration", base, []string{h1, h2, h3})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, repo, "rev-parse", "gosplan/r1/integration"); got != tip {
		t.Errorf("branch at %s, returned tip %s", got, tip)
	}
	for _, h := range []string{h1, h2, h3} {
		git(t, repo, "merge-base", "--is-ancestor", h, tip)
	}
	// Merge order: h3 is merged last, so it is the second parent of the tip.
	if p2 := git(t, repo, "rev-parse", tip+"^2"); p2 != h3 {
		t.Errorf("last merged parent = %s, want %s", p2, h3)
	}
	if got := git(t, repo, "rev-parse", "master"); got != base {
		t.Errorf("master moved to %s", got)
	}
	if got := git(t, repo, "symbolic-ref", "--short", "HEAD"); got != "master" {
		t.Errorf("checked-out branch is %s", got)
	}
	// Integrating again with fewer heads resets the branch to base first.
	tip2, err := Git{}.Integrate(ctx, repo, "gosplan/r1/integration", base, []string{h1})
	if err != nil {
		t.Fatal(err)
	}
	if tip2 == tip {
		t.Error("second integrate did not reset")
	}
	if out := git(t, repo, "ls-tree", "--name-only", tip2); strings.Contains(out, "h2.txt") {
		t.Errorf("stale merge survived reset:\n%s", out)
	}
	if out := git(t, repo, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("temp worktree left behind:\n%s", out)
	}
}

func TestWtIntegrateConflictKeepsLastGood(t *testing.T) {
	repo, base := newRepo(t)
	h1 := branchWith(t, repo, base, "h1", "a.txt", "one\nA\nthree\n")
	h2 := branchWith(t, repo, base, "h2", "a.txt", "one\nB\nthree\n")
	tip, err := Git{}.Integrate(context.Background(), repo, "gosplan/r1/integration", base, []string{h1, h2})
	if !errors.Is(err, core.ErrMergeConflict) || !strings.Contains(err.Error(), h2) {
		t.Fatalf("err = %v, tip %q", err, tip)
	}
	if got := git(t, repo, "rev-parse", "gosplan/r1/integration"); got != h1 {
		t.Errorf("branch at %s, want last good merge %s", got, h1)
	}
	if out := git(t, repo, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("temp worktree left behind:\n%s", out)
	}
}

func TestWtIntegrateRefusesNonGosplanBranch(t *testing.T) {
	repo, base := newRepo(t)
	h1 := branchWith(t, repo, base, "h1", "h1.txt", "1\n")
	for _, b := range []string{"master", "main", "feature/x"} {
		if _, err := (Git{}).Integrate(context.Background(), repo, b, base, []string{h1}); err == nil {
			t.Errorf("Integrate into %q: want error", b)
		}
	}
	if got := git(t, repo, "rev-parse", "master"); got != base {
		t.Errorf("master moved to %s", got)
	}
}

func TestWtIntegrateRefusesCheckedOutBranch(t *testing.T) {
	repo, base := newRepo(t)
	git(t, repo, "checkout", "-q", "-b", "gosplan/r1/live")
	if _, err := (Git{}).Integrate(context.Background(), repo, "gosplan/r1/live", base, nil); err == nil {
		t.Error("want error for a checked-out branch")
	}
}

func TestWtDetachedWorktree(t *testing.T) {
	repo, base := newRepo(t)
	dir := filepath.Join(t.TempDir(), "mut")
	ctx := context.Background()
	if err := (Git{}).AddDetached(ctx, repo, base, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != base {
		t.Errorf("detached HEAD = %s", got)
	}
	if err := (Git{}).RemoveDetached(ctx, repo, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir still exists: %v", err)
	}
	if out := git(t, repo, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("worktree list not clean:\n%s", out)
	}
	if err := (Git{}).AddDetached(ctx, repo, "HEAD", dir); err == nil {
		t.Error("AddDetached accepted a ref")
	}
}

func TestWtResetAndResolve(t *testing.T) {
	repo, base := newRepo(t)
	ctx := context.Background()
	p, err := Git{}.Prepare(ctx, core.PrepareRequest{Project: repo, Run: "r1", Task: "t1", BaseSHA: base, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.Dir, "a.txt", "changed\n")
	writeFile(t, p.Dir, "junk.txt", "x\n")
	git(t, p.Dir, "add", "a.txt")
	git(t, p.Dir, "commit", "-q", "-m", "work")
	writeFile(t, p.Dir, "untracked.txt", "x\n")
	// A hook in the shared repo must not run on the orchestrator's reset.
	hook := filepath.Join(repo, ".git", "hooks", "post-index-change")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (Git{}).Reset(ctx, p.Dir, base); err != nil {
		t.Fatal(err)
	}
	if head := git(t, p.Dir, "rev-parse", "HEAD"); head != base {
		t.Errorf("HEAD = %s, want %s", head, base)
	}
	for _, f := range []string{"junk.txt", "untracked.txt"} {
		if _, err := os.Stat(filepath.Join(p.Dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset", f)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("post-index-change hook ran during Reset")
	}
	if err := (Git{}).Reset(ctx, p.Dir, "HEAD"); err == nil {
		t.Error("Reset accepted a ref")
	}
	if got, err := (Git{}).ResolveCommit(ctx, repo, "master"); err != nil || got != base {
		t.Errorf("ResolveCommit(master) = %q, %v", got, err)
	}
	if _, err := (Git{}).ResolveCommit(ctx, repo, "nope"); err == nil {
		t.Error("ResolveCommit(nope): want error")
	}
}
