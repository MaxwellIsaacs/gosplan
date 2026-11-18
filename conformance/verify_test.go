// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// verify_test.go: GREEN NOW. One test per `gosplan verify` gate (the golden
// fixtures in testdata/golden where one exists), plus gate order, refs and
// the config-from-base rule. Black box: the `gosplan verify --json` CLI only.
package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wantGate(t *testing.T, v verdict, res result, gate, status, detailSub string) {
	t.Helper()
	st, d := v.gate(gate)
	if st != status || !strings.Contains(d, detailSub) {
		t.Fatalf("gate %s: want %s (detail containing %q), got %s %q\n%s", gate, status, detailSub, st, d, res)
	}
	if status == "fail" {
		if res.code != 1 || v.Result != "fail" {
			t.Fatalf("a failed gate must give exit 1 and result fail, got %s", res)
		}
		found := false
		for _, g := range v.FailedGates {
			found = found || g == gate
		}
		if !found {
			t.Fatalf("failed_gates %v must list %s", v.FailedGates, gate)
		}
	}
}

// Golden honest-pass: every gate passes or skips, exit 0.
//
// Clause: 01-verify-contract.md#VC-RESULT
// Clause: 01-verify-contract.md#VC-EXIT
func TestGateGoldenHonestPass(t *testing.T) {
	v := checkGolden(t, "honest-pass")
	for _, g := range v.Gates {
		if g.Status == "fail" {
			t.Fatalf("honest pass has a failed gate %s", g.Name)
		}
	}
	if v.Unrestricted {
		t.Fatal("honest-pass runs with --scope, so it is not unrestricted")
	}
}

// All gates run, in contract order, even after an early failure.
//
// Clause: 01-verify-contract.md#VC-GATE-ORDER
func TestGateOrderAllRunAfterFailure(t *testing.T) {
	r := newRepo(t, featureToml, map[string]string{"README": "r"})
	v, res := r.verify() // no commits: committed_work fails first
	if res.code != 1 {
		t.Fatalf("want exit 1, got %s", res)
	}
	if got := strings.Join(v.gateNames(), ","); got != strings.Join(gateOrder, ",") {
		t.Fatalf("gate order: want %v, got %v", gateOrder, v.gateNames())
	}
}

// Clause: 01-verify-contract.md#VC-OBJECT-INTEGRITY
func TestGateObjectIntegrity(t *testing.T) {
	t.Run("corrupt_loose_object", func(t *testing.T) {
		r := honestRepo(t)
		blob := gitc(t, r.dir, "rev-parse", r.base+":nightrunner.toml")
		loose := filepath.Join(r.dir, ".git", "objects", blob[:2], blob[2:])
		if err := os.Chmod(loose, 0o644); err != nil {
			t.Fatalf("base config blob is not loose: %v", err)
		}
		if err := os.WriteFile(loose, []byte("not zlib, forged"), 0o644); err != nil {
			t.Fatal(err)
		}
		v, res := r.verify()
		wantGate(t, v, res, "object_integrity", "fail", "")
	})
	t.Run("shallow_root", func(t *testing.T) {
		r := honestRepo(t)
		writeFile(t, r.dir, ".git/shallow", r.head()+"\n")
		v, res := r.verify()
		wantGate(t, v, res, "object_integrity", "fail", "")
	})
}

// Clause: 01-verify-contract.md#VC-COMMITTED-WORK
func TestGateCommittedWork(t *testing.T) {
	t.Run("no_commit", func(t *testing.T) {
		r := newRepo(t, featureToml, map[string]string{"src/feature.txt": "x"})
		v, res := r.verify()
		wantGate(t, v, res, "committed_work", "fail", "")
	})
	t.Run("empty_commit", func(t *testing.T) {
		r := newRepo(t, featureToml, map[string]string{"src/feature.txt": "x"})
		gitc(t, r.dir, "commit", "-q", "--allow-empty", "-m", "nothing")
		v, res := r.verify()
		wantGate(t, v, res, "committed_work", "fail", "")
	})
}

// Each default lock holds even when scope allows the file and the project
// adds its own locked globs; project globs are added, not substituted.
//
// Clause: 01-verify-contract.md#VC-LOCKED-PATHS
// Clause: 01-verify-contract.md#VC-LOCKED-DEFAULTS
func TestGateLockedPaths(t *testing.T) {
	toml := featureToml + "[verify.locked]\npaths = [\"go.mod\"]\n"
	cases := []struct {
		path   string
		atBase bool
	}{
		{"nightrunner.toml", true}, {".gitattributes", false}, {".mcp.json", true},
		{".claude/settings.json", true}, {".claude/agents/new.md", false}, {"CLAUDE.md", true},
		{"go.mod", true}, // project-added lock
	}
	for _, c := range cases {
		t.Run(strings.NewReplacer("/", "_", ".", "_").Replace(c.path), func(t *testing.T) {
			files := map[string]string{"README": "r"}
			if c.atBase && c.path != "nightrunner.toml" {
				files[c.path] = "original\n"
			}
			r := newRepo(t, toml, files)
			writeFile(t, r.dir, "src/feature.txt", "x")
			writeFile(t, r.dir, c.path, "changed by the worker\n")
			r.commit("feature + " + c.path)
			v, res := r.verify("--scope", "src/**", "--scope", c.path)
			wantGate(t, v, res, "locked_paths", "fail", c.path)
		})
	}
}

// Golden tampered-protected-test, plus deletion and nested testdata.
//
// Clause: 01-verify-contract.md#VC-PROTECTED-PATHS
func TestGateProtectedPaths(t *testing.T) {
	t.Run("golden_tampered", func(t *testing.T) { checkGolden(t, "tampered-protected-test") })
	t.Run("delete_test", func(t *testing.T) {
		r := newRepo(t, featureToml, map[string]string{"pkg/a_test.go": "package pkg\n"})
		writeFile(t, r.dir, "src/feature.txt", "x")
		gitc(t, r.dir, "rm", "-q", "pkg/a_test.go")
		r.commit("delete the test")
		v, res := r.verify()
		wantGate(t, v, res, "protected_paths", "fail", "pkg/a_test.go")
	})
	t.Run("nested_testdata_and_tests_dir", func(t *testing.T) {
		for _, p := range []string{"a/b/testdata/x.json", "tests/t.sh"} {
			r := newRepo(t, featureToml, map[string]string{p: "orig\n"})
			writeFile(t, r.dir, "src/feature.txt", "x")
			writeFile(t, r.dir, p, "edited\n")
			r.commit("edit " + p)
			v, res := r.verify()
			wantGate(t, v, res, "protected_paths", "fail", p)
		}
	})
}

// Adding a protected file needs --allow-new-protected.
//
// Clause: 01-verify-contract.md#VC-PROTECTED-NEW
func TestGateProtectedNewFile(t *testing.T) {
	r := newRepo(t, featureToml, map[string]string{"README": "r"})
	writeFile(t, r.dir, "src/feature.txt", "x")
	writeFile(t, r.dir, "pkg/zz_test.go", "package pkg\n")
	r.commit("feature + new test")
	v, res := r.verify()
	wantGate(t, v, res, "protected_paths", "fail", "pkg/zz_test.go")
	v, res = r.verify("--allow-new-protected")
	if res.code != 0 {
		t.Fatalf("with --allow-new-protected an added test file passes, got %s", res)
	}
	wantGate(t, v, res, "protected_paths", "pass", "")
}

// Golden out-of-scope names the stray path.
//
// Clause: 01-verify-contract.md#VC-SCOPE
func TestGateScope(t *testing.T) {
	v := checkGolden(t, "out-of-scope")
	if st, _ := v.gate("scope"); st != "fail" {
		t.Fatalf("scope gate: %s", st)
	}
}

// Clause: 01-verify-contract.md#VC-SCOPE-UNRESTRICTED
func TestGateScopeUnrestricted(t *testing.T) {
	r := honestRepo(t)
	v, res := r.verify()
	wantGate(t, v, res, "scope", "skip", "")
	if res.code != 0 || !v.Unrestricted {
		t.Fatalf("no --scope: want pass with scope_unrestricted true, got %s", res)
	}
}

// Clause: 01-verify-contract.md#VC-COMMANDS-PRESENT
func TestGateCommandsPresent(t *testing.T) {
	r := newRepo(t, "[verify]\n", map[string]string{"README": "r"})
	writeFile(t, r.dir, "src/feature.txt", "x")
	r.commit("feature")
	v, res := r.verify()
	wantGate(t, v, res, "commands_present", "fail", "")
}

// A symlink in head fails the clean export; commands are then not run.
//
// Clause: 01-verify-contract.md#VC-CLEAN-CHECKOUT
func TestGateCleanCheckout(t *testing.T) {
	r := newRepo(t, featureToml, map[string]string{"README": "r"})
	writeFile(t, r.dir, "src/feature.txt", "x")
	if err := os.Symlink("/etc/passwd", filepath.Join(r.dir, "src", "link")); err != nil {
		t.Fatal(err)
	}
	r.commit("feature + escaping symlink")
	v, res := r.verify()
	wantGate(t, v, res, "clean_checkout", "fail", "")
	if st, _ := v.gate("cmd:test"); st == "pass" {
		t.Fatal("cmd:test must not pass after a failed clean checkout")
	}
}

// Golden uncommitted-only-pass: commands see only committed files.
//
// Clause: 01-verify-contract.md#VC-CMD
func TestGateCmdCleanExport(t *testing.T) {
	v := checkGolden(t, "uncommitted-only-pass")
	if st, _ := v.gate("cmd:test"); st != "fail" {
		t.Fatalf("cmd:test: %s", st)
	}
}

// A command that exceeds its timeout fails its gate.
//
// Clause: 01-verify-contract.md#VC-CMD
// Clause: 02-protected-config.md#CF-TIMEOUTS
func TestGateCmdTimeout(t *testing.T) {
	r := newRepo(t, "[verify]\ntest = \"sleep 30\"\n[verify.timeouts]\ntest_seconds = 1\n", map[string]string{"README": "r"})
	writeFile(t, r.dir, "src/feature.txt", "x")
	r.commit("feature")
	v, res := r.verify()
	wantGate(t, v, res, "cmd:test", "fail", "")
}

// The worker's toml edit cannot change the command; the base config runs.
//
// Clause: 01-verify-contract.md#VC-CONFIG-FROM-BASE
func TestConfigReadFromBase(t *testing.T) {
	r := newRepo(t, featureToml, map[string]string{"README": "r"})
	writeFile(t, r.dir, "nightrunner.toml", "[verify]\ntest = \"true\"\n")
	writeFile(t, r.dir, "README", "r2")
	r.commit("weaken the verify command")
	v, res := r.verify()
	wantGate(t, v, res, "cmd:test", "fail", "")
	wantGate(t, v, res, "locked_paths", "fail", "nightrunner.toml")
}

// Refs and short SHAs are rejected (exit 2), for --base and --head.
//
// Clause: 01-verify-contract.md#VC-REFS
// Clause: 01-verify-contract.md#VC-EXIT
func TestVerifyRejectsRefs(t *testing.T) {
	r := honestRepo(t)
	head := r.head()
	for _, c := range [][2]string{{"master", head}, {r.base, "task/x"}, {r.base[:12], head}, {r.base, "HEAD"}} {
		if _, res := verifyAt(t, r.dir, c[0], c[1]); res.code != 2 {
			t.Fatalf("base=%s head=%s: want exit 2, got %s", c[0], c[1], res)
		}
	}
}
