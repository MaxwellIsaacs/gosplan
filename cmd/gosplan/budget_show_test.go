package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

var budgetTestNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// budgetSeed builds a v2 state dir with run r1: task 1.a passed, 2.a
// pending, 3.a running; pool five_hour at 97% (resets in 1h), seven_day at
// 30% (resets in 4 days), session past its reset; all from undocumented
// sources. It also holds one recorded decision event for 2.a.
func budgetSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := state.Open(filepath.Join(dir, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ex := func(q string, a ...any) {
		t.Helper()
		if _, err := s.DB().Exec(q, a...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ex(`INSERT INTO specs (dir, content_sha256, imported_at) VALUES ('/spec', 'x', '2026-11-02T08:00:00Z')`)
	ex(`INSERT INTO runs (id, spec_id, project, base_sha, status, started_at) VALUES ('r1', 1, '/proj', 'abc', 'running', '2026-11-02T08:00:00Z')`)
	for _, tk := range [][2]string{{"1.a", "passed"}, {"2.a", "pending"}, {"3.a", "running"}} {
		ex(`INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source) VALUES ('r1', ?, 1, '/p', ?, 'tiers')`, tk[0], tk[1])
	}
	cfg, _ := json.Marshal(budget.DefaultConfig())
	ex(`INSERT INTO budget_runs (run_id, owner, mode, max_parallel, caps_json, config_sha256, config_json, heartbeat_at)
		VALUES ('r1', 'o', 'observe', 2, '{}', 'sha', ?, '2026-11-02T11:59:30Z')`, string(cfg))
	u := func(f float64) *float64 { return &f }
	i := func(t time.Time) *int64 { v := t.Unix(); return &v }
	err = s.RecordWindows(context.Background(), []state.WindowObservation{
		{Window: "five_hour", Utilization: u(0.97), ResetsAt: i(budgetTestNow.Add(time.Hour)), Status: "allowed", Source: "rate_limit_event", Documented: false, ObservedAt: budgetTestNow.Add(-5 * time.Minute), Raw: "{}"},
		{Window: "seven_day", Utilization: u(0.30), ResetsAt: i(budgetTestNow.Add(96 * time.Hour)), Status: "allowed", Source: "rate_limit_event", Documented: false, ObservedAt: budgetTestNow.Add(-5 * time.Minute), Raw: "{}"},
		{Window: "session", Utilization: u(0.50), ResetsAt: i(budgetTestNow.Add(-time.Minute)), Status: "allowed", Source: "usage_text", Documented: false, ObservedAt: budgetTestNow.Add(-5 * time.Minute), Raw: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ex(`INSERT INTO budget_events (run_id, task_id, type, data_json, ts) VALUES ('r1', '2.a', 'budget_decision',
		'{"now":"2026-11-02T11:50:00Z","mode":"observe","enforced":false,"decision":{"admit":false,"rule":"headroom"},"suppressed":2}', '2026-11-02T11:50:00Z')`)
	return dir
}

func budgetRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	old := budgetNow
	budgetNow = func() time.Time { return budgetTestNow }
	defer func() { budgetNow = old }()
	return budgetCLICapture(t, func() int { return dispatch(append([]string{"budget"}, args...)) })
}

func budgetJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	code, out, errs := budgetRun(t, args...)
	if code != exitPass {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	return m
}

func poolByName(t *testing.T, m map[string]any, name string) map[string]any {
	t.Helper()
	for _, p := range m["pools"].([]any) {
		if p := p.(map[string]any); p["pool"] == name {
			return p
		}
	}
	t.Fatalf("no pool %s in %v", name, m["pools"])
	return nil
}

func TestBudgetShowJSON(t *testing.T) {
	dir := budgetSeed(t)
	m := budgetJSON(t, "--state-dir", dir, "--json")
	if m["schema"] != "gosplan.budget/v1" || m["mode"] != "observe" {
		t.Fatalf("schema/mode: %v %v", m["schema"], m["mode"])
	}
	for _, k := range []string{"config_sha256", "policy_defaults", "now", "pools", "reservations", "breakers", "estimates", "fits", "notes"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
	fh := poolByName(t, m, "five_hour")
	if fh["documented"] != false || fh["source"] != "rate_limit_event" {
		t.Errorf("unifiedWindows pool must be documented:false: %v", fh)
	}
	for _, k := range []string{"pool", "level", "observed_at", "age_seconds", "source", "documented", "stale", "stale_reason", "resets_at",
		"rate_per_hour", "rate_basis", "horizon_seconds", "horizon_basis", "projected", "headroom", "open_reserved"} {
		if _, ok := fh[k]; !ok {
			t.Errorf("pool missing key %s", k)
		}
	}
	if fh["horizon_basis"] != "reset" {
		t.Errorf("five_hour resets in 1h: horizon_basis %v", fh["horizon_basis"])
	}
	se := poolByName(t, m, "session")
	if se["stale"] != true || se["stale_reason"] != "past_reset" {
		t.Errorf("session past its reset: %v", se)
	}
	_, text, _ := budgetRun(t, "--state-dir", dir)
	if !strings.Contains(text, "(undocumented)") || !strings.Contains(text, "(stale: past_reset)") {
		t.Errorf("text lacks (undocumented)/(stale: past_reset):\n%s", text)
	}
}

func TestBudgetShowPolicyDefaults(t *testing.T) {
	dir := budgetSeed(t)
	keys := func(m map[string]any) []string {
		var out []string
		for _, k := range m["policy_defaults"].([]any) {
			out = append(out, k.(string))
		}
		return out
	}
	base := keys(budgetJSON(t, "--state-dir", dir, "--json"))
	has := func(ks []string, k string) bool {
		for _, x := range ks {
			if x == k {
				return true
			}
		}
		return false
	}
	if !has(base, "budget.stale_after") || len(base) == 0 {
		t.Fatalf("defaults must list budget.stale_after: %v", base)
	}
	cfg := filepath.Join(t.TempDir(), "b.toml")
	if err := os.WriteFile(cfg, []byte("[budget]\nmode = \"observe\"\nstale_after = \"30m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set := keys(budgetJSON(t, "--state-dir", dir, "--json", "--budget-config", cfg))
	if has(set, "budget.stale_after") || len(set) != len(base)-1 {
		t.Errorf("setting stale_after must remove exactly that key: %d -> %d", len(base), len(set))
	}
	_, text, _ := budgetRun(t, "--state-dir", dir, "--budget-config", cfg)
	last := strings.TrimRight(text, "\n")
	last = last[strings.LastIndex(last, "\n")+1:]
	want := "policy defaults in effect: " + strings.Join(set, ", ")
	if last != want {
		t.Errorf("last line\n got %q\nwant %q", last, want)
	}
}

func TestBudgetShowHorizonBasis(t *testing.T) {
	dir := budgetSeed(t)
	m := budgetJSON(t, "--state-dir", dir, "--json")
	sd := poolByName(t, m, "seven_day")
	if sd["horizon_basis"] != "max_horizon" {
		t.Errorf("reset 4 days away: horizon_basis %v, want max_horizon", sd["horizon_basis"])
	}
	_, text, _ := budgetRun(t, "--state-dir", dir)
	if !strings.Contains(text, "at horizon max_horizon") || !strings.Contains(text, "at horizon reset") || strings.Contains(text, "at reset") {
		t.Errorf("text must name the horizon basis and never say 'at reset' for max_horizon:\n%s", text)
	}
}

func TestBudgetShowV1(t *testing.T) {
	dir := budgetV1StateDir(t)
	code, out, _ := budgetRun(t, "--state-dir", dir)
	if code != exitPass || !strings.Contains(out, "no budget tables (schema version 1)") {
		t.Fatalf("v1: code %d out %q", code, out)
	}
	if code, _, errs := budgetCLICapture(t, func() int { return dispatch([]string{"status", "--state-dir", dir}) }); code != exitError || !strings.Contains(errs, "no runs") {
		t.Errorf("status on a v1 db without runs: code %d stderr %q", code, errs)
	}
}

// budgetEnforceConfig writes an enforce-mode config and returns its path.
func budgetEnforceConfig(t *testing.T) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "enforce.toml")
	if err := os.WriteFile(cfg, []byte("[budget]\nmode = \"enforce\"\n[budget.cold]\nunit_fraction = 0.05\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBudgetExplainObserveWouldAdmit(t *testing.T) {
	dir := budgetSeed(t)
	m := budgetJSON(t, "explain", "2.a", "--state-dir", dir, "--model", "opus", "--json")
	if m["admit"] != false || m["rule"] != "headroom" {
		t.Errorf("observe mode computes the headroom refusal but does not enforce it: %v %v", m["admit"], m["rule"])
	}
	_, text, _ := budgetRun(t, "explain", "2.a", "--state-dir", dir, "--model", "opus")
	if !strings.Contains(text, "would start now (observe mode: rule headroom is recorded, not enforced") {
		t.Errorf("verdict:\n%s", text)
	}
}

func TestBudgetExplainParked(t *testing.T) {
	dir := budgetSeed(t)
	cfg := budgetEnforceConfig(t)
	m := budgetJSON(t, "explain", "2.a", "--state-dir", dir, "--model", "opus", "--budget-config", cfg, "--json")
	if m["schema"] != "gosplan.budget-explain/v1" || m["admit"] != false || m["task"] != "2.a" || m["status"] != "pending" {
		t.Fatalf("explain: %v", m)
	}
	if m["rule"] != "headroom" {
		t.Errorf("rule %v, want headroom (five_hour at 97%%)", m["rule"])
	}
	if m["admit_at"] == nil {
		t.Errorf("admit_at must be the five_hour reset: %v", m)
	}
	short := 0
	for _, p := range m["pools"].([]any) {
		p := p.(map[string]any)
		if p["headroom"].(float64) < p["reserve"].(float64) {
			short++
		}
	}
	if short == 0 {
		t.Errorf("no pool with headroom < reserve: %v", m["pools"])
	}
	ld, _ := m["last_decision"].(map[string]any)
	if ld == nil || ld["suppressed"].(float64) != 2 || ld["decision"].(map[string]any)["rule"] != "headroom" {
		t.Errorf("last_decision from the seeded event: %v", m["last_decision"])
	}
	_, text, _ := budgetRun(t, "explain", "2.a", "--state-dir", dir, "--model", "opus", "--budget-config", cfg)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "parked: headroom until 2026-11-02T13:") {
		t.Errorf("verdict line %q", last)
	}
	// explain never writes an event.
	s, err := state.OpenReadOnly(filepath.Join(dir, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM budget_events`).Scan(&n); err != nil || n != 1 {
		t.Errorf("budget_events count %d (err %v), want the one seeded", n, err)
	}
}

func TestBudgetExplainNotPendingAndMissing(t *testing.T) {
	dir := budgetSeed(t)
	m := budgetJSON(t, "explain", "1.a", "--state-dir", dir, "--json")
	if m["admit"] != false || m["rule"] != "not_pending" || !strings.Contains(m["reason"].(string), "passed") {
		t.Errorf("passed task: %v", m)
	}
	_, text, _ := budgetRun(t, "explain", "1.a", "--state-dir", dir)
	if !strings.Contains(text, "not pending: passed") {
		t.Errorf("text verdict: %q", text)
	}
	code, _, errs := budgetRun(t, "explain", "9.z", "--state-dir", dir)
	if code != exitError || errs == "" {
		t.Errorf("missing task: code %d stderr %q, want exit 2", code, errs)
	}
}
