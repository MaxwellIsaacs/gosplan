package reconcile

import (
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
)

// specHarness is a run of 1.a and 2.a (2.a depends on 1.a, sidecar deps,
// scoped) imported and never started: a resume of it checks the DB's
// static task columns against the spec.
func specHarness(t *testing.T) *harness {
	t.Helper()
	a := task("1.a", 1)
	b := spec.Task{ID: "2.a", Tier: 2, Deps: []string{"1.a"}, DepsSource: "sidecar",
		Scope: []string{"src/**", "tests/**"}, AllowNewProtected: true}
	h := newHarness(t, a, b)
	h.cfg.Resume = true
	return h
}

// Each static column edited while gosplan run is down halts the resume
// state_tampered, naming the task and the field, before anything launches.
func TestReconcileResumeChecksSpecColumns(t *testing.T) {
	cases := []struct {
		name, sql, want string
	}{
		{"tier", `UPDATE tasks SET tier = 1 WHERE id = '2.a'`, `tasks row 2.a: tier db="1" spec="2"`},
		{"prompt_path", `UPDATE tasks SET prompt_path = '/tmp/evil.md' WHERE id = '2.a'`, `tasks row 2.a: prompt_path db="/tmp/evil.md"`},
		{"deps_source", `UPDATE tasks SET deps_source = 'tiers' WHERE id = '2.a'`, `tasks row 2.a: deps_source db="tiers" spec="sidecar"`},
		{"deps removed", `DELETE FROM task_deps WHERE task_id = '2.a'`, `tasks row 2.a: deps db="[]" spec="[1.a]"`},
		{"deps added", `INSERT INTO task_deps (run_id, task_id, dep_id) VALUES ('` + testRun + `', '1.a', '2.a')`, `tasks row 1.a: deps db="[2.a]" spec="[]"`},
		{"scope null", `UPDATE tasks SET scope_json = 'null' WHERE id = '2.a'`, `tasks row 2.a: scope_json db="[]" spec="[src/** tests/**]"`},
		{"scope widened", `UPDATE tasks SET scope_json = '["**"]' WHERE id = '2.a'`, `tasks row 2.a: scope_json db="[**]"`},
		{"allow_new_protected", `UPDATE tasks SET allow_new_protected = 1 WHERE id = '1.a'`, `tasks row 1.a: allow_new_protected db="true" spec="false"`},
		{"extra task", `INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source) VALUES ('` + testRun + `', '9.z', 9, '/x', 'pending', 'tiers')`, `tasks row 9.z: not in the spec`},
		{"run base", `UPDATE runs SET base_sha = 'otherbase'`, `runs row ` + testRun + `: base_sha db="otherbase", events.jsonl run_started="runbase"`},
		{"spec sha", `UPDATE specs SET content_sha256 = 'forged'`, `specs row of run ` + testRun + `: content_sha256 db="forged"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := specHarness(t)
			h.exec(c.sql)
			o := h.runOutcome()
			wantHalt(t, o, ReasonTampered+": ", c.want)
			if n, _ := h.l.snapshot(); len(n) != 0 {
				t.Errorf("%d launches after a forged %s", len(n), c.name)
			}
		})
	}
}

// A task deleted from the DB (with its deps) is named as missing.
func TestReconcileResumeChecksTaskSet(t *testing.T) {
	h := specHarness(t)
	h.exec(`DELETE FROM task_deps WHERE task_id = '2.a'`)
	h.exec(`DELETE FROM tasks WHERE id = '2.a'`)
	wantHalt(t, h.runOutcome(), ReasonTampered+": ", "tasks row 2.a: missing")
}

// The spec dir and specs row forged together still disagree with the
// spec hash run_started recorded in events.jsonl.
func TestReconcileResumeChecksLoggedSpecHash(t *testing.T) {
	h := specHarness(t)
	h.exec(`UPDATE specs SET content_sha256 = 'edited'`)
	sp := *h.cfg.Spec
	sp.ContentSHA256 = "edited"
	h.cfg.Spec = &sp
	wantHalt(t, h.runOutcome(), ReasonTampered+": ", `events.jsonl run_started recorded spec_sha256="test"`)
}

// An unchanged resume passes the checks, and scope '[]' reads the same as
// null (both unrestricted).
func TestReconcileResumeSpecUnchanged(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.exec(`UPDATE tasks SET scope_json = '[]'`)
	h.cfg.Resume = true
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("resume = %+v", o)
	}
}
