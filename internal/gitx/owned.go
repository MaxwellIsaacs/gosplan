package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// fullSHA matches a 40-character lowercase hex commit id. Refs and short ids
// are rejected: a ref could be moved by whoever controls the worker repo.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// RequireFullSHA returns an error unless s is a full 40-char commit id.
func RequireFullSHA(s string) error {
	if !fullSHA.MatchString(s) {
		return fmt.Errorf("gitx: %q is not a full 40-char commit SHA", s)
	}
	return nil
}

// Owned is a bare repository created and owned by the verifier. Nothing in it
// comes from the worker's .git except objects that were transferred and
// re-hashed by git's receive side.
type Owned struct {
	Repo
}

// NewOwned creates an empty bare repository at dir with a verifier-written
// config. The directory must not exist yet.
func NewOwned(dir string) (Owned, error) {
	cmd := exec.Command("git", "init", "--bare", "--quiet", dir)
	cmd.Env = env()
	if out, err := cmd.CombinedOutput(); err != nil {
		return Owned{}, fmt.Errorf("gitx: init owned repo: %w: %s", err, out)
	}
	return Owned{Repo{Dir: dir}}, nil
}

// transferEnv is the environment for the fetch. Its GIT_CONFIG_* overrides
// apply to the receiving git (this process's fetch) only. For a local fetch,
// git strips repo-local environment such as GIT_CONFIG_COUNT before it spawns
// upload-pack, so these overrides do NOT reach the sending side: the worker's
// repo config still governs upload-pack. They are kept as defence in depth.
// The security property rests on the receive side instead: re-hashing every
// object, transfer.fsckObjects, and the ref and history checks in FetchFrom
// (01-verify-contract.md, Trust model, step 1).
func transferEnv(home string) []string {
	return append(env(),
		"HOME="+home,
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=uploadpack.packobjectshook", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=uploadpack.allowanysha1inwant", "GIT_CONFIG_VALUE_1=true",
		"GIT_CONFIG_KEY_2=core.hooksPath", "GIT_CONFIG_VALUE_2=/dev/null",
		"GIT_CONFIG_KEY_3=transfer.fsckobjects", "GIT_CONFIG_VALUE_3=true",
	)
}

// FetchFrom copies commits base and head from the source repository at srcDir
// into o. Git's receive side re-hashes every object it gets, so an object
// whose stored content differs from its id is rejected rather than copied.
// base and head must be full SHAs. After the fetch both are checked to exist.
func (o Owned) FetchFrom(ctx context.Context, srcDir, base, head, home string) error {
	if err := RequireFullSHA(base); err != nil {
		return err
	}
	if err := RequireFullSHA(head); err != nil {
		return err
	}
	// The fetch runs with -C o.Dir, so a relative source path would resolve
	// against the owned repo itself (e.g. the CLI default --project ".").
	src, err := filepath.Abs(srcDir)
	if err != nil {
		return fmt.Errorf("gitx: worker repo path: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", o.Dir,
		"-c", "core.hooksPath=/dev/null",
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc",
		"--no-recurse-submodules", src,
		base+":refs/gosplan/base", head+":refs/gosplan/head")
	cmd.Env = transferEnv(home)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gitx: transfer from worker repo: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	// git fetch can exit 0 while rejecting a ref (e.g. a shallow root). The
	// full history must be present: every object reachable from head but not
	// base has to be readable. Otherwise the copy is incomplete.
	// Check the refs the fetch was asked to create, not just the objects: a
	// rejected ref update leaves its objects behind.
	for ref, sha := range map[string]string{"refs/gosplan/base": base, "refs/gosplan/head": head} {
		got, err := o.run("rev-parse", "--verify", ref+"^{commit}")
		if err != nil || got != sha {
			return fmt.Errorf("gitx: %s not created at %s after transfer", ref, sha)
		}
	}
	if _, err := o.run("rev-list", "--objects", base+".."+head); err != nil {
		return fmt.Errorf("gitx: history incomplete after transfer: %w", err)
	}
	return nil
}

// Remove deletes the owned repository directory.
func (o Owned) Remove() error { return os.RemoveAll(filepath.Clean(o.Dir)) }
