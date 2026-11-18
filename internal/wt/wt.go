// Package wt is the real core.Workspace: task worktrees on gosplan/<run>/<task>
// branches, branch-tip resolution, and the end-of-run integration branch.
//
// It never checks out, moves or writes main, master or the project's
// checked-out branch: the only refs it creates or moves live under gosplan/.
// Merges (task bases and the integration branch) are done with plumbing by
// SHA, with fixed identity and dates, so they are reproducible.
// Every git call goes through gitx.Repo so hooks, fsmonitor, attributes,
// replace refs and user/system config are disabled. The repo's own
// .git/config is still read; a worker can write it (docs/trust-until-d7.md).
package wt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/gitx"
)

// Git is the real core.Workspace.
type Git struct {
	// Identity for merge commits.
	Name, Email string // default "gosplan", "gosplan@localhost"
}

var _ core.Workspace = Git{}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// BranchName returns "gosplan/<run>/<task>".
func BranchName(run, task string) string { return "gosplan/" + run + "/" + task }

func (g Git) identity() []string {
	name, email := g.Name, g.Email
	if name == "" {
		name = "gosplan"
	}
	if email == "" {
		email = "gosplan@localhost"
	}
	return []string{"-c", "user.name=" + name, "-c", "user.email=" + email, "-c", "commit.gpgsign=false"}
}

// git runs git in dir and returns trimmed stdout. ctx cancellation kills it.
func (g Git) git(ctx context.Context, dir string, args ...string) (string, error) {
	return g.gitEnv(ctx, dir, nil, args...)
}

// gitEnv is git with extra environment entries (commit dates).
func (g Git) gitEnv(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	cmd := gitx.Repo{Dir: dir}.Command(append(g.identity(), args...)...)
	cmd.Env = append(cmd.Env, extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "),
			err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func requireSHA(what, s string) error {
	if !fullSHA.MatchString(s) {
		return fmt.Errorf("wt: %s %q is not a full 40-char SHA", what, s)
	}
	return nil
}

func (g Git) branchExists(ctx context.Context, project, branch string) bool {
	_, err := g.git(ctx, project, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func validBranch(ctx context.Context, g Git, project, branch string) error {
	if _, err := g.git(ctx, project, "check-ref-format", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("wt: invalid branch name %q", branch)
	}
	return nil
}

// Prepare creates the task worktree on gosplan/<run>/<task> at the base
// ComputeBase gives for BaseSHA and DepHeads. No merge runs inside the
// worktree, so the base is a pure function of those SHAs.
//
// If the branch already exists (a resume or a retry), the live worktree on it
// is reused and Prepared.BaseSHA is still the computed base, not the
// worktree's current HEAD: the caller resets the worktree to it.
func (g Git) Prepare(ctx context.Context, req core.PrepareRequest) (core.Prepared, error) {
	branch := BranchName(req.Run, req.Task)
	if err := validBranch(ctx, g, req.Project, branch); err != nil {
		return core.Prepared{}, err
	}
	dir := filepath.Join(req.Root, req.Run, req.Task)
	p := core.Prepared{Dir: dir, Branch: branch}
	base, err := g.ComputeBase(ctx, req.Project, req.BaseSHA, req.DepHeads)
	if err != nil {
		return p, err
	}
	p.BaseSHA = base

	if g.branchExists(ctx, req.Project, branch) {
		// Resume: reuse only a live worktree that is on this branch.
		if _, err := os.Stat(dir); err != nil {
			return p, fmt.Errorf("wt: branch %s exists but worktree %s is missing", branch, dir)
		}
		cur, err := g.git(ctx, dir, "symbolic-ref", "--short", "HEAD")
		if err != nil || cur != branch {
			return p, fmt.Errorf("wt: branch %s exists but %s is on %q", branch, dir, cur)
		}
		return p, nil
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return p, err
	}
	if _, err := g.git(ctx, req.Project, "worktree", "add", "-b", branch, dir, base); err != nil {
		return p, err
	}
	return p, nil
}

// MergeConflict is the error for a merge that conflicts. It wraps
// core.ErrMergeConflict.
type MergeConflict struct {
	Into, Head string   // the commit merged into and the head that conflicted
	Paths      []string // conflicted paths, as git merge-tree lists them
}

func (e *MergeConflict) Error() string {
	return fmt.Sprintf("%v: merging head %s into %s: conflicting paths: %s",
		core.ErrMergeConflict, e.Head, e.Into, strings.Join(e.Paths, ", "))
}

func (e *MergeConflict) Unwrap() error { return core.ErrMergeConflict }

// ConflictHead and ConflictPaths let callers outside wt read the conflict
// without importing it.
func (e *MergeConflict) ConflictHead() string    { return e.Head }
func (e *MergeConflict) ConflictPaths() []string { return e.Paths }

// ComputeBase merges heads into base in order and returns the result. It
// only uses plumbing (merge-base, merge-tree, commit-tree) in the project
// repo, by SHA: no worktree, no ref is read or moved. A head already
// contained is skipped, a descendant is fast-forwarded to, and anything else
// becomes a two-parent commit whose author, committer and date are fixed
// (gosplan, the later parent's committer date). The same inputs therefore
// give the same SHA every time, so the orchestrator can recompute a task base
// instead of reading one back. On conflict the error is a *MergeConflict.
func (g Git) ComputeBase(ctx context.Context, project, base string, heads []string) (string, error) {
	if err := requireSHA("base", base); err != nil {
		return "", err
	}
	for _, h := range heads {
		if err := requireSHA("head", h); err != nil {
			return "", err
		}
	}
	cur := base
	for _, h := range heads {
		next, err := g.mergeOne(ctx, project, cur, h)
		if err != nil {
			return "", err
		}
		cur = next
	}
	return cur, nil
}

func (g Git) mergeOne(ctx context.Context, project, cur, h string) (string, error) {
	if cur == h {
		return cur, nil
	}
	if ok, err := g.isAncestor(ctx, project, h, cur); err != nil || ok {
		return cur, err
	}
	if ok, err := g.isAncestor(ctx, project, cur, h); err != nil || ok {
		if ok {
			return h, nil
		}
		return "", err
	}
	out, err := g.git(ctx, project, "merge-tree", "--write-tree", "--name-only", "--no-messages", cur, h)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		lines := strings.Split(out, "\n")
		return "", &MergeConflict{Into: cur, Head: h, Paths: lines[1:]}
	}
	if err != nil {
		return "", fmt.Errorf("wt: merging head %s: %w", h, err)
	}
	tree := strings.SplitN(out, "\n", 2)[0]
	if err := requireSHA("merged tree", tree); err != nil {
		return "", err
	}
	date, err := g.laterDate(ctx, project, cur, h)
	if err != nil {
		return "", err
	}
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	sha, err := g.gitEnv(ctx, project, env, "commit-tree", tree, "-p", cur, "-p", h, "-m", "gosplan: merge "+h)
	if err != nil {
		return "", err
	}
	return sha, requireSHA("merge commit", sha)
}

// isAncestor reports whether a is an ancestor of (or equal to) b.
func (g Git) isAncestor(ctx context.Context, project, a, b string) (bool, error) {
	_, err := g.git(ctx, project, "merge-base", "--is-ancestor", a, b)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	}
	return false, err
}

// laterDate is "@<unix> +0000" for the later committer date of a and b.
func (g Git) laterDate(ctx context.Context, project, a, b string) (string, error) {
	out, err := g.git(ctx, project, "show", "-s", "--format=%ct", a, b)
	if err != nil {
		return "", err
	}
	var max int64
	for _, f := range strings.Fields(out) {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return "", fmt.Errorf("wt: committer date %q: %w", f, err)
		}
		if n > max {
			max = n
		}
	}
	return fmt.Sprintf("@%d +0000", max), nil
}

// Head returns the full SHA of refs/heads/<branch>.
func (g Git) Head(ctx context.Context, project, branch string) (string, error) {
	out, err := g.git(ctx, project, "rev-parse", "--verify", "--end-of-options", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", err
	}
	if err := requireSHA("head of "+branch, out); err != nil {
		return "", err
	}
	return out, nil
}

// Reset puts the worktree at dir (and its branch) back at base: reset
// --hard, then clean -fdx. It runs through gitx like every other call here,
// so hooks, fsmonitor, system/global config and replace objects are off.
// The worktree's repo-local config and in-tree .gitattributes still apply
// (a worker can write both; see docs/trust-until-d7.md).
func (g Git) Reset(ctx context.Context, dir, base string) error {
	if dir == "" {
		return errors.New("wt: reset: no worktree")
	}
	if err := requireSHA("base", base); err != nil {
		return err
	}
	if _, err := g.git(ctx, dir, "reset", "-q", "--hard", base); err != nil {
		return fmt.Errorf("wt: reset %s: %w", dir, err)
	}
	if _, err := g.git(ctx, dir, "clean", "-q", "-f", "-d", "-x"); err != nil {
		return fmt.Errorf("wt: reset %s: %w", dir, err)
	}
	return nil
}

// ResolveCommit resolves ref in project to a full commit SHA, through gitx.
func (g Git) ResolveCommit(ctx context.Context, project, ref string) (string, error) {
	out, err := g.git(ctx, project, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil || requireSHA("commit", out) != nil {
		return "", fmt.Errorf("resolve %q in %s: not a commit", ref, project)
	}
	return out, nil
}

// Remove deletes a task worktree directory and prunes. The branch is kept.
func (g Git) Remove(ctx context.Context, project, dir string) error {
	return g.removeTree(ctx, project, dir)
}

func (g Git) removeTree(ctx context.Context, project, dir string) error {
	if _, err := os.Stat(dir); err == nil {
		if _, err := g.git(ctx, project, "worktree", "remove", "--force", dir); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := g.git(ctx, project, "worktree", "prune")
	return err
}

// AddDetached creates a throwaway detached worktree of sha at dir.
func (g Git) AddDetached(ctx context.Context, project, sha, dir string) error {
	if err := requireSHA("sha", sha); err != nil {
		return err
	}
	_, err := g.git(ctx, project, "worktree", "add", "--detach", dir, sha)
	return err
}

// RemoveDetached removes a worktree made by AddDetached and prunes.
func (g Git) RemoveDetached(ctx context.Context, project, dir string) error {
	return g.removeTree(ctx, project, dir)
}

// checkedOut reports whether branch is checked out in any worktree of project.
func (g Git) checkedOut(ctx context.Context, project, branch string) (bool, error) {
	out, err := g.git(ctx, project, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(out, "\n") {
		if l == "branch refs/heads/"+branch {
			return true, nil
		}
	}
	return false, nil
}

// Integrate sets branch (which must be under gosplan/ and not checked out) to
// ComputeBase(baseSHA, heads) and returns the new tip. Heads are merged by
// SHA; no ref other than branch is read or written. On conflict the branch
// is set to the last good merge and the error is a *MergeConflict.
func (g Git) Integrate(ctx context.Context, project, branch, baseSHA string, heads []string) (string, error) {
	if !strings.HasPrefix(branch, "gosplan/") {
		return "", fmt.Errorf("wt: refusing to integrate into %q: branch must be under gosplan/", branch)
	}
	if err := validBranch(ctx, g, project, branch); err != nil {
		return "", err
	}
	if err := requireSHA("base", baseSHA); err != nil {
		return "", err
	}
	for _, h := range heads {
		if err := requireSHA("head", h); err != nil {
			return "", err
		}
	}
	if used, err := g.checkedOut(ctx, project, branch); err != nil {
		return "", err
	} else if used {
		return "", fmt.Errorf("wt: branch %s is checked out in a worktree", branch)
	}
	cur := baseSHA
	var merr error
	for _, h := range heads {
		next, err := g.mergeOne(ctx, project, cur, h)
		if err != nil {
			merr = err
			break
		}
		cur = next
	}
	if _, err := g.git(ctx, project, "update-ref", "refs/heads/"+branch, cur); err != nil {
		return "", err
	}
	if merr != nil {
		return "", merr
	}
	return cur, nil
}
