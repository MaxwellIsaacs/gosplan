// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// budget_caps_test.go: RED BY DESIGN. Estimates, belts, per-run caps and the
// deadline.
package conformance

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBudgetEstimateFromHistory: the token estimate is the nearest-rank p90
// of prior attempts of the same model across runs, with n and basis; fewer
// samples than min_samples is cold.
// Clause: budget#BG-EST
func TestBudgetEstimateFromHistory(t *testing.T) {
	f := newRunFix(t, "(1.a || 1.b || 1.c || 1.d || 1.e || 1.f)", nil)
	sc := map[string][]map[string]any{}
	for i, id := range []string{"1.a", "1.b", "1.c", "1.d", "1.e", "1.f"} {
		sc[id] = []map[string]any{{"op": "usage", "input": 100 * (i + 1), "output": 0}, {"op": "commit"}}
	}
	f.scenario(sc)
	mustExit(t, "gosplan run (observe, six samples)", f.runNR(), 0)
	check := func(args []string, wantBasis string, wantTokens float64) {
		m := bgJSON(t, f, "gosplan budget --json", append([]string{"budget", "--state-dir", f.state, "--json"}, args...)...)
		for _, e := range bgList(m, "estimates") {
			if e["model"] != "sonnet" {
				continue
			}
			basis, _ := e["basis"].(string)
			n, _ := bgNum(e, "n")
			q, _ := bgNum(e, "quantile")
			if n != 6 || q != 0.9 || !strings.HasPrefix(basis, wantBasis) {
				t.Fatalf("sonnet estimate: want n=6 quantile 0.9 basis %s*, got %v", wantBasis, e)
			}
			if wantTokens > 0 {
				if tok, _ := bgNum(e, "tokens"); tok != wantTokens {
					t.Fatalf("sonnet estimate tokens: want nearest-rank p90 %v, got %v", wantTokens, e["tokens"])
				}
			}
			return
		}
		t.Fatalf("gosplan budget --json has no sonnet estimate: %v", m["estimates"])
	}
	check(nil, "history", 600)
	cold := bgConfig(t, "", "[budget.estimate]\nmin_samples = 7\n")
	check([]string{"--budget-config", cold}, "cold", 0)
}

// TestBudgetBeltMaxBudgetUSD: in enforce mode with cost history the worker
// gets --max-budget-usd = p90 cost estimate x factor (rounded up to cents);
// observe mode passes none; the help text says it does not cap plan usage.
// Clause: budget#BG-BELT
// Clause: budget#BG-CLAIMS
func TestBudgetBeltMaxBudgetUSD(t *testing.T) {
	f := newRunFix(t, "(1.a || 1.b || 1.c || 1.d || 1.e)", nil)
	sc := map[string][]map[string]any{}
	for i, id := range []string{"1.a", "1.b", "1.c", "1.d", "1.e"} {
		sc[id] = []map[string]any{{"op": "usage", "cost": 0.1 * float64(i+1)}, {"op": "commit"}}
	}
	f.scenario(sc)
	mustExit(t, "gosplan run (observe, cost history)", f.runNR(), 0)
	for _, s := range f.starts("") {
		if argvHas(s.Argv, "--max-budget-usd", "") {
			t.Fatalf("observe mode must not pass --max-budget-usd: %v", s.Argv)
		}
	}
	g := newRunFix(t, "1.g", nil)
	cfg := bgConfig(t, "", "[budget.estimate]\nmin_samples = 5\n[budget.belt]\nusd_factor = 2.0\n")
	args := []string{nrBin, "run", "--spec", g.spec, "--project", g.project, "--state-dir", f.state,
		"--claude", fakeClaude, "--gosplan-bin", nrBin, "--max-parallel", "3", "--max-attempts", "2",
		"--stall-timeout", "3s", "--retry-base", "1s", "--mutation", "off", "--usage-probe=false",
		"--attest-key", filepath.Join(f.state, "no-such-key"), "--budget-config", cfg}
	mustExit(t, "gosplan run (enforce, history)", run(t, 3*time.Minute, g.project, g.env(), args...), 0)
	st := g.starts("1.g")
	if len(st) == 0 || !argvHas(st[0].Argv, "--max-budget-usd", "1.00") {
		t.Fatalf("want --max-budget-usd 1.00 (p90 0.50 x 2.0), argv %v", st)
	}
	help := gosplan(t, f.project, nil, "run", "-h")
	bgContainsAll(t, "gosplan run -h", help.stdout+help.stderr, "max-budget-usd", "estimate", "does not cap plan usage")
}

// TestBudgetCapUSDDrains: --cap-usd-estimate halts the run cleanly (drain)
// once settled cost estimates reach it; later tasks stay pending.
// Clause: budget#BG-CAPS
func TestBudgetCapUSDDrains(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a -> 3.a", nil)
	mustExit(t, "gosplan run --cap-usd-estimate 0.0015", f.runNR("--cap-usd-estimate", "0.0015"), 1)
	if len(f.starts("3.a")) != 0 {
		t.Fatalf("3.a started after the USD estimate cap was reached")
	}
	r, raw := f.status()
	if s := r.task(t, "3.a").Status; s != "pending" {
		t.Fatalf("3.a after a cap halt: want pending, got %s", s)
	}
	b, _ := raw["budget"].(map[string]any)
	h, _ := b["halt"].(map[string]any)
	if h == nil || !strings.Contains(fmt.Sprint(h["reason"]), "cap:usd_estimate") {
		t.Fatalf("report budget.halt: want reason cap:usd_estimate, got %v", b["halt"])
	}
}

// TestBudgetDeadlineStops: at --deadline the run stops (live workers
// killed), ends halted with reason deadline and lists the parked tasks; a
// deadline already past starts nothing.
// Clause: budget#BG-DEADLINE
func TestBudgetDeadlineStops(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a", nil)
	f.scenario(map[string][]map[string]any{"1.a": {{"op": "sleep", "seconds": 40, "heartbeat": true}, {"op": "commit"}}})
	t0 := time.Now()
	deadline := t0.Add(6 * time.Second).UTC().Format(time.RFC3339)
	mustExit(t, "gosplan run --deadline +6s", f.runNR("--deadline", deadline), 1)
	if el := time.Since(t0); el > 30*time.Second {
		t.Fatalf("the run went on %s past a 6s deadline", el)
	}
	r, raw := f.status()
	if s := r.task(t, "2.a").Status; s != "pending" {
		t.Fatalf("2.a after a deadline halt: want pending (parked), got %s", s)
	}
	b, _ := raw["budget"].(map[string]any)
	h, _ := b["halt"].(map[string]any)
	if h == nil || !strings.HasPrefix(fmt.Sprint(h["reason"]), "deadline") || !strings.Contains(fmt.Sprint(h["parked"]), "2.a") {
		t.Fatalf("report budget.halt: want reason deadline with 2.a parked, got %v", b["halt"])
	}

	g := newRunFix(t, "1.a", nil)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	mustExit(t, "gosplan run --deadline (past)", g.runNR("--deadline", past), 1)
	if n := len(g.starts("")); n != 0 {
		t.Fatalf("a past deadline must start nothing, %d workers started", n)
	}
}
