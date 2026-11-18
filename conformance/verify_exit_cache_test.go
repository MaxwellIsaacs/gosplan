// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// verify_exit_cache_test.go: GREEN NOW. Exit codes with cancellation, the
// verdict cache's validity rules, and the M1 config defaults. Black box:
// the `gosplan verify` CLI, its --cache dir as an opaque directory, and files the
// configured commands write.
package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exit 0, 1 and 2 for pass, gate failure and could-not-verify.
//
// Clause: 01-verify-contract.md#VC-EXIT
func TestVerifyExitCodes(t *testing.T) {
	r := honestRepo(t)
	if _, res := r.verify(); res.code != 0 {
		t.Fatalf("pass: want exit 0, got %s", res)
	}
	f := newRepo(t, featureToml, map[string]string{"README": "r"})
	writeFile(t, f.dir, "README", "r2")
	f.commit("no feature")
	if _, res := f.verify(); res.code != 1 {
		t.Fatalf("gate failure: want exit 1, got %s", res)
	}
	noCfg := newRepo(t, "", map[string]string{"README": "r"})
	writeFile(t, noCfg.dir, "src/feature.txt", "x")
	noCfg.commit("feature")
	if _, res := noCfg.verify(); res.code != 2 {
		t.Fatalf("missing config at base: want exit 2, got %s", res)
	}
	// An unknown head is missing history: a transfer failure, so an
	// object_integrity FAIL (exit 1), never a pass (contract, Trust model 1).
	missing := strings.Repeat("ab", 20)
	if v, res := verifyAt(t, r.dir, r.base, missing); res.code != 1 || v.FailedGates[0] != "object_integrity" {
		t.Fatalf("unknown head SHA: want exit 1 with object_integrity failed, got %s", res)
	}
	if res := gosplan(t, "", nil, "verify", "--task", "1.a"); res.code != 2 {
		t.Fatalf("usage error: want exit 2, got %s", res)
	}
}

// SIGTERM during a command: exit 2, the command's process group is killed
// and the export and HOME dirs are removed.
//
// Clause: 01-verify-contract.md#VC-CANCEL
// Clause: 01-verify-contract.md#VC-EXIT
func TestVerifyCancelCleansUp(t *testing.T) {
	marks := t.TempDir()
	pidFile := filepath.Join(marks, "child.pid")
	toml := "[verify]\ntest = \"sleep 600 & echo $! > " + pidFile + "; wait\"\n"
	r := newRepo(t, toml, map[string]string{"README": "r"})
	writeFile(t, r.dir, "src/feature.txt", "x")
	r.commit("feature")
	tmp := t.TempDir()
	cmd := exec.Command(nrBin, "verify", "--project", r.dir, "--task", "1.a", "--base", r.base, "--head", r.head())
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if !eventually(2*time.Minute, func() bool { _, err := os.Stat(pidFile); return err == nil }) {
		_ = cmd.Process.Kill()
		t.Fatal("the verify command never started")
	}
	time.Sleep(time.Second) // let the shell finish writing the pid
	childPID, _ := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("gosplan verify did not exit within a minute of SIGTERM")
	}
	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("signalled verify: want exit 2, got %d", code)
	}
	if !eventually(10*time.Second, func() bool { return !pidAlive(childPID) }) {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("command child %d survived cancellation", childPID)
	}
	ents, _ := os.ReadDir(tmp)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "gosplan-") {
			t.Fatalf("cancelled verify left %s in its temp dir", e.Name())
		}
	}
}

// Same tree + same key hits; the hit keeps the result.
//
// Clause: 01-verify-contract.md#VC-CACHE-HIT
func TestCacheHitSameTree(t *testing.T) {
	r := honestRepo(t)
	cache := t.TempDir()
	v1, res := r.verify("--cache", cache)
	if res.code != 0 || v1.CacheHit {
		t.Fatalf("first run: want fresh pass, got %s", res)
	}
	v2, res := r.verify("--cache", cache)
	if res.code != 0 || !v2.CacheHit || v2.TreeSHA != v1.TreeSHA {
		t.Fatalf("second run: want cache hit pass on tree %s, got %s", v1.TreeSHA, res)
	}
}

// Changing a command, its timeout, or the scope misses.
//
// Clause: 01-verify-contract.md#VC-CACHE-KEY
func TestCacheKeyChangesMiss(t *testing.T) {
	r := honestRepo(t)
	cache := t.TempDir()
	if v, res := r.verify("--cache", cache); res.code != 0 || v.CacheHit {
		t.Fatalf("prime: %s", res)
	}
	if v, res := r.verify("--cache", cache, "--scope", "src/**"); res.code != 0 || v.CacheHit {
		t.Fatalf("scope change must miss, got hit=%v %s", v.CacheHit, res)
	}
	// Same head tree, other base configs: another command, another timeout,
	// another protected glob. The config is read from base, so only the key
	// changes (gates 1-5 may fail on the toml diff; the cache must still miss).
	head := r.head()
	for i, toml := range []string{
		"[verify]\ntest = \"test -f src/feature.txt && true\"\n",
		featureToml + "[verify.timeouts]\ntest_seconds = 77\n",
		featureToml + "[verify.protected]\npaths = [\"docs/**\"]\n",
	} {
		br := "base" + strconv.Itoa(i)
		gitc(t, r.dir, "checkout", "-q", "-b", br, r.base)
		writeFile(t, r.dir, "nightrunner.toml", toml)
		gitc(t, r.dir, "commit", "-qam", "other config")
		base := gitc(t, r.dir, "rev-parse", "HEAD")
		gitc(t, r.dir, "checkout", "-q", "task/x")
		if v, res := verifyAt(t, r.dir, base, head, "--cache", cache); res.code == 2 || v.CacheHit {
			t.Fatalf("config %q must miss, got hit=%v %s", toml, v.CacheHit, res)
		}
	}
	// Control: the original key still hits.
	if v, res := verifyAt(t, r.dir, r.base, head, "--cache", cache); res.code != 0 || !v.CacheHit {
		t.Fatalf("original key must still hit, got %s", res)
	}
}

// A failed command is never cached.
//
// Clause: 01-verify-contract.md#VC-CACHE-PASS-ONLY
func TestCacheFailNotStored(t *testing.T) {
	r := newRepo(t, featureToml, map[string]string{"README": "r"})
	writeFile(t, r.dir, "README", "r2")
	r.commit("no feature")
	cache := t.TempDir()
	for i := 0; i < 2; i++ {
		v, res := r.verify("--cache", cache)
		if res.code != 1 || v.CacheHit {
			t.Fatalf("run %d: a failing tree must fail fresh, got hit=%v %s", i, v.CacheHit, res)
		}
	}
}

// Truncated or emptied entries are misses and the run still verifies.
//
// Clause: 01-verify-contract.md#VC-CACHE-INVALID
func TestCachePartialEntriesMiss(t *testing.T) {
	for _, mode := range []string{"empty", "partial"} {
		t.Run(mode, func(t *testing.T) {
			r := honestRepo(t)
			cache := t.TempDir()
			if _, res := r.verify("--cache", cache); res.code != 0 {
				t.Fatalf("prime: %s", res)
			}
			n := 0
			_ = filepath.Walk(cache, func(p string, fi os.FileInfo, err error) error {
				if err != nil || !fi.Mode().IsRegular() {
					return nil
				}
				n++
				size := int64(0)
				if mode == "partial" {
					size = fi.Size() / 2
				}
				return os.Truncate(p, size)
			})
			if n == 0 {
				t.Fatal("a passing verify with --cache wrote no cache entry")
			}
			v, res := r.verify("--cache", cache)
			if res.code != 0 || v.CacheHit {
				t.Fatalf("%s entries: want a fresh pass (miss), got hit=%v %s", mode, v.CacheHit, res)
			}
		})
	}
}

// A cache hit on the same tree still runs gates 1-5: a different base
// whose diff touches a protected file fails, even though the tree is cached.
//
// Clause: 01-verify-contract.md#VC-CACHE-SCOPE-OF
func TestCacheHitStillChecksDiffGates(t *testing.T) {
	r := newRepo(t, featureToml, map[string]string{"pkg/a_test.go": "package pkg // original\n"})
	gitc(t, r.dir, "checkout", "-q", "master")
	writeFile(t, r.dir, "pkg/a_test.go", "package pkg // weakened\n")
	gitc(t, r.dir, "commit", "-qam", "tree with the weakened test")
	honestBase := gitc(t, r.dir, "rev-parse", "HEAD")
	writeFile(t, r.dir, "src/feature.txt", "x")
	gitc(t, r.dir, "add", "-A")
	gitc(t, r.dir, "commit", "-qm", "feature")
	head := gitc(t, r.dir, "rev-parse", "HEAD")
	cache := t.TempDir()
	if v, res := verifyAt(t, r.dir, honestBase, head, "--cache", cache); res.code != 0 || v.CacheHit {
		t.Fatalf("prime from honest base: want fresh pass, got %s", res)
	}
	// Same head tree, but measured from the original base the diff edits a test.
	v, res := verifyAt(t, r.dir, r.base, head, "--cache", cache)
	wantGate(t, v, res, "protected_paths", "fail", "pkg/a_test.go")
}

// test, lint, build, custom[] run in that order through bash, each only if set.
//
// Clause: 02-protected-config.md#CF-COMMANDS
func TestConfigCommandsOrderAndShell(t *testing.T) {
	order := filepath.Join(t.TempDir(), "order")
	add := func(n string) string { return "[[ 1 == 1 ]] && echo " + n + " >> " + order }
	toml := "[verify]\n" +
		"custom = [\"" + add("c0") + "\", \"" + add("c1") + "\"]\n" +
		"build = \"" + add("build") + "\"\n" +
		"test = \"" + add("test") + "\"\n"
	r := newRepo(t, toml, map[string]string{"README": "r"})
	writeFile(t, r.dir, "README", "r2")
	r.commit("work")
	v, res := r.verify()
	if res.code != 0 {
		t.Fatalf("want pass (bash-only syntax must work), got %s", res)
	}
	want := []string{"object_integrity", "committed_work", "locked_paths", "protected_paths", "scope", "commands_present", "clean_checkout", "cmd:test", "cmd:build", "cmd:custom[0]", "cmd:custom[1]"}
	if strings.Join(v.gateNames(), ",") != strings.Join(want, ",") {
		t.Fatalf("gates: want %v (lint unset, so absent), got %v", want, v.gateNames())
	}
	if got := strings.Fields(readFile(t, order)); strings.Join(got, ",") != "test,build,c0,c1" {
		t.Fatalf("run order: want test,build,c0,c1, got %v", got)
	}
}

// custom_seconds applies to every custom entry; default timeouts let a
// short command finish.
//
// Clause: 02-protected-config.md#CF-TIMEOUTS
func TestConfigTimeouts(t *testing.T) {
	r := newRepo(t, "[verify]\ntest = \"sleep 2\"\ncustom = [\"true\", \"sleep 30\"]\n[verify.timeouts]\ncustom_seconds = 1\n", map[string]string{"README": "r"})
	writeFile(t, r.dir, "README", "r2")
	r.commit("work")
	v, res := r.verify()
	wantGate(t, v, res, "cmd:test", "pass", "")
	wantGate(t, v, res, "cmd:custom[0]", "pass", "")
	wantGate(t, v, res, "cmd:custom[1]", "fail", "")
}

// Project protected globs add to the defaults.
//
// Clause: 02-protected-config.md#CF-PROTECTED-DEFAULT
// Clause: 01-verify-contract.md#VC-PROTECTED-PATHS
func TestConfigProtectedAdditive(t *testing.T) {
	toml := featureToml + "[verify.protected]\npaths = [\"docs/**\"]\n"
	for _, p := range []string{"docs/spec.md", "pkg/a_test.go"} {
		r := newRepo(t, toml, map[string]string{p: "orig\n"})
		writeFile(t, r.dir, "src/feature.txt", "x")
		writeFile(t, r.dir, p, "edited\n")
		r.commit("edit " + p)
		v, res := r.verify()
		wantGate(t, v, res, "protected_paths", "fail", p)
	}
}
