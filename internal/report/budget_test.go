package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// seedBudget adds a budget_runs row (mode enforce, halted drain), a park,
// and three decision events to the seeded run.
func seedBudget(t *testing.T) string {
	t.Helper()
	path := seed(t, "")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg, _ := json.Marshal(budget.DefaultConfig())
	mustExec(t, s, `INSERT INTO budget_runs (run_id, owner, mode, max_parallel, caps_json, config_sha256, config_json, heartbeat_at, halt, halt_reason)
		VALUES (?, 'o', 'enforce', 2, '{}', 'sha', ?, '2026-10-28T03:59:00Z', 'drain', 'deadline')`, run, string(cfg))
	mustExec(t, s, `INSERT INTO budget_parks (run_id, task_id, rule, reason, parked_at, not_before, decision_eid) VALUES (?, '2.b', 'headroom', 'r', '2026-10-28T03:50:00Z', '2026-10-28T04:10:00Z', 1)`, run)
	for _, ev := range []struct{ task, data string }{
		{"2.b", `{"decision":{"admit":false,"rule":"headroom"},"suppressed":3}`},
		{"3.a", `{"decision":{"admit":false,"rule":"bootstrap"}}`},
		{"1.a", `{"decision":{"admit":true,"rule":"admit"}}`},
	} {
		mustExec(t, s, `INSERT INTO budget_events (run_id, task_id, type, data_json, ts) VALUES (?, ?, 'budget_decision', ?, '2026-10-28T03:50:00Z')`, run, ev.task, ev.data)
	}
	return path
}

func TestBudgetReportBlock(t *testing.T) {
	r := build(t, seedBudget(t))
	b := r.Budget
	if b == nil {
		t.Fatal("report has no budget block")
	}
	if b.Mode != "enforce" {
		t.Errorf("mode %q", b.Mode)
	}
	if b.Decisions.Admitted != 1 || b.Decisions.Refused != 5 || b.Decisions.ByRule["headroom"] != 4 || b.Decisions.ByRule["bootstrap"] != 1 {
		t.Errorf("decisions (suppressed:3 counts 1+3 refusals): %+v", b.Decisions)
	}
	if b.Halt == nil || b.Halt.Reason != "deadline" || len(b.Halt.Parked) != 1 || b.Halt.Parked[0] != "2.b" {
		t.Errorf("halt: %+v", b.Halt)
	}
	if len(b.Notes) != 5 || b.Notes[0] != "Token and USD numbers are estimates." ||
		!strings.Contains(b.Notes[2], "does not cap plan usage") || !strings.Contains(b.Notes[4], "policy") {
		t.Errorf("notes: %q", b.Notes)
	}
	var five *BudgetPool
	for i := range b.Pools {
		if b.Pools[i].Pool == "five_hour" {
			five = &b.Pools[i]
		}
	}
	if five == nil || five.Documented != true || !five.Stale || five.StaleReason != "past_reset" {
		t.Errorf("five_hour pool (seeded documented, past its reset): %+v", five)
	}
	text := Text(r, testNow)
	for _, want := range []string{"budget: enforce; pools ", "open 0; breakers -; halt deadline"} {
		if !strings.Contains(text, want) {
			t.Errorf("status text lacks %q:\n%s", want, text)
		}
	}
}

func TestBudgetReportKeepsOldKeys(t *testing.T) {
	r := build(t, seedBudget(t))
	raw, err := JSON(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema", "run", "spec_dir", "project", "base_sha", "status", "started_at", "finished_at",
		"integration_branch", "integration_sha", "tasks", "windows", "notes", "budget"} {
		if _, ok := m[k]; !ok {
			t.Errorf("report lost key %q", k)
		}
	}
	if string(m["schema"]) != `"gosplan.run-report/v1"` {
		t.Errorf("schema %s", m["schema"])
	}
	if len(r.Notes) != len(Notes) {
		t.Errorf("top-level notes changed: %q", r.Notes)
	}
	// A report without a budget block (v1 database) has no budget key.
	r.Budget = nil
	raw, _ = JSON(r)
	if strings.Contains(string(raw), `"budget"`) {
		t.Errorf("budget key must be omitted when nil")
	}
}

func TestBudgetReportV1DBHasNoBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gosplan.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1; CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := state.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, err := BuildBudget(context.Background(), s, run, testNow)
	if err != nil || b != nil {
		t.Fatalf("v1 db: %v, %v; want nil, nil", b, err)
	}
}

func TestBudgetReportNoBudgetRunIsOff(t *testing.T) {
	r := build(t, seed(t, ""))
	if r.Budget == nil || r.Budget.Mode != "off" || r.Budget.Halt != nil || r.Budget.Decisions.Refused != 0 {
		t.Errorf("run without budget_runs row: %+v", r.Budget)
	}
}
