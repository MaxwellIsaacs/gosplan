// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// status_test.go: RED BY DESIGN. `gosplan status` and report.json.
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keysMissing(m map[string]any, keys ...string) []string {
	var out []string
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// TestStatusJSONSchema: `gosplan status --json` is the v1 report with every documented
// field, and report.json on disk is the same report.
// Clause: orchestrator#DY-STATUS-SCHEMA
func TestStatusJSONSchema(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a", nil)
	mustExit(t, "gosplan run", f.runNR(), 0)
	r, raw := f.status()
	if r.Schema != "gosplan.run-report/v1" {
		t.Fatalf("schema: want gosplan.run-report/v1, got %q", r.Schema)
	}
	if m := keysMissing(raw, "schema", "run", "spec_dir", "project", "base_sha", "status", "started_at", "finished_at",
		"integration_branch", "integration_sha", "tasks", "windows", "notes"); len(m) > 0 {
		t.Fatalf("report lacks top-level fields %v", m)
	}
	tasks, _ := raw["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	for _, x := range tasks {
		tm, _ := x.(map[string]any)
		if m := keysMissing(tm, "id", "tier", "status", "deps", "deps_source", "blocked_reason", "attempts"); len(m) > 0 {
			t.Fatalf("task lacks fields %v", m)
		}
		atts, _ := tm["attempts"].([]any)
		if len(atts) == 0 {
			t.Fatalf("task %v has no attempts", tm["id"])
		}
		for _, a := range atts {
			am, _ := a.(map[string]any)
			if m := keysMissing(am, "n", "model", "outcome", "exit_code", "result_subtype", "num_turns", "duration_ms",
				"cost_usd_estimate", "head_sha", "verdict", "attestation_path", "mutation"); len(m) > 0 {
				t.Fatalf("attempt lacks fields %v", m)
			}
			if _, ok := am["cost_usd"]; ok {
				t.Fatalf("attempt has cost_usd; cost must be labelled cost_usd_estimate")
			}
			vm, _ := am["verdict"].(map[string]any)
			if m := keysMissing(vm, "result", "failed_gates", "tree_sha", "cache_hit"); len(m) > 0 {
				t.Fatalf("verdict lacks fields %v", m)
			}
		}
	}
	joined := strings.ToLower(strings.Join(r.Notes, " "))
	if !strings.Contains(joined, "estimate") {
		t.Fatalf("notes must say cost is an estimate: %v", r.Notes)
	}
	path := filepath.Join(f.state, "runs", r.Run, "report.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report.json missing at %s: %v", path, err)
	}
	var disk report
	if err := json.Unmarshal(b, &disk); err != nil || disk.Schema != r.Schema || disk.Run != r.Run || disk.Status != r.Status {
		t.Fatalf("report.json differs from gosplan status --json: %v %+v", err, disk)
	}
}

// TestStatusText: the human view names the run, its status, every task and
// marks mutation advisory; a state dir without gosplan.db is exit 2.
// Clause: orchestrator#DY-STATUS-TEXT
func TestStatusText(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a", nil)
	mustExit(t, "gosplan run", f.runNR(), 0)
	res := gosplan(t, f.project, f.env(), "status", "--state-dir", f.state)
	if res.code != 0 {
		t.Fatalf("gosplan status: want exit 0, got %s", res)
	}
	run := f.onlyRun()
	for _, want := range []string{run, "complete", "1.a", "2.a", "(advisory)"} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("gosplan status text lacks %q:\n%s", want, res.stdout)
		}
	}
	if res := gosplan(t, f.project, f.env(), "status", "--state-dir", t.TempDir()); res.code != 2 {
		t.Fatalf("gosplan status on a state dir without gosplan.db: want exit 2, got %s", res)
	}
}
