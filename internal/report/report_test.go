package report

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

const run = "20261028-031500-ab12"

var testNow = time.Date(2026, 10, 28, 4, 0, 0, 0, time.UTC)

func mustExec(t *testing.T, s *state.Store, q string, args ...any) {
	t.Helper()
	if _, err := s.DB().Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// seed builds a run with 1.a passed (signed, with mutation), 2.b failed after
// 3 attempts, 3.a abandoned (depends on 2.b), and two window rows.
func seed(t *testing.T, mutation string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gosplan.db")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustExec(t, s, `INSERT INTO specs (dir, content_sha256, imported_at) VALUES ('/spec', 'x', '2026-10-28T03:15:00Z')`)
	mustExec(t, s, `INSERT INTO runs (id, spec_id, project, base_sha, status, started_at, finished_at, integration_branch, integration_sha)
		VALUES (?, 1, '/proj', '81f81606ddb0aaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'halted', '2026-10-28T03:15:00Z', '2026-10-28T05:02:00Z',
		        'gosplan/x/integration', '1c2d3e4f5a6b0000000000000000000000000000')`, run)
	for _, tk := range []struct{ id, status, reason string }{
		{"1.a", "passed", ""}, {"2.b", "failed", "attempts exhausted"}, {"3.a", "abandoned", "dep 2.b failed"},
	} {
		mustExec(t, s, `INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source, blocked_reason)
			VALUES (?, ?, 1, '/p', ?, 'tiers', ?)`, run, tk.id, tk.status, tk.reason)
	}
	mustExec(t, s, `INSERT INTO task_deps VALUES (?, '3.a', '2.b')`, run)
	mustExec(t, s, `INSERT INTO task_deps VALUES (?, '2.b', '1.a')`, run)
	mustExec(t, s, `UPDATE tasks SET attempts_used = 3 WHERE id = '2.b'`)
	mustExec(t, s, `UPDATE tasks SET attempts_used = 1 WHERE id = '1.a'`)
	mustExec(t, s, `INSERT INTO attempts (run_id, task_id, n, session_id, model, started_at, ended_at, outcome, exit_code,
			result_subtype, num_turns, duration_ms, cost_usd_estimate, head_sha, verdict_result, verdict_json,
			failed_gates_json, attestation_path, mutation_json)
		VALUES (?, '1.a', 1, 's', 'opus', '2026-10-28T03:16:00Z', '2026-10-28T03:30:00Z', 'exited', 0, 'success', 12, 840000, 1.25,
		        'abc', 'pass', '{"tree_sha":"tree1","cache_hit":true}', '[]', '/att/1.a.1.dsse.json', ?)`, run, nullable(mutation))
	for n := 1; n <= 3; n++ {
		mustExec(t, s, `INSERT INTO attempts (run_id, task_id, n, session_id, model, started_at, outcome, verdict_result,
				verdict_json, failed_gates_json)
			VALUES (?, '2.b', ?, 's', 'opus', '2026-10-28T03:40:00Z', 'exited', 'fail', '{"tree_sha":"t2"}', '["cmd:test"]')`, run, n)
	}
	f := 0.42
	reset := int64(time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC).Unix())
	g := 0.43
	if err := s.RecordWindows(context.Background(), []state.WindowObservation{
		{Window: "five_hour", Utilization: &f, ResetsAt: &reset, Status: "allowed", Source: "rate_limit_event", Documented: true,
			ObservedAt: testNow.Add(-12 * time.Minute), Raw: "{}"},
		{Window: "session", Utilization: &g, Status: "allowed", Source: "usage_text", Documented: false,
			ObservedAt: testNow.Add(-31 * time.Minute), Raw: "x"},
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func build(t *testing.T, path string) Report {
	t.Helper()
	s, err := state.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := Build(context.Background(), s, run, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func task(t *testing.T, r Report, id string) Task {
	t.Helper()
	for _, tk := range r.Tasks {
		if tk.ID == id {
			return tk
		}
	}
	t.Fatalf("no task %s", id)
	return Task{}
}

func TestReportSchemaAndTasks(t *testing.T) {
	r := build(t, seed(t, `{"test_efficacy":83,"mutants_total":12}`))
	if r.Schema != "gosplan.run-report/v1" || r.Run != run || r.Status != "halted" {
		t.Fatalf("header: %+v", r)
	}
	if len(r.Tasks) != 3 {
		t.Fatalf("tasks = %d", len(r.Tasks))
	}
	for id, want := range map[string]string{"1.a": "passed", "2.b": "failed", "3.a": "abandoned"} {
		if got := task(t, r, id).Status; got != want {
			t.Errorf("%s status = %q, want %q", id, got, want)
		}
	}
	if n := len(task(t, r, "2.b").Attempts); n != 3 {
		t.Errorf("2.b attempts = %d, want 3", n)
	}
	if w := task(t, r, "3.a").DepsWaiting; len(w) != 1 || w[0] != "2.b" {
		t.Errorf("3.a deps_waiting = %v, want [2.b]", w)
	}
	if w := task(t, r, "2.b").DepsWaiting; len(w) != 0 {
		t.Errorf("2.b deps_waiting = %v, want none (1.a passed)", w)
	}
	a := task(t, r, "1.a").Attempts[0]
	if a.Verdict == nil || a.Verdict.Result != "pass" || a.Verdict.TreeSHA != "tree1" || !a.Verdict.CacheHit {
		t.Errorf("1.a verdict = %+v", a.Verdict)
	}
	if a.AttestationPath == "" {
		t.Error("1.a attestation path empty")
	}
	if v := task(t, r, "2.b").Attempts[0].Verdict; v == nil || len(v.FailedGates) != 1 || v.FailedGates[0] != "cmd:test" {
		t.Errorf("2.b verdict = %+v", v)
	}
}

func TestReportLabels(t *testing.T) {
	for _, stored := range []string{`{"test_efficacy":83,"mutants_total":12,"advisory":false}`, `{"test_efficacy":83,"mutants_total":12}`} {
		r := build(t, seed(t, stored))
		m := task(t, r, "1.a").Attempts[0].Mutation
		if m["advisory"] != true {
			t.Errorf("stored %s: advisory = %v, want true", stored, m["advisory"])
		}
	}
	r := build(t, seed(t, `{"test_efficacy":83,"mutants_total":12}`))
	for _, w := range r.Windows {
		if w.Documented != (w.Window == "five_hour") {
			t.Errorf("window %s documented = %v", w.Window, w.Documented)
		}
	}
	text := Text(r, testNow)
	t.Log("\n" + text)
	for _, line := range strings.Split(text, "\n") {
		if has := strings.Contains(line, "(undocumented)"); has != strings.HasPrefix(line, "session") {
			t.Errorf("line %q: (undocumented) = %v", line, has)
		}
	}
	if !strings.Contains(text, "(signed)") || !strings.Contains(text, "83% of 12") || !strings.Contains(text, "MUTATION (advisory)") {
		t.Errorf("text missing signed/mutation labels:\n%s", text)
	}
	b, _ := json.Marshal(r)
	for _, k := range []string{`"cost_usd_estimate"`, `"documented":false`, `"advisory":true`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("json lacks %s", k)
		}
	}
	if strings.Contains(string(b), `"cost_usd"`) {
		t.Error("json has unlabeled cost_usd")
	}
}

func TestReportNotes(t *testing.T) {
	r := build(t, seed(t, ""))
	if len(r.Notes) != 4 {
		t.Fatalf("notes = %d", len(r.Notes))
	}
	joined := strings.Join(r.Notes, "\n")
	for _, want := range []string{"client-side estimate, not billing or plan usage", "undocumented sources", "not an official API",
		"advisory signal about test strength", "holder of the key made the statement"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes lack %q", want)
		}
	}
	if m := task(t, r, "1.a").Attempts[0].Mutation; m != nil {
		t.Errorf("mutation = %v, want nil when none stored", m)
	}
}

func TestReportReadOnly(t *testing.T) {
	path := seed(t, "")
	before, _ := os.Stat(path)
	s, err := state.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Build(context.Background(), s, run, testNow); err != nil {
		t.Fatalf("Build on read-only store: %v", err)
	}
	if _, err := s.DB().Exec(`DELETE FROM runs`); err == nil {
		t.Fatal("store is writable")
	}
	after, _ := os.Stat(path)
	if before.Size() != after.Size() {
		t.Error("db size changed")
	}
}

func TestReportWindowAge(t *testing.T) {
	r := build(t, seed(t, ""))
	want := map[string]int64{"five_hour": 12 * 60, "session": 31 * 60}
	for _, w := range r.Windows {
		if w.AgeSeconds != want[w.Window] {
			t.Errorf("%s age = %d, want %d", w.Window, w.AgeSeconds, want[w.Window])
		}
	}
	if len(r.Windows) != 2 {
		t.Errorf("windows = %d", len(r.Windows))
	}
}

func TestReportWriteRoundTrip(t *testing.T) {
	r := build(t, seed(t, `{"test_efficacy":50}`))
	p := filepath.Join(t.TempDir(), "runs", run, "report.json")
	if err := Write(p, r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil || back.Schema != Schema || len(back.Tasks) != 3 {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
		t.Errorf("temp file left behind: %v", ents)
	}
}
