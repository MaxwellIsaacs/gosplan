package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// replayFixture writes an M1 events.jsonl (one task_state), its state.json,
// and returns their paths.
func replayFixture(t *testing.T) (dir, logPath, statePath string) {
	t.Helper()
	dir = t.TempDir()
	logPath = filepath.Join(dir, "events.jsonl")
	l, err := events.Open(logPath, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append("1.a", events.TaskState, events.TaskStateData{From: "", To: "passed"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	statePath = filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(`{"1.a":"passed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, logPath, statePath
}

// writeBudgetLog writes a budget.jsonl with one admitted decision made by
// budget.Decide from the inputs it logs.
func writeBudgetLog(t *testing.T, path string) {
	t.Helper()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	c := budget.DefaultConfig()
	c.Mode = budget.ModeEnforce
	c.ColdUnitFraction = 0.05
	u, reset := 0.15, now.Add(3*time.Hour).Unix()
	o := budget.Obs{ID: 1, Pool: "five_hour", Utilization: &u, ResetsAt: &reset, Status: "allowed", Source: "rate_limit_event", ObservedAt: now.Add(-time.Minute)}
	cand := budget.Candidate{Key: budget.Key{Run: "run-1", Task: "1.a", Attempt: 1}, Class: "normal", Models: []string{"sonnet"}, RunStartedAt: now.Add(-time.Hour), Tier: 1}
	s := budget.Snapshot{Now: now, Mode: c.Mode, Config: c, ConfigSHA256: c.SHA256(), Obs: map[string][]budget.Obs{"five_hour": {o}},
		Samples: map[string][]budget.Sample{}, Floors: map[string]budget.Obs{}, Candidates: []budget.Candidate{cand},
		Runs: []budget.RunState{{Run: "run-1", Mode: c.Mode}}, For: cand.Key}
	d := budget.Decide(s)
	l, err := budgetlog.Open(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	app := func(typ string, data any) {
		if _, err := l.Append("1.a", typ, data); err != nil {
			t.Fatal(err)
		}
	}
	app(budgetlog.TypeInput, map[string]any{"kind": "config", "id": s.ConfigSHA256, "row": c})
	app(budgetlog.TypeInput, map[string]any{"kind": "obs", "id": o.ID, "row": o})
	app(budgetlog.TypeDecision, map[string]any{
		"eid": 1, "now": now, "mode": s.Mode, "enforced": true, "attempt": 1, "config_sha256": s.ConfigSHA256,
		"obs_ids": []int64{1}, "span_ids": []int64{}, "sample_ids": []int64{}, "floors": s.Floors,
		"open": []any{}, "recent": []any{}, "breakers": []any{}, "candidates": s.Candidates, "runs": s.Runs,
		"for": s.For, "decision": d, "suppressed": 0,
	})
}

func replayCheck(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return budgetCLICapture(t, func() int { return cmdReplay(args) })
}

// TestBudgetReplayCLIM1Only: with no budget.jsonl, `gosplan replay --check`
// prints exactly the M1 line and nothing about budgets.
func TestBudgetReplayCLIM1Only(t *testing.T) {
	_, logPath, statePath := replayFixture(t)
	code, out, errOut := replayCheck(t, "--log", logPath, "--state", statePath, "--check")
	if code != exitPass || out != "ok: replayed statuses match state\n" || errOut != "" {
		t.Fatalf("code %d stdout %q stderr %q", code, out, errOut)
	}
}

// TestBudgetReplayCLIWithBudgetLog: a budget.jsonl next to --log adds the
// ok line (default discovery), --budget-log names one elsewhere, and a
// tampered decision exits 1 with the drift line.
func TestBudgetReplayCLIWithBudgetLog(t *testing.T) {
	dir, logPath, statePath := replayFixture(t)
	bl := filepath.Join(dir, "budget.jsonl")
	writeBudgetLog(t, bl)
	code, out, _ := replayCheck(t, "--log", logPath, "--state", statePath, "--check")
	if code != exitPass || out != "ok: replayed statuses match state\nok: 1 budget decisions recomputed\n" {
		t.Fatalf("code %d stdout %q", code, out)
	}

	other := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.Rename(bl, other); err != nil {
		t.Fatal(err)
	}
	code, out, _ = replayCheck(t, "--log", logPath, "--state", statePath, "--check")
	if code != exitPass || out != "ok: replayed statuses match state\n" {
		t.Fatalf("budget log moved away: code %d stdout %q", code, out)
	}
	code, out, _ = replayCheck(t, "--log", logPath, "--state", statePath, "--check", "--budget-log", other)
	if code != exitPass || !strings.HasSuffix(out, "ok: 1 budget decisions recomputed\n") {
		t.Fatalf("--budget-log: code %d stdout %q", code, out)
	}

	b, _ := os.ReadFile(other)
	tampered := strings.Replace(string(b), `"admit":true`, `"admit":false`, 1)
	if tampered == string(b) {
		t.Fatal("fixture decision has no admit:true to flip")
	}
	if err := os.WriteFile(other, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = replayCheck(t, "--log", logPath, "--state", statePath, "--check", "--budget-log", other)
	if code != exitFail || !strings.Contains(out, "budget drift: eid 1 (budget_decision): decision.admit") {
		t.Fatalf("tampered: code %d stdout %q", code, out)
	}
}

// TestBudgetReplayCLINoCheckUnchanged: without --check the M1 JSON output
// is not followed by any budget line, even with a budget log present.
func TestBudgetReplayCLINoCheckUnchanged(t *testing.T) {
	dir, logPath, _ := replayFixture(t)
	_, without, _ := replayCheck(t, "--log", logPath)
	writeBudgetLog(t, filepath.Join(dir, "budget.jsonl"))
	code, with, _ := replayCheck(t, "--log", logPath)
	if code != exitPass || with != without || strings.Contains(with, "budget") {
		t.Fatalf("code %d, output changed:\nwithout %q\nwith    %q", code, without, with)
	}
}

// TestBudgetReplayCLIUnreadableBudgetLog: an explicit --budget-log that does
// not exist is an error (exit 2), not a silent pass.
func TestBudgetReplayCLIUnreadableBudgetLog(t *testing.T) {
	_, logPath, statePath := replayFixture(t)
	code, _, errOut := replayCheck(t, "--log", logPath, "--state", statePath, "--check", "--budget-log", "/nonexistent/budget.jsonl")
	if code != exitError || !strings.Contains(errOut, "read budget log") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
}

// TestBudgetCalibrateTextFixture: the text output states each number's n and
// basis, prints PASS/FAIL/n too small, and never calls a ratio the plan's
// formula except to deny it.
func TestBudgetCalibrateTextFixture(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	est, act := 100.0, 90.0
	var settled []budget.SettledRes
	for i := 0; i < 20; i++ {
		a := act
		if i < 3 {
			a = 110
		}
		settled = append(settled, budget.SettledRes{Model: "sonnet", SettledAt: now, EstTokens: &est, ActualTokens: &a})
	}
	cfg := budget.DefaultConfig()
	cfg.EstimateQuantile = 0.9
	cal := budget.Calibrate(budget.CalibrateInput{Now: now, Config: cfg, Settled: settled,
		Fits:      []budget.Fit{{Pool: "five_hour", Basis: "none:too_few_points", N: 2}},
		Decisions: []budget.RecordedDecision{{Now: now, Decision: budget.Decision{Rule: budget.RuleStale}}}})
	out := formatCalibration(cal)
	t.Log("\n" + out)
	for _, want := range []string{
		"overrun 3/20 = 0.150 (expected 0.100)",
		"PASS        overrun_rate sonnet",
		"n too small projection_error",
		"FAIL        refusal_mix",
		"none:too_few_points",
		"not the plan's formula",
		"stale",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

// TestBudgetCalibrateCLIEmptyState: on a fresh schema-2 state dir calibrate
// exits 0, and --json carries the schema name and the calibrate keys.
func TestBudgetCalibrateCLIEmptyState(t *testing.T) {
	dir := t.TempDir()
	s, err := state.Open(filepath.Join(dir, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, out, errOut := budgetCLICapture(t, func() int { return dispatch([]string{"budget", "calibrate", "--state-dir", dir, "--json"}) })
	if code != exitPass {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	for _, k := range []string{`"schema": "gosplan.budget-calibrate/v1"`, `"fits"`, `"estimates"`, `"refusals"`, `"projection_error"`, `"alias_evidence"`, `"segments"`, `"notes"`} {
		if !strings.Contains(out, k) {
			t.Errorf("json lacks %s:\n%s", k, out)
		}
	}
	code, out, _ = budgetCLICapture(t, func() int { return dispatch([]string{"budget", "calibrate", "--state-dir", dir}) })
	if code != exitPass || !strings.Contains(out, "one-week check:") {
		t.Fatalf("text: code %d out %q", code, out)
	}
}
