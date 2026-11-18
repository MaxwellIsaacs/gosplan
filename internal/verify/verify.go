// Package verify implements the M1 verifier: gates and cache rules.
//
// Trust boundary: config, locked and protected globs come from the base
// commit (read through git with replace refs and hooks disabled). Commands run
// in a fresh export of the head commit, with a minimal environment and a fresh
// HOME. Nothing the worker wrote in its own working tree or in the shared .git
// is trusted.
//
// Not covered here: commands run as the same OS user as the verifier. Code in
// the head tree can therefore write anywhere that user can, including the
// cache directory. Closing that needs a separate user or a sandbox (D7). Until
// then the cache must live outside any directory command code is allowed to
// write, and the caller is responsible for that.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/gitx"

	"github.com/bmatcuk/doublestar/v4"
)

// CacheVersion is part of every cache key. Bump it when the export or gate
// semantics change, so entries made by an older verifier are never reused.
// The toolchain is not in the key (01-verify-contract.md, Cache): a tree that
// fails under a changed toolchain can still hit a cached PASS, so bump this
// when toolchains change too, or disable the cache.
const CacheVersion = "m1-r3"

// cacheVersion is the version actually hashed. It is a variable only so a
// test can simulate a CacheVersion bump; production code never changes it.
var cacheVersion = CacheVersion

// Gate names. cmd:<name> gates are built from Command.Name.
const (
	GateObjectIntegrity = "object_integrity"
	GateCommittedWork   = "committed_work"
	GateLockedPaths     = "locked_paths"
	GateProtectedPaths  = "protected_paths"
	GateScope           = "scope"
	GateCommandsPresent = "commands_present"
	GateCleanCheckout   = "clean_checkout"
)

// Options configures one verification.
type Options struct {
	Repo  gitx.Repo
	Task  string
	Base  string   // ref recorded in the launch event
	Head  string   // task branch tip
	Scope []string // nil = unrestricted (scope gate skipped)

	// AllowNewProtected lets the task add files matching protected globs
	// (new tests). Off by default: a new file can override a test's behaviour
	// (e.g. a TestMain). The orchestrator sets it only from the task sidecar.
	AllowNewProtected bool

	CacheDir string // "" disables the cache; see the package doc for placement
	LogDir   string // "" keeps command output in WorkDir, removed after the run
	WorkDir  string // parent for temporary export and HOME dirs; "" = os temp dir
}

// Verdict is the outcome of one verification. It maps onto the `verdict`
// event in the event schema.
type Verdict struct {
	Task         string                 `json:"task"`
	Base         string                 `json:"base"`
	Head         string                 `json:"head"`
	BaseSHA      string                 `json:"base_sha"`
	HeadSHA      string                 `json:"head_sha"`
	TreeSHA      string                 `json:"tree_sha"`
	Unrestricted bool                   `json:"scope_unrestricted"`
	CacheHit     bool                   `json:"cache_hit"`
	Gates        []events.GateData      `json:"gates"`
	Commands     []events.CommandResult `json:"commands"`
	Result       string                 `json:"result"`
	FailedGates  []string               `json:"failed_gates"`
}

// Passed reports whether the verdict is a pass.
func (v Verdict) Passed() bool { return v.Result == "pass" }

// Run verifies one task. A returned error means verification could not run
// at all (bad refs, missing config, git failure, cancellation). That is exit
// 2 in the CLI, and it is never a PASS. A failing task is a Verdict with
// Result "fail" and a nil error.
func Run(ctx context.Context, opt Options) (Verdict, error) {
	v := Verdict{Task: opt.Task, Base: opt.Base, Head: opt.Head, Unrestricted: opt.Scope == nil}

	if err := gitx.RequireFullSHA(opt.Base); err != nil {
		return v, fmt.Errorf("verify: base: %w", err)
	}
	if err := gitx.RequireFullSHA(opt.Head); err != nil {
		return v, fmt.Errorf("verify: head: %w", err)
	}
	v.BaseSHA, v.HeadSHA = opt.Base, opt.Head

	workParent := opt.WorkDir
	if workParent == "" {
		workParent = os.TempDir()
	}
	ownedParent, err := os.MkdirTemp(workParent, "gosplan-owned-*")
	if err != nil {
		return v, fmt.Errorf("verify: owned dir: %w", err)
	}
	defer os.RemoveAll(ownedParent)
	home, err := os.MkdirTemp(workParent, "gosplan-xfer-home-*")
	if err != nil {
		return v, fmt.Errorf("verify: transfer home: %w", err)
	}
	defer os.RemoveAll(home)

	// Everything below reads only from this verifier-owned repository. The
	// worker's .git (objects, config, attributes, replace refs) is touched only
	// by the transfer, which re-hashes objects.
	owned, err := gitx.NewOwned(filepath.Join(ownedParent, "repo.git"))
	if err != nil {
		return v, fmt.Errorf("verify: %w", err)
	}
	if err := owned.FetchFrom(ctx, opt.Repo.Dir, opt.Base, opt.Head, home); err != nil {
		// A signal or cancellation is infrastructure: exit 2, retry.
		if ctx.Err() != nil {
			return v, fmt.Errorf("verify: interrupted during transfer: %w", ctx.Err())
		}
		// Integrity failure: the worker's objects do not match their ids, the
		// commits are missing, or the history is incomplete. A FAIL, not an
		// infra error, so a worker cannot make the orchestrator retry forever.
		v.add(events.GateData{Name: GateObjectIntegrity, Status: events.GateFail, Detail: err.Error()})
		return v.finish(), nil
	}
	v.add(events.GateData{Name: GateObjectIntegrity, Status: events.GatePass})

	if v.TreeSHA, err = owned.TreeSHA(opt.Head); err != nil {
		return v, fmt.Errorf("verify: tree of head: %w", err)
	}
	cfg, err := LoadConfig(owned.Repo, opt.Base)
	if err != nil {
		return v, err
	}
	protected := cfg.Protected()
	locked := cfg.LockedPaths()
	cmds := cfg.Commands()
	for _, p := range append(append(append([]string{}, protected...), locked...), opt.Scope...) {
		if !doublestar.ValidatePattern(p) {
			return v, fmt.Errorf("verify: invalid glob %q", p)
		}
	}

	changes, err := owned.DiffNames(opt.Base, opt.Head)
	if err != nil {
		return v, fmt.Errorf("verify: diff: %w", err)
	}
	commits, err := owned.CountCommits(opt.Base, opt.Head)
	if err != nil {
		return v, fmt.Errorf("verify: count commits: %w", err)
	}
	// committed_work counts a change that adds no files as no work.
	if len(changes) == 0 {
		commits = 0
	}

	// Gates 1–3 and the presence check depend on history, not just the
	// tree, so they are never cached.
	v.add(committedWork(commits))
	v.add(lockedPaths(changes, locked))
	v.add(protectedPaths(changes, protected, opt.AllowNewProtected))
	v.add(scopeGate(changes, opt.Scope))
	presence := commandsPresent(cmds)
	v.add(presence)

	// Gates 4–5 depend only on the tree and the command set: cacheable.
	key := cacheKey(v.TreeSHA, cmds, protected, locked, opt.Scope)
	if opt.CacheDir != "" && presence.Status == events.GatePass {
		if cached, ok := readCache(opt.CacheDir, key); ok && validCache(cached, key, v.TreeSHA, cmds) {
			v.CacheHit = true
			v.Gates = append(v.Gates, cached.Gates...)
			v.Commands = cached.Commands
			return v.finish(), nil
		}
	}

	if presence.Status != events.GatePass {
		v.Gates = append(v.Gates, skippedCheckout(cmds, "not run: no commands configured")...)
		return v.finish(), nil
	}

	checkout, results, err := runCleanCheckout(ctx, opt, owned, v.HeadSHA, cmds, workParent)
	if err != nil {
		return v, err
	}
	v.Gates = append(v.Gates, checkout...)
	v.Commands = results

	if opt.CacheDir != "" && allPass(checkout) {
		if err := writeCache(opt.CacheDir, key, cacheEntry{
			Key: key, TreeSHA: v.TreeSHA, Gates: checkout, Commands: results,
		}); err != nil {
			return v, fmt.Errorf("verify: write cache: %w", err)
		}
	}
	return v.finish(), nil
}

func (v *Verdict) add(g events.GateData) { v.Gates = append(v.Gates, g) }

// finish sets Result and FailedGates. Result is "pass" only if no gate failed.
// A skipped gate is not a failure. A PASS always has at least one command,
// because commands_present fails when none are configured.
func (v Verdict) finish() Verdict {
	v.FailedGates = []string{}
	for _, g := range v.Gates {
		if g.Status == events.GateFail {
			v.FailedGates = append(v.FailedGates, g.Name)
		}
	}
	v.Result = "pass"
	if len(v.FailedGates) > 0 {
		v.Result = "fail"
	}
	return v
}

// Event returns the verdict as a `verdict` event payload. Commands is never
// nil, so the wire format always has an array.
func (v Verdict) Event() events.VerdictData {
	cmds := v.Commands
	if cmds == nil {
		cmds = []events.CommandResult{}
	}
	return events.VerdictData{
		TreeSHA: v.TreeSHA, CacheHit: v.CacheHit, Commands: cmds,
		Result: v.Result, FailedGates: v.FailedGates,
	}
}

func committedWork(n int) events.GateData {
	if n == 0 {
		return events.GateData{Name: GateCommittedWork, Status: events.GateFail,
			Detail: "no commits beyond base, or no file changes; the agent did not commit its work"}
	}
	return events.GateData{Name: GateCommittedWork, Status: events.GatePass,
		Detail: fmt.Sprintf("%d commit(s) beyond base", n)}
}

func commandsPresent(cmds []Command) events.GateData {
	if len(cmds) == 0 {
		return events.GateData{Name: GateCommandsPresent, Status: events.GateFail,
			Detail: "no verification commands configured; nothing would be checked"}
	}
	return events.GateData{Name: GateCommandsPresent, Status: events.GatePass}
}

// lockedPaths fails on any change to a locked file, whatever the scope says.
// Locked files configure the verifier itself, so a task cannot edit them.
func lockedPaths(changes []gitx.Change, globs []string) events.GateData {
	var bad []string
	for _, c := range changes {
		if matchAny(globs, c.Path) {
			bad = append(bad, fmt.Sprintf("%s %s", c.Status, c.Path))
		}
	}
	if len(bad) > 0 {
		return events.GateData{Name: GateLockedPaths, Status: events.GateFail,
			Detail: "locked files changed: " + strings.Join(bad, ", ")}
	}
	return events.GateData{Name: GateLockedPaths, Status: events.GatePass}
}

// protectedPaths fails on any modify, delete or type change of a protected
// file. Every M/D/T entry existed at base by definition, so no existence
// check is needed. Added files fail too unless AllowNewProtected is set.
// Renames are not detected, so moving a protected file shows as a delete.
func protectedPaths(changes []gitx.Change, globs []string, allowNew bool) events.GateData {
	var bad []string
	for _, c := range changes {
		if !matchAny(globs, c.Path) {
			continue
		}
		if c.Status == "A" && allowNew {
			continue
		}
		bad = append(bad, fmt.Sprintf("%s %s", c.Status, c.Path))
	}
	if len(bad) > 0 {
		return events.GateData{Name: GateProtectedPaths, Status: events.GateFail,
			Detail: "protected paths changed: " + strings.Join(bad, ", ")}
	}
	return events.GateData{Name: GateProtectedPaths, Status: events.GatePass}
}

func scopeGate(changes []gitx.Change, scope []string) events.GateData {
	if scope == nil {
		return events.GateData{Name: GateScope, Status: events.GateSkip, Detail: "unrestricted"}
	}
	var out []string
	for _, c := range changes {
		if !matchAny(scope, c.Path) {
			out = append(out, c.Path)
		}
	}
	if len(out) > 0 {
		return events.GateData{Name: GateScope, Status: events.GateFail,
			Detail: "writes outside declared scope: " + strings.Join(out, ", ")}
	}
	return events.GateData{Name: GateScope, Status: events.GatePass}
}

func matchAny(globs []string, path string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, path); ok {
			return true
		}
	}
	return false
}

func skippedCheckout(cmds []Command, why string) []events.GateData {
	gates := []events.GateData{{Name: GateCleanCheckout, Status: events.GateSkip, Detail: why}}
	for _, c := range cmds {
		gates = append(gates, events.GateData{Name: "cmd:" + c.Name, Status: events.GateSkip, Detail: why})
	}
	return gates
}

func allPass(gs []events.GateData) bool {
	for _, g := range gs {
		if g.Status == events.GateFail {
			return false
		}
	}
	return true
}

// runCleanCheckout exports head into a fresh directory and runs each command
// there with a fresh HOME. If the export fails, clean_checkout fails and the
// commands are recorded as skipped. Export errors are gate failures, not
// infrastructure errors, so a broken tree cannot pass by being unreadable.
func runCleanCheckout(ctx context.Context, opt Options, owned gitx.Owned, headSHA string, cmds []Command, workParent string) ([]events.GateData, []events.CommandResult, error) {
	logDir := opt.LogDir
	if logDir == "" {
		logDir = workParent
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("verify: log dir: %w", err)
	}

	if ctx.Err() != nil {
		return nil, nil, fmt.Errorf("verify: interrupted before checkout: %w", ctx.Err())
	}
	tree, err := os.MkdirTemp(workParent, "gosplan-export-*")
	if err != nil {
		return nil, nil, fmt.Errorf("verify: export dir: %w", err)
	}
	defer os.RemoveAll(tree)
	home, err := os.MkdirTemp(workParent, "gosplan-home-*")
	if err != nil {
		return nil, nil, fmt.Errorf("verify: home dir: %w", err)
	}
	defer os.RemoveAll(home)

	if err := owned.Export(headSHA, tree); err != nil {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("verify: interrupted during export: %w", ctx.Err())
		}
		// A tree that cannot be materialised is a failure, never a skip.
		gates := []events.GateData{{Name: GateCleanCheckout, Status: events.GateFail,
			Detail: "export failed: " + err.Error()}}
		for _, c := range cmds {
			gates = append(gates, events.GateData{Name: "cmd:" + c.Name, Status: events.GateSkip,
				Detail: "not run: clean checkout failed"})
		}
		return gates, nil, nil
	}

	gates := []events.GateData{{Name: GateCleanCheckout, Status: events.GatePass}}
	env := commandEnv(home)
	var results []events.CommandResult
	for _, c := range cmds {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("verify: interrupted: %w", ctx.Err())
		}
		outPath := logPathFor(logDir, opt.Task, c.Name)
		out := runCommand(ctx, tree, env, c.Shell, c.Timeout, outPath)
		if out.Interrupted {
			return nil, nil, fmt.Errorf("verify: interrupted while running %s", c.Name)
		}
		results = append(results, events.CommandResult{
			Name: c.Name, Argv: []string{"bash", "-c", c.Shell},
			ExitCode: out.ExitCode, DurationMS: out.Duration.Milliseconds(),
		})
		g := events.GateData{Name: "cmd:" + c.Name, Status: events.GatePass,
			DurationMS: out.Duration.Milliseconds()}
		switch {
		case out.Err != nil:
			g.Status, g.Detail = events.GateFail, "could not start: "+out.Err.Error()
		case out.TimedOut:
			g.Status, g.Detail = events.GateFail, fmt.Sprintf("timed out after %s", c.Timeout)
		case out.ExitCode != 0:
			g.Status, g.Detail = events.GateFail, fmt.Sprintf("exit %d", out.ExitCode)
		}
		if g.Status == events.GateFail && opt.LogDir != "" {
			g.Detail += "; log: " + outPath
		}
		gates = append(gates, g)
	}
	return gates, results, nil
}

// cacheEntry is what is stored for a passing tree. Only gates 4–5 are cached.
type cacheEntry struct {
	Key      string                 `json:"key"`
	TreeSHA  string                 `json:"tree_sha"`
	Gates    []events.GateData      `json:"gates"`
	Commands []events.CommandResult `json:"commands"`
}

// cacheKey hashes everything that decides gates 4–5: the tree, the commands
// with their timeouts, and every glob set. Changing any of them misses.
func cacheKey(tree string, cmds []Command, protected, locked, scope []string) string {
	type c struct {
		Name    string `json:"name"`
		Shell   string `json:"shell"`
		Timeout int64  `json:"timeout_s"`
	}
	cs := make([]c, 0, len(cmds))
	for _, x := range cmds {
		cs = append(cs, c{x.Name, x.Shell, int64(x.Timeout.Seconds())})
	}
	blob, _ := json.Marshal(struct {
		Version   string   `json:"version"`
		Tree      string   `json:"tree"`
		Commands  []c      `json:"commands"`
		Protected []string `json:"protected"`
		Locked    []string `json:"locked"`
		Scope     []string `json:"scope"`
	}{cacheVersion, tree, cs, protected, locked, scope})
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// validCache accepts an entry only if it is exactly what a passing run of
// this tree and command set would have produced: same key and tree, the
// same command names in order, a clean_checkout pass, and a pass for every
// expected cmd gate. An empty or partial entry is a miss.
func validCache(e cacheEntry, key, tree string, cmds []Command) bool {
	if e.Key != key || e.TreeSHA != tree {
		return false
	}
	if len(e.Commands) != len(cmds) {
		return false
	}
	for i, c := range cmds {
		if e.Commands[i].Name != c.Name {
			return false
		}
	}
	want := map[string]bool{GateCleanCheckout: false}
	for _, c := range cmds {
		want["cmd:"+c.Name] = false
	}
	for _, g := range e.Gates {
		if g.Status != events.GatePass {
			return false
		}
		if _, ok := want[g.Name]; ok {
			want[g.Name] = true
		}
	}
	for _, seen := range want {
		if !seen {
			return false
		}
	}
	return true
}

func readCache(dir, key string) (cacheEntry, bool) {
	b, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if json.Unmarshal(b, &e) != nil {
		return cacheEntry{}, false
	}
	return e, true
}

// writeCache writes via a uniquely named temp file, fsyncs, then renames, so
// concurrent writers and crashes cannot leave a half-written entry under the
// final name.
func writeCache(dir, key string, e cacheEntry) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, key+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, key+".json"))
}
