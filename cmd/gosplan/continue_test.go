package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunE2EContinue: a run halts with 2.b failed and 3.a abandoned. After
// the fix, `--continue <run>` starts a new run of exactly 2.b and 3.a on the
// first run's integration tip; it completes, its integration branch holds
// every task's work, and the first run's record is untouched.
func TestRunE2EContinue(t *testing.T) {
	f := runNewFix(t, "e2e-spec")
	runScenario(t, f.scn, "2.b", map[string][]map[string]any{"*": runCommitSteps("2.b", "BAD\n")})
	f.mustExit(f.run("--mutation", "off"), 1)
	first := f.report()
	if first.Status != "halted" || first.IntegrationSHA == "" {
		t.Fatalf("first run: %s, integration %q", first.Status, first.IntegrationSHA)
	}

	// The halted run is not resumable; gosplan says how to continue it.
	res := f.exec(nil, f.args("--mutation", "off", "--run", first.Run)...)
	if !strings.Contains(res.stderr, "--continue "+first.Run) {
		t.Errorf("resume of a halted run does not point at --continue:\n%s", res.stderr)
	}

	runScenario(t, f.scn, "2.b", map[string][]map[string]any{"*": runCommitSteps("2.b", "ok\n")})
	res = f.run("--mutation", "off", "--continue", first.Run)
	f.mustExit(res, 0)
	if !strings.Contains(res.stderr, "continuing run "+first.Run+": tasks 2.b 3.a") {
		t.Errorf("stderr lacks the continuation line:\n%s", res.stderr)
	}
	second := f.report()
	if second.Run == first.Run || second.Status != "complete" {
		t.Fatalf("second run %s: %s", second.Run, second.Status)
	}
	if len(second.Tasks) != 2 || second.Tasks[0].ID != "2.b" || second.Tasks[1].ID != "3.a" {
		t.Fatalf("second run tasks = %+v, want 2.b and 3.a", second.Tasks)
	}
	if second.BaseSHA != first.IntegrationSHA {
		t.Errorf("second run base %s, want the first run's integration tip %s", second.BaseSHA, first.IntegrationSHA)
	}
	if deps := runTask(t, second, "3.a").Deps; len(deps) != 1 || deps[0] != "2.b" {
		t.Errorf("3.a deps = %v, want [2.b] (2.a and the rest are in the base)", deps)
	}
	tree := git(t, f.project, "ls-tree", "--name-only", second.IntegrationSHA)
	for _, id := range []string{"1.a", "2.a", "2.b", "2.c", "3.a"} {
		if !strings.Contains(tree, "fake-"+id+".txt") {
			t.Errorf("integration tree lacks fake-%s.txt:\n%s", id, tree)
		}
	}
	if b, err := os.ReadFile(filepath.Join(f.state, "runs", second.Run, "subset.json")); err != nil || !strings.Contains(string(b), first.Run) {
		t.Errorf("subset.json: %s %v", b, err)
	}
	// Resuming the ended continuation run re-applies the subset: it is
	// reported, not refused as a changed spec.
	f.mustExit(f.exec(nil, f.args("--mutation", "off", "--run", second.Run)...), 0)

	// --continue on a run with nothing left is an error, as is --only on an unknown task.
	if res := f.exec(nil, f.args("--continue", second.Run)...); res.code != 2 || !strings.Contains(res.stderr, "every task passed") {
		t.Errorf("--continue of a complete run: exit %d\n%s", res.code, res.stderr)
	}
	if res := f.exec(nil, f.args("--on-run", first.Run, "--only", "3.a")...); res.code != 2 || !strings.Contains(res.stderr, "add 2.b to --only") {
		t.Errorf("--only without a failed dep: exit %d\n%s", res.code, res.stderr)
	}
}
