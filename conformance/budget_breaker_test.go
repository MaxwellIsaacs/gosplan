// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// budget_breaker_test.go: RED BY DESIGN. Breakers and the api_retry stall
// allowance.
package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBudgetRateLimitedHoldsNewStarts: a rejected rate_limit_event opens
// RATE_LIMITED: no new attempt starts until the observed reset, and the worker
// that saw it is not killed.
// Clause: budget#BG-RATELIMITED
func TestBudgetRateLimitedHoldsNewStarts(t *testing.T) {
	f := newRunFix(t, "1.a -> (2.a || 2.b)", nil)
	f.scenario(map[string][]map[string]any{"1.a": {
		{"op": "rate_limit", "window": "five_hour", "status": "rejected", "resets_in": 20},
		{"op": "sleep", "seconds": 2, "heartbeat": true}, {"op": "commit"}}})
	mustExit(t, "gosplan run --budget-config (rejected window)", f.runNR("--budget-config", bgConfig(t, "", "")), 0)
	if st := f.starts("1.a"); len(st) != 1 {
		t.Fatalf("the worker that saw the limit must not be killed or retried: 1.a started %d times", len(st))
	}
	resets := sql(t, f.db(), "SELECT resets_at FROM window_observations WHERE status = 'rejected' ORDER BY id LIMIT 1;")
	if resets == "" {
		t.Fatalf("no rejected observation recorded")
	}
	var reset int64
	for _, c := range resets {
		reset = reset*10 + int64(c-'0')
	}
	spans := bgSpans(f)
	for _, id := range []string{"2.a", "2.b"} {
		s, ok := bgFirstStart(spans, id)
		if !ok {
			t.Fatalf("%s never started", id)
		}
		if s < reset*1000 {
			t.Fatalf("%s started at %d ms while RATE_LIMITED was open (reset %d s)", id, s, reset)
		}
	}
	evs := bgLog(t, f, f.onlyRun())
	opened, closed := false, false
	for _, b := range bgOfType(evs, "budget_breaker") {
		br, _ := b["breaker"].(map[string]any)
		if br["kind"] != "RATE_LIMITED" {
			continue
		}
		opened = opened || b["action"] == "open"
		closed = closed || b["action"] == "close"
	}
	if !opened || !closed {
		t.Fatalf("want RATE_LIMITED open and close events, got open=%v close=%v", opened, closed)
	}
	held := false
	for _, d := range bgDecisions(t, evs, "") {
		held = held || (!d.Decision.Admit && d.Decision.Rule == "breaker" && d.Decision.Breaker == "RATE_LIMITED")
	}
	if !held {
		t.Fatalf("no decision refused with rule breaker / RATE_LIMITED")
	}
	if s := sql(t, f.db(), "SELECT group_concat(state) FROM budget_breakers WHERE kind = 'RATE_LIMITED';"); s != "closed" {
		t.Fatalf("RATE_LIMITED breaker rows after the run: want closed, got %q", s)
	}
}

// TestBudgetAPIRetryIsHeartbeat: a worker that prints api_retry and then
// waits retry_delay_ms in silence (longer than the stall timeout) is not killed
// as stalled. Always on, with or without a budget config.
// Clause: budget#BG-HEARTBEAT
func TestBudgetAPIRetryIsHeartbeat(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a.1": {
		{"op": "api_retry", "attempt": 1, "retry_delay_ms": 5000},
		{"op": "sleep", "seconds": 5}, {"op": "commit"}}})
	mustExit(t, "gosplan run --stall-timeout 3s (api_retry wait 5s)", f.runNR(), 0)
	if st := f.starts("1.a"); len(st) != 1 {
		t.Fatalf("1.a: an api_retry wait was killed as a stall: want 1 attempt, got %d", len(st))
	}
}

// TestBudgetSameFailureBlocks: three attempts with the same failure signature
// (here: no commit) send the task to blocked (a human moment), although
// attempts remain. The breaker is SAME_FAILURE; the reconciler's own
// no_progress halt is a different rule.
// Clause: budget#BG-SAMEFAILURE
func TestBudgetSameFailureBlocks(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a": {{"op": "result", "text": "I did nothing"}}})
	mustExit(t, "gosplan run --budget-config --max-attempts 5 (no commits)", f.runNR("--budget-config", bgConfig(t, "", ""), "--max-attempts", "5"), 1)
	if n := len(f.starts("1.a")); n != 3 {
		t.Fatalf("SAME_FAILURE: want exactly 3 attempts, got %d", n)
	}
	r, _ := f.status()
	tk := r.task(t, "1.a")
	if tk.Status != "blocked" || !strings.Contains(tk.BlockedReason, "SAME_FAILURE") {
		t.Fatalf("1.a: want blocked with SAME_FAILURE, got %s %q", tk.Status, tk.BlockedReason)
	}
}

// TestBudgetRetryStormCooldown: N retries within the window open
// RETRY_STORM; no attempt of the run starts until the cooldown has passed.
// Clause: budget#BG-RETRYSTORM
func TestBudgetRetryStormCooldown(t *testing.T) {
	f := newRunFix(t, "(1.a || 1.b)", nil)
	fail := []map[string]any{{"op": "exit", "code": 3}}
	f.scenario(map[string][]map[string]any{"1.a.1": fail, "1.a.2": fail, "1.b.1": fail, "1.b.2": fail})
	cfg := bgConfig(t, "", "[budget.breaker.retry_storm]\nretries = 2\nwindow = \"1m\"\ncooldown = \"6s\"\n")
	mustExit(t, "gosplan run --budget-config --max-attempts 4 (retry storm)", f.runNR("--budget-config", cfg, "--max-attempts", "4"), 0)
	var openAt int64
	for _, b := range bgOfType(bgLog(t, f, f.onlyRun()), "budget_breaker") {
		br, _ := b["breaker"].(map[string]any)
		if br["kind"] == "RETRY_STORM" && b["action"] == "open" && openAt == 0 {
			openAt = bgParseTS(t, b["_ts"].(string)).UnixMilli()
		}
	}
	if openAt == 0 {
		t.Fatalf("two retries within 1m never opened RETRY_STORM")
	}
	later := 0
	for _, s := range bgSpans(f) {
		if s.start > openAt {
			later++
			if s.start < openAt+6000-500 {
				t.Fatalf("%s.%s started %d ms after RETRY_STORM opened; cooldown is 6s", s.task, s.attempt, s.start-openAt)
			}
		}
	}
	if later == 0 {
		t.Fatalf("no attempt after the storm: the run cannot have completed")
	}
}

// TestBudgetVerifierErrorHalts: K consecutive could-not-verify results across
// the run halt it (nothing is trustworthy); later tasks never start. The rule
// is the reconciler's, always on in every mode: this run has no budget config
// (observe mode), and the gate only logs the breaker.
// Clause: budget#BG-VERIFIERERR
func TestBudgetVerifierErrorHalts(t *testing.T) {
	f := newRunFix(t, "(1.a || 1.b) -> 2.a", nil)
	broken := filepath.Join(t.TempDir(), "gosplan-broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\necho 'verifier unavailable' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustExit(t, "gosplan run --max-verify-errors 2 (verifier always exits 2)", f.runNR("--max-verify-errors", "2", "--gosplan-bin", broken), 1)
	if len(f.starts("2.a")) != 0 {
		t.Fatalf("2.a started after the verifier broke")
	}
	run := f.onlyRun()
	reason := ""
	for _, ev := range readEvents(t, filepath.Join(f.state, "runs", run, "events.jsonl")) {
		if ev.Type == "run_finished" {
			reason = string(ev.Data)
		}
	}
	if !strings.Contains(strings.ToLower(reason), "verifier_error") {
		t.Fatalf("run_finished must name verifier_error, got %s", reason)
	}
	opened := false
	for _, b := range bgOfType(bgLog(t, f, run), "budget_breaker") {
		br, _ := b["breaker"].(map[string]any)
		opened = opened || (br["kind"] == "VERIFIER_ERROR" && b["action"] == "open")
	}
	if !opened {
		t.Fatalf("no VERIFIER_ERROR open event")
	}
}
