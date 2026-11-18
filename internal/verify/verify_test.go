package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/gitx"
)

// fixture is a project repo with a base commit (carrying nightrunner.toml)
// and a task branch the tests can commit onto.
type fixture struct {
	t    *testing.T
	dir  string
	repo gitx.Repo
	base string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newFixture builds a repo whose base commit has the given nightrunner.toml
// and any extra base files. The task branch "task/x" is checked out.
func newFixture(t *testing.T, toml string, baseFiles map[string]string) *fixture {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "nightrunner.toml", toml)
	for p, c := range baseFiles {
		write(t, dir, p, c)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "checkout", "-q", "-b", "task/x")
	return &fixture{t: t, dir: dir, repo: gitx.Repo{Dir: dir}, base: base}
}

// commit stages everything currently in the worktree and commits it.
func (f *fixture) commit(msg string) {
	git(f.t, f.dir, "add", "-A")
	git(f.t, f.dir, "commit", "-q", "-m", msg)
}

// head returns the full SHA of the task branch tip.
func (f *fixture) head() string {
	return git(f.t, f.dir, "rev-parse", "HEAD")
}

func (f *fixture) run(scope []string, cache string) Verdict {
	f.t.Helper()
	v, err := Run(context.Background(), Options{
		Repo: f.repo, Task: "1.a", Base: f.base, Head: f.head(),
		Scope: scope, CacheDir: cache, LogDir: f.t.TempDir(), WorkDir: f.t.TempDir(),
	})
	if err != nil {
		f.t.Fatalf("Run error (infra, not verdict): %v", err)
	}
	return v
}

func gate(v Verdict, name string) events.GateData {
	for _, g := range v.Gates {
		if g.Name == name {
			return g
		}
	}
	return events.GateData{}
}

const baseToml = `
[verify]
test = "test -f src/feature.txt"
`

// The four golden cases are committed fixtures: see golden_test.go and
// testdata/golden/README.md.

// Trust rule: the worker cannot change the verify command by editing the
// toml on its branch. The base config is the one that runs.
func TestWorkerCannotEditVerifyCommand(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "nightrunner.toml", `
[verify]
test = "true"
`)
	f.commit("worker weakens the command")

	v := f.run(nil, "")
	if v.Passed() {
		t.Fatal("want FAIL: base command requires src/feature.txt, which is absent")
	}
	if len(v.Commands) != 1 || v.Commands[0].Name != "test" {
		t.Fatalf("commands should come from base config: %+v", v.Commands)
	}
}

// Adding a protected file fails by default: a new file can override a test
// (TestMain, a nested go.mod). It passes only when the task opts in.
func TestAddingProtectedTestRequiresOptIn(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	write(t, f.dir, "pkg/zz_test.go", "package pkg\n\nimport (\"os\"; \"testing\")\n\nfunc TestMain(m *testing.M) { os.Exit(0) }\n")
	f.commit("feature + TestMain override")

	v := f.run(nil, "")
	if g := gate(v, GateProtectedPaths); g.Status != events.GateFail {
		t.Fatalf("added TestMain must fail protected_paths by default, got %+v", g)
	}

	v2, err := Run(context.Background(), Options{
		Repo: f.repo, Task: "1.a", Base: f.base, Head: f.head(),
		AllowNewProtected: true, LogDir: t.TempDir(), WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if g := gate(v2, GateProtectedPaths); g.Status != events.GatePass {
		t.Fatalf("opt-in must allow adding a protected file, got %+v", g)
	}
}

func TestNoCommitsFailsCommittedWork(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"src/feature.txt": "x"})
	v := f.run(nil, "")
	if g := gate(v, GateCommittedWork); g.Status != events.GateFail {
		t.Fatalf("committed_work: want fail, got %+v", g)
	}
	if v.Passed() {
		t.Fatal("no commits must not pass")
	}
}

func TestUnrestrictedScopeIsRecorded(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")
	v := f.run(nil, "")
	if !v.Unrestricted {
		t.Fatal("nil scope must be recorded as unrestricted")
	}
	if g := gate(v, GateScope); g.Status != events.GateSkip {
		t.Fatalf("scope: want skip, got %+v", g)
	}
}

func TestTimeoutFailsCommand(t *testing.T) {
	f := newFixture(t, `
[verify]
test = "sleep 30"
[verify.timeouts]
test_seconds = 1
`, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	start := time.Now()
	v := f.run(nil, "")
	if time.Since(start) > 15*time.Second {
		t.Fatalf("timeout did not kill the command promptly: %s", time.Since(start))
	}
	g := gate(v, "cmd:test")
	if g.Status != events.GateFail || !strings.Contains(g.Detail, "timed out") {
		t.Fatalf("cmd:test: want timeout fail, got %+v", g)
	}
}

// Cache: same tree → cache hit with the same command results; a different
// command set → miss.
func TestCacheHitOnSameTreeMissOnNewCommand(t *testing.T) {
	cache := t.TempDir()
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	first := f.run(nil, cache)
	if first.CacheHit || !first.Passed() {
		t.Fatalf("first run: want fresh pass, got hit=%v result=%s", first.CacheHit, first.Result)
	}
	second := f.run(nil, cache)
	if !second.CacheHit || !second.Passed() {
		t.Fatalf("second run: want cache hit pass, got hit=%v result=%s", second.CacheHit, second.Result)
	}
	if second.TreeSHA != first.TreeSHA {
		t.Fatal("tree sha must match across runs")
	}

	// Change the scope: gates 1–3 recompute, cache key changes → miss.
	third := f.run([]string{"src/**"}, cache)
	if third.CacheHit {
		t.Fatal("changed scope must miss the cache")
	}
}

// Failing verdicts are not cached, so a flaky failure is retried, not frozen.
func TestFailureNotCached(t *testing.T) {
	cache := t.TempDir()
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "docs/x.md", "no feature file")
	f.commit("no feature")

	f.run(nil, cache)
	entries, _ := filepath.Glob(filepath.Join(cache, "*.json"))
	if len(entries) != 0 {
		t.Fatalf("failing tree must not be cached, found %v", entries)
	}
}

func TestCustomCommandsRunInOrder(t *testing.T) {
	f := newFixture(t, `
[verify]
custom = ["test -f src/feature.txt", "test -f missing.txt"]
`, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	v := f.run(nil, "")
	if len(v.Commands) != 2 || v.Commands[0].Name != "custom[0]" || v.Commands[1].Name != "custom[1]" {
		t.Fatalf("custom order wrong: %+v", v.Commands)
	}
	if g := gate(v, "cmd:custom[1]"); g.Status != events.GateFail {
		t.Fatalf("custom[1]: want fail, got %+v", g)
	}
	if v.Passed() {
		t.Fatal("one failing custom command must fail the verdict")
	}
}

func TestMissingConfigAtBaseIsInfraError(t *testing.T) {
	f := newFixture(t, baseToml, nil)
	git(t, f.dir, "rm", "-q", "nightrunner.toml")
	f.commit("drop config")
	_, err := Run(context.Background(), Options{
		Repo: f.repo, Task: "1.a", Base: f.head(), Head: f.head(),
	})
	if err == nil {
		t.Fatal("expected infra error when base has no nightrunner.toml")
	}
}

// ---- adversarial cases from the review (2026-10-26) ----

// A worker cannot swap the base config by writing git replace refs into the
// shared repo: the verifier reads objects with replace refs disabled.
func TestReplaceRefCannotSwapBaseConfig(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "README", "r2") // no src/feature.txt: the base test must fail
	f.commit("unrelated")
	baseBlob := git(t, f.dir, "rev-parse", f.base+":nightrunner.toml")
	weakBlob := hashObject(t, f.dir, "[verify]\ntest = \"true\"\n")
	git(t, f.dir, "replace", baseBlob, weakBlob)

	v := f.run(nil, "")
	if v.Passed() {
		t.Fatal("replace ref must not swap the base config: want FAIL")
	}
}

func hashObject(t *testing.T, dir, content string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// A hook in the shared .git must never run during verification.
func TestRepoHooksDoNotRun(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	hp := filepath.Join(f.dir, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hp, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	f.run(nil, "")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook in .git/hooks ran during verification")
	}
}

// A forged cache entry (right key, no gates) must be a miss, not a PASS.
func TestForgedCacheEntryIsRejected(t *testing.T) {
	cache := t.TempDir()
	f := newFixture(t, `
[verify]
test = "false"
`, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	tree := git(t, f.dir, "rev-parse", "HEAD^{tree}")
	cmds := []Command{{Name: "test", Shell: "false", Timeout: 600 * time.Second}}
	key := cacheKey(tree, cmds, DefaultProtected, DefaultLocked, nil)
	forged := `{"key":"` + key + `","tree_sha":"` + tree + `","gates":[],"commands":[]}`
	if err := os.WriteFile(filepath.Join(cache, key+".json"), []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}

	v := f.run(nil, cache)
	if v.CacheHit || v.Passed() {
		t.Fatalf("forged entry must not be used: hit=%v result=%s", v.CacheHit, v.Result)
	}
}

func TestValidCacheRules(t *testing.T) {
	cmds := []Command{{Name: "test"}, {Name: "lint"}}
	good := cacheEntry{Key: "k", TreeSHA: "t",
		Commands: []events.CommandResult{{Name: "test"}, {Name: "lint"}},
		Gates: []events.GateData{
			{Name: GateCleanCheckout, Status: events.GatePass},
			{Name: "cmd:test", Status: events.GatePass},
			{Name: "cmd:lint", Status: events.GatePass}}}
	if !validCache(good, "k", "t", cmds) {
		t.Fatal("good entry rejected")
	}
	bad := good
	bad.Gates = good.Gates[:2] // lint gate missing
	if validCache(bad, "k", "t", cmds) {
		t.Fatal("entry missing a cmd gate accepted")
	}
	bad = good
	bad.Gates = append([]events.GateData{}, good.Gates...)
	bad.Gates[1].Status = events.GateFail
	if validCache(bad, "k", "t", cmds) {
		t.Fatal("failing gate accepted")
	}
	if validCache(good, "k", "other-tree", cmds) {
		t.Fatal("tree mismatch accepted")
	}
}

// Changing a locked file (the config itself) fails, regardless of scope.
func TestLockedConfigChangeFails(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	write(t, f.dir, "nightrunner.toml", "[verify]\ntest = \"true\"\n")
	f.commit("feature + weakened config")

	v := f.run([]string{"src/**", "nightrunner.toml"}, "")
	if g := gate(v, GateLockedPaths); g.Status != events.GateFail {
		t.Fatalf("locked_paths: want fail, got %+v", g)
	}
	if v.Passed() {
		t.Fatal("config change must fail even when in scope")
	}
}

// Go keeps testdata per package, so a nested testdata file is protected.
func TestNestedTestdataIsProtected(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{
		"src/feature.txt":         "x",
		"pkg/testdata/golden.txt": "expected",
	})
	write(t, f.dir, "pkg/testdata/golden.txt", "edited to match")
	f.commit("edit golden")

	v := f.run(nil, "")
	if g := gate(v, GateProtectedPaths); g.Status != events.GateFail {
		t.Fatalf("nested testdata edit must fail protected_paths, got %+v", g)
	}
}

// A config with no commands cannot pass: nothing would be checked.
func TestNoCommandsFails(t *testing.T) {
	f := newFixture(t, "[verify]\n", map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	v := f.run(nil, "")
	if v.Passed() {
		t.Fatal("no configured commands must not pass")
	}
	if g := gate(v, GateCommandsPresent); g.Status != events.GateFail {
		t.Fatalf("commands_present: want fail, got %+v", g)
	}
}

// Background children of a passing command are killed when it exits, so they
// cannot write after the verdict.
func TestBackgroundChildKilledAfterCommand(t *testing.T) {
	late := filepath.Join(t.TempDir(), "late")
	f := newFixture(t, "[verify]\ntest = \"(sleep 2; touch "+late+") & exit 0\"\n",
		map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	v := f.run(nil, "")
	if !v.Passed() {
		t.Fatalf("want pass (exit 0), got %v", v.FailedGates)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(late); err == nil {
		t.Fatal("background child wrote after the verdict")
	}
}

// A setsid child must not hang the verifier (output goes to a file, not a
// pipe). It can outlive the group; that limit is documented in exec.go.
func TestSetsidChildDoesNotHangVerifier(t *testing.T) {
	f := newFixture(t, "[verify]\ntest = \"setsid sleep 20 & exit 0\"\n",
		map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	start := time.Now()
	v := f.run(nil, "")
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("verifier hung on setsid child: %s", d)
	}
	if !v.Passed() {
		t.Fatalf("want pass, got %v", v.FailedGates)
	}
}

// A symlink that escapes the checkout makes the export fail, which must be a
// clean_checkout FAIL, not a skip.
func TestEscapingSymlinkFailsCheckout(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	if err := os.Symlink("../../../../etc", filepath.Join(f.dir, "evil")); err != nil {
		t.Fatal(err)
	}
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("escaping symlink")

	v := f.run(nil, "")
	g := gate(v, GateCleanCheckout)
	if g.Status != events.GateFail {
		t.Fatalf("clean_checkout: want fail for escaping symlink, got %+v", g)
	}
	if v.Passed() {
		t.Fatal("escaping symlink must not pass")
	}
}

// Cancellation is an error (exit 2), never a verdict.
func TestCancelledContextIsError(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Options{Repo: f.repo, Task: "1.a", Base: f.base, Head: f.head(),
		LogDir: t.TempDir(), WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("cancelled run must return an error, not a verdict")
	}
}

// A corrupted loose object (same id, different bytes) must fail object_integrity,
// never pass and never be a silent infra error.
func TestCorruptedObjectFailsIntegrity(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	blob := git(t, f.dir, "rev-parse", f.base+":nightrunner.toml")
	loose := filepath.Join(f.dir, ".git", "objects", blob[:2], blob[2:])
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatalf("base config blob is not loose: %v", err)
	}
	if err := os.WriteFile(loose, []byte("not zlib, forged"), 0o644); err != nil {
		t.Fatal(err)
	}

	v := f.run(nil, "")
	if v.Passed() {
		t.Fatal("corrupted object must not pass")
	}
	if g := gate(v, GateObjectIntegrity); g.Status != events.GateFail {
		t.Fatalf("object_integrity: want fail, got %+v", g)
	}
}

// Worker-written attributes in the shared .git must not change the export.
// The base test requires pkg/a_test.go; export-ignore must not hide it.
func TestSharedAttributesDoNotChangeExport(t *testing.T) {
	f := newFixture(t, `
[verify]
test = "test -f pkg/a_test.go && test -f src/feature.txt"
`, map[string]string{"pkg/a_test.go": "package pkg"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")
	attrs := filepath.Join(f.dir, ".git", "info", "attributes")
	if err := os.WriteFile(attrs, []byte("*_test.go export-ignore\n* export-subst\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	v := f.run(nil, "")
	if !v.Passed() {
		t.Fatalf("attributes must not hide files from the export: %v", v.FailedGates)
	}
}

// A worker-configured upload-pack hook in its own repo config must not run.
func TestWorkerTransferHookDoesNotRun(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")
	marker := filepath.Join(t.TempDir(), "transfer-hook-ran")
	git(t, f.dir, "config", "uploadpack.packObjectsHook", "touch "+marker)

	v := f.run(nil, "")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("uploadpack.packObjectsHook from the worker repo ran")
	}
	if !v.Passed() {
		t.Fatalf("honest task should pass: %v", v.FailedGates)
	}
}

// Refs are not accepted as base or head: a ref can be moved by the worker.
func TestRefsRejected(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")
	_, err := Run(context.Background(), Options{Repo: f.repo, Task: "1.a",
		Base: f.base, Head: "task/x", LogDir: t.TempDir(), WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("a branch name as head must be rejected")
	}
}

// Relative PATH entries would resolve inside the export, so they are dropped.
func TestRelativePathEntriesDropped(t *testing.T) {
	got := absolutePath("/usr/bin:bin:.:/opt/tools:")
	if got != "/usr/bin:/opt/tools" {
		t.Fatalf("absolutePath = %q", got)
	}
}

// A shallow root in the worker repo: git fetch can exit 0 while refusing the
// ref update. That must be an integrity FAIL, never a PASS and never exit 2.
func TestShallowRootIsIntegrityFail(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")
	if err := os.WriteFile(filepath.Join(f.dir, ".git", "shallow"), []byte(f.head()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	v, err := Run(context.Background(), Options{Repo: f.repo, Task: "1.a",
		Base: f.base, Head: f.head(), LogDir: t.TempDir(), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("shallow root must be a verdict, not an error: %v", err)
	}
	if v.Passed() {
		t.Fatal("shallow root must not pass")
	}
	if g := gate(v, GateObjectIntegrity); g.Status != events.GateFail {
		t.Fatalf("object_integrity: want fail, got %+v", g)
	}
}

// Defaults always apply: a project that locks go.mod must still lock nightrunner.toml.
func TestDefaultsAlwaysApply(t *testing.T) {
	f := newFixture(t, `
[verify]
test = "test -f src/feature.txt"
[verify.locked]
paths = ["go.mod"]
[verify.protected]
paths = ["docs/**"]
`, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	write(t, f.dir, "nightrunner.toml", "[verify]\ntest = \"true\"\n")
	f.commit("feature + weakened config")

	v := f.run(nil, "")
	if g := gate(v, GateLockedPaths); g.Status != events.GateFail {
		t.Fatalf("config must stay locked when project adds locked paths: %+v", g)
	}
}

// A commit that changes no files is not work.
func TestEmptyCommitIsNotWork(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"src/feature.txt": "x"})
	git(t, f.dir, "commit", "-q", "--allow-empty", "-m", "nothing")
	v := f.run(nil, "")
	if g := gate(v, GateCommittedWork); g.Status != events.GateFail {
		t.Fatalf("empty commit must fail committed_work: %+v", g)
	}
}

// Bumping CacheVersion must miss every entry written under the old version.
// This is the documented way to invalidate the cache after a toolchain change.
func TestCacheVersionBumpMisses(t *testing.T) {
	cache := t.TempDir()
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")

	if v := f.run(nil, cache); v.CacheHit || !v.Passed() {
		t.Fatalf("first run: want fresh pass, got hit=%v result=%s", v.CacheHit, v.Result)
	}
	if v := f.run(nil, cache); !v.CacheHit {
		t.Fatal("second run: want cache hit before the bump")
	}

	t.Cleanup(func() { cacheVersion = CacheVersion })
	cacheVersion = CacheVersion + "-bumped"
	if v := f.run(nil, cache); v.CacheHit || !v.Passed() {
		t.Fatalf("after bump: want fresh pass (miss), got hit=%v result=%s", v.CacheHit, v.Result)
	}
	if v := f.run(nil, cache); !v.CacheHit {
		t.Fatal("after bump: the new entry should hit on the next run")
	}
}

// Each default locked path is pinned by its own case: changing the file fails
// locked_paths even when the scope allows it and the project adds its own
// locked globs. Removing a default from DefaultLocked fails its case, and
// adding one without a case fails the coverage check.
func TestEachDefaultLockHolds(t *testing.T) {
	cases := []struct {
		glob, path string
		atBase     bool // file exists at base (modify) or not (add)
	}{
		{"nightrunner.toml", "nightrunner.toml", true},
		{".gitattributes", ".gitattributes", false},
		{".mcp.json", ".mcp.json", true},
		{".claude/**", ".claude/settings.json", true},
		{".claude/**", ".claude/agents/new.md", false},
		{"CLAUDE.md", "CLAUDE.md", true},
	}
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.glob] = true
	}
	for _, g := range DefaultLocked {
		if !covered[g] {
			t.Errorf("default locked glob %q has no case in this test", g)
		}
	}

	toml := baseToml + "[verify.locked]\npaths = [\"go.mod\"]\n"
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			baseFiles := map[string]string{"README": "r"}
			if c.atBase && c.path != ConfigPath {
				baseFiles[c.path] = "original\n"
			}
			f := newFixture(t, toml, baseFiles)
			write(t, f.dir, "src/feature.txt", "x")
			write(t, f.dir, c.path, "changed by the worker\n")
			f.commit("feature + " + c.path)

			v := f.run([]string{"src/**", c.path}, "")
			g := gate(v, GateLockedPaths)
			if g.Status != events.GateFail || !strings.Contains(g.Detail, c.path) {
				t.Fatalf("locked_paths: want fail naming %s, got %+v", c.path, g)
			}
			if v.Passed() {
				t.Fatalf("change to %s must not pass", c.path)
			}
		})
	}
}

// A relative project dir (the CLI default is ".") must name the worker repo,
// not resolve against the verifier's own repo during the transfer.
func TestRelativeProjectDir(t *testing.T) {
	f := newFixture(t, baseToml, map[string]string{"README": "r"})
	write(t, f.dir, "src/feature.txt", "x")
	f.commit("feature")
	head := f.head()
	t.Chdir(f.dir)

	v, err := Run(context.Background(), Options{Repo: gitx.Repo{Dir: "."}, Task: "1.a",
		Base: f.base, Head: head, LogDir: t.TempDir(), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Passed() {
		t.Fatalf("relative project dir: want pass, got %v; gates: %+v", v.FailedGates, v.Gates)
	}
}
