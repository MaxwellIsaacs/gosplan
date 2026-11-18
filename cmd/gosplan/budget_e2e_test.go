package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// TestBudgetE2EAssembly is the budget gate end-to-end check:
// the e2e fixture (1.a -> (2.a || 2.b || 2.c) -> 3.a, 2.c without deps) runs
// with an enforce-mode budget.toml (cold.unit_fraction 0.05 is this
// test's own policy value: enforce has no default for it). 1.a reports
// five_hour at 0.97 resetting in a few seconds, so the 2.x tasks are refused
// for headroom and parked. While a task is parked, `gosplan budget explain` says
// so; after the reset every task is admitted and the run completes; then
// `gosplan replay --check` recomputes every budget decision and
// `gosplan budget calibrate --json` parses.
func TestBudgetE2EAssembly(t *testing.T) {
	const resetIn = 10 // seconds
	f := runNewFix(t, "e2e-spec")
	f.scenario("1.a", []map[string]any{
		{"op": "rate_limit", "window": "five_hour", "utilization": 0.97, "resets_in": resetIn},
		{"op": "commit", "file": "fake-1.a.txt", "content": "ok\n"}})
	// The budget.toml lives outside the state dir, so `gosplan budget explain`
	// (given no --budget-config) must decide with the run's recorded mode
	// and config, not with <state>/budget.toml (absent: observe defaults).
	cfg := bgCfg(t, "stale_after = \"10m\"\n", "")

	// gosplan run in the background.
	cmd := exec.Command(f.bins.gosplan, f.args("--usage-probe=false", "--mutation", "off", "--budget-config", cfg)...)
	cmd.Dir = f.project
	cmd.Env = append(os.Environ(), "CONF_FAKE_DIR="+f.scn)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logf, err := os.Create(filepath.Join(t.TempDir(), "gosplan-run.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	runLog := func() string { b, _ := os.ReadFile(logf.Name()); return string(b) }

	// The park: a budget_parks row for a 2.x task, with rule headroom.
	parked := ""
	deadline := time.Now().Add(60 * time.Second)
	for parked == "" && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("gosplan run ended (%v) before any task parked:\n%s", err, runLog())
		case <-time.After(100 * time.Millisecond):
		}
		parked = bgParkedTask(f.state, "headroom")
	}
	if parked == "" {
		t.Fatalf("no task parked for headroom within 60s:\n%s", runLog())
	}

	// gosplan budget explain during the park.
	ex := f.exec(nil, "budget", "explain", parked, "--state-dir", f.state, "--json")
	if ex.code != 0 {
		t.Fatalf("gosplan budget explain %s: exit %d\n%s%s", parked, ex.code, ex.stdout, ex.stderr)
	}
	var e map[string]any
	if err := json.Unmarshal([]byte(ex.stdout), &e); err != nil {
		t.Fatalf("explain --json: %v\n%s", err, ex.stdout)
	}
	if e["schema"] != "gosplan.budget-explain/v1" || e["admit"] != false || e["rule"] != "headroom" || e["admit_at"] == nil || e["last_decision"] == nil {
		t.Fatalf("explain %s during the park: want admit false, rule headroom, admit_at and last_decision; got %v", parked, e)
	}
	if txt := f.exec(nil, "budget", "explain", parked, "--state-dir", f.state); txt.code != 0 || !strings.Contains(txt.stdout, "parked: headroom until") {
		t.Fatalf("explain text during the park: want 'parked: headroom until', got exit %d\n%s", txt.code, txt.stdout)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gosplan run: %v\n%s", err, runLog())
		}
	case <-time.After(3 * time.Minute):
		t.Fatalf("gosplan run did not finish:\n%s", runLog())
	}
	r := f.report()
	if r.Status != "complete" {
		t.Fatalf("report status %q:\n%s", r.Status, runLog())
	}
	for _, x := range r.Tasks {
		if x.Status != "passed" {
			t.Fatalf("task %s: want passed, got %s", x.ID, x.Status)
		}
	}

	// The admit after the reset: the parked task has an enforced headroom
	// refusal, its last decision is an admit, and it started only after
	// the observed reset.
	evs := bgEvents(t, f)
	ds := bgDecisions(t, evs, parked)
	if len(ds) < 2 {
		t.Fatalf("%s: want a refusal and a later admit, got %d decisions", parked, len(ds))
	}
	var first *bgDec
	for i := range ds {
		if !ds[i].Decision.Admit && ds[i].Decision.Rule == "headroom" {
			first = &ds[i]
			break
		}
	}
	last := ds[len(ds)-1]
	if first == nil || !first.Enforced {
		t.Fatalf("%s: want an enforced headroom refusal among its decisions, got %+v", parked, ds)
	}
	p, ok := bgPool(*first, "five_hour")
	if !ok || p["level"] != 0.97 {
		t.Fatalf("%s refusal: five_hour level want 0.97, got %v", parked, p)
	}
	reset := int64(p["resets_at"].(float64))
	if !last.Decision.Admit {
		t.Fatalf("%s was never admitted: %+v", parked, last.Decision)
	}
	st := bgStarts(bgSpans(t, f), parked)
	if len(st) != 1 || st[0].start < reset*1000 {
		t.Fatalf("%s must start once, after the reset at %d s: %+v", parked, reset, st)
	}
	if n := f.sql1(`SELECT count(*) FROM budget_parks`); n != "0" {
		t.Fatalf("budget_parks after a complete run: want 0 rows, got %s", n)
	}

	// gosplan replay --check recomputes every budget decision.
	rd := f.runDir()
	rp := f.exec(nil, "replay", "--log", filepath.Join(rd, "events.jsonl"), "--state", filepath.Join(rd, "state.json"), "--check")
	if rp.code != 0 || !strings.Contains(rp.stdout, "budget decisions recomputed") {
		t.Fatalf("gosplan replay --check: want exit 0 and 'budget decisions recomputed', got exit %d\n%s%s", rp.code, rp.stdout, rp.stderr)
	}

	// gosplan budget calibrate --json parses.
	cal := f.exec(nil, "budget", "calibrate", "--state-dir", f.state, "--json")
	var c map[string]any
	if cal.code != 0 || json.Unmarshal([]byte(cal.stdout), &c) != nil || c["schema"] != "gosplan.budget-calibrate/v1" {
		t.Fatalf("gosplan budget calibrate --json: want exit 0 and schema gosplan.budget-calibrate/v1, got exit %d\n%s%s", cal.code, cal.stdout, cal.stderr)
	}
	for _, k := range []string{"estimates", "refusals", "projection_error", "notes"} {
		if _, ok := c[k]; !ok {
			t.Fatalf("calibrate --json lacks %q: %v", k, c)
		}
	}
	if ref, _ := c["refusals"].(map[string]any); ref["headroom"] == nil {
		t.Fatalf("calibrate refusals: want the headroom refusals counted, got %v", c["refusals"])
	}
}

// bgParkedTask returns a task with a budget_parks row of rule in the state
// dir's gosplan.db ("" when none yet or the DB cannot be opened read-only).
func bgParkedTask(stateDir, rule string) string {
	p := filepath.Join(stateDir, "gosplan.db")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	s, err := state.OpenReadOnly(p)
	if err != nil {
		return ""
	}
	defer s.Close()
	var task string
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s.DB().QueryRowContext(ctx, `SELECT task_id FROM budget_parks WHERE rule = ? ORDER BY task_id LIMIT 1`, rule).Scan(&task) != nil {
		return ""
	}
	return task
}

// TestBudgetE2EBootstrapReservationCountsInNewPool: 1.a is admitted under
// the bootstrap rule (no pool observed yet), so its reservation row has no
// pool entry; its worker then reports five_hour. While 1.a runs, 1.b's
// decision must count 1.a's reservation as one cold unit (0.05) in
// five_hour: open_reserved 0.05, not 0.
func TestBudgetE2EBootstrapReservationCountsInNewPool(t *testing.T) {
	f := bgFix(t, "(1.a || 1.b)", nil)
	f.scenario("1.a", []map[string]any{{"op": "rate_limit", "window": "five_hour", "utilization": 0.50, "resets_in": 3600},
		{"op": "sleep", "seconds": 3, "heartbeat": true}, {"op": "commit"}})
	f.mustExit(f.brun("--budget-config", bgCfg(t, "", "")), 0)
	evs := bgEvents(t, f)
	if n := f.sql1(`SELECT count(*) FROM budget_reservations WHERE task_id = '1.a' AND pools_json = '[]'`); n != "1" {
		t.Fatalf("1.a's bootstrap reservation must have no pool entry in its row, got %s such rows", n)
	}
	var got *bgDec
	for _, d := range bgDecisions(t, evs, "1.b") {
		if _, ok := bgPool(d, "five_hour"); ok && len(d.Open) > 0 {
			d := d
			got = &d
			break
		}
	}
	if got == nil {
		t.Fatalf("no 1.b decision saw five_hour while 1.a's reservation was open: %+v", bgDecisions(t, evs, "1.b"))
	}
	p, _ := bgPool(*got, "five_hour")
	if p["open_reserved"] != 0.05 {
		t.Fatalf("1.b decision: five_hour open_reserved want 0.05 (1.a's unit), got %v (rule %s: %s)", p["open_reserved"], got.Decision.Rule, got.Decision.Reason)
	}
	bgReplay(t, evs)
}

// TestBudgetE2EDeadlineReportParked: a deadline halt while 1.a runs leaves
// 2.a pending without ever having been refused (no budget_parks row); the
// report's budget.halt still lists it as parked.
func TestBudgetE2EDeadlineReportParked(t *testing.T) {
	f := bgFix(t, "1.a -> 2.a", nil)
	f.scenario("1.a", []map[string]any{{"op": "sleep", "seconds": 40, "heartbeat": true}, {"op": "commit"}})
	f.mustExit(f.brun("--deadline", time.Now().Add(4*time.Second).UTC().Format(time.RFC3339)), 1)
	if n := f.sql1(`SELECT count(*) FROM budget_parks WHERE task_id = '2.a'`); n != "0" {
		t.Fatalf("2.a was never refused, want no budget_parks row, got %s", n)
	}
	r := f.report()
	if r.Budget == nil || r.Budget.Halt == nil {
		t.Fatalf("report budget.halt missing: %+v", r.Budget)
	}
	if h := r.Budget.Halt; h.Reason != "deadline" || strings.Join(h.Parked, ",") != "1.a,2.a" {
		t.Fatalf("report budget.halt: want reason deadline, parked [1.a 2.a], got %+v", *h)
	}
}

// TestBudgetE2EFitLoggedAfterConfig: with the /usage probe on, observations
// exist before the first decision, so the first decision's budget_fit line
// comes first in budget.jsonl. Replay recomputes a fit with the newest config
// line before it, so the config budget_input must precede every budget_fit,
// and `gosplan replay --check` must pass (observe mode, no config file).
func TestBudgetE2EFitLoggedAfterConfig(t *testing.T) {
	f := bgFix(t, "1.a", nil)
	f.mustExit(f.run("--mutation", "off"), 0)
	evs := bgEvents(t, f)
	cfgAt, fitAt := -1, -1
	for i, ev := range evs {
		var d struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if cfgAt < 0 && ev.Type == "budget_input" && d.Kind == "config" {
			cfgAt = i
		}
		if fitAt < 0 && ev.Type == "budget_fit" {
			fitAt = i
		}
	}
	if fitAt < 0 {
		t.Fatalf("no budget_fit line: the probe's observations should give the first decision a fit")
	}
	if cfgAt < 0 || cfgAt > fitAt {
		t.Fatalf("budget_fit at line %d precedes the config budget_input (line %d)", fitAt+1, cfgAt+1)
	}
	rd := f.runDir()
	rp := f.exec(nil, "replay", "--log", filepath.Join(rd, "events.jsonl"), "--state", filepath.Join(rd, "state.json"), "--check")
	if rp.code != 0 || !strings.Contains(rp.stdout, "budget decisions recomputed") {
		t.Fatalf("gosplan replay --check: exit %d\n%s%s", rp.code, rp.stdout, rp.stderr)
	}
}
