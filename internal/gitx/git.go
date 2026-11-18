// Package gitx is a thin wrapper over the git CLI for the verifier.
//
// Every invocation ignores replace refs, repo-local hooks, system and global
// config, so the verifier reads what the commit objects say and nothing else.
// Worker-controlled state inside the shared .git directory cannot change what
// it reads or runs here.
package gitx

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Repo is a git repository at Dir.
type Repo struct {
	Dir string
}

// baseArgs disable every hook-like or config-driven behaviour that could run
// code or rewrite object reads.
var baseArgs = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.pager=cat",
	"-c", "core.untrackedCache=false",
	"-c", "core.attributesFile=/dev/null",
}

// env is the minimal environment for git. Replace refs are disabled so a
// `git replace` in the shared repo cannot swap object contents under us.
func env() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ATTR_NOSYSTEM=1",
	}
}

// Command builds a git command in r.Dir with the verifier's git settings.
func (r Repo) Command(args ...string) *exec.Cmd {
	full := append(append([]string{"-C", r.Dir}, baseArgs...), args...)
	cmd := exec.Command("git", full...)
	cmd.Env = env()
	return cmd
}

func (r Repo) run(args ...string) (string, error) {
	cmd := r.Command(args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ResolveCommit returns the full SHA for ref, which must name a commit.
func (r Repo) ResolveCommit(ref string) (string, error) {
	return r.run("rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

// TreeSHA returns the tree object id of commit sha.
func (r Repo) TreeSHA(sha string) (string, error) {
	return r.run("rev-parse", "--verify", sha+"^{tree}")
}

// CountCommits returns the number of commits reachable from head but not base.
func (r Repo) CountCommits(base, head string) (int, error) {
	out, err := r.run("rev-list", "--count", base+".."+head)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return 0, fmt.Errorf("gitx: parse rev-list count %q: %w", out, err)
	}
	return n, nil
}

// Change is one path differing between two commits.
type Change struct {
	Status string // A, M, D, T (renames are disabled, so no R/C)
	Path   string
}

// DiffNames lists paths that differ between base and head. Renames are not
// detected: a renamed file shows as a delete plus an add, so moving a
// protected file still counts as a modification of it.
func (r Repo) DiffNames(base, head string) ([]Change, error) {
	out, err := r.run("diff", "--no-renames", "--no-ext-diff", "--no-textconv",
		"--name-status", "-z", base, head)
	if err != nil {
		return nil, err
	}
	return parseNameStatusZ(out)
}

func parseNameStatusZ(out string) ([]Change, error) {
	if out == "" {
		return nil, nil
	}
	parts := strings.Split(out, "\x00")
	var changes []Change
	for i := 0; i < len(parts); i++ {
		status := parts[i]
		if status == "" {
			continue
		}
		if i+1 >= len(parts) {
			return nil, fmt.Errorf("gitx: name-status: status %q without path", status)
		}
		i++
		changes = append(changes, Change{Status: status[:1], Path: parts[i]})
	}
	return changes, nil
}

// ShowFile returns the blob content of path at commit sha. ok is false if the
// path does not exist at that commit. Any other git failure is an error.
func (r Repo) ShowFile(sha, path string) (content string, ok bool, err error) {
	if _, err := r.run("cat-file", "-e", sha+":"+path); err != nil {
		return "", false, nil
	}
	out, err := r.run("cat-file", "-p", sha+":"+path)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}
