package reconcile

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
)

// usageLimitStep is what claude prints when the plan's five-hour window
// refuses the session: a rejected rate_limit_event, then an error result
// with api_error usage_limit_reached, exit 1, about a second in.
func usageLimitStep(resetsAt int64) step {
	rl, _ := json.Marshal(map[string]any{
		"type": "rate_limit_event",
		"rate_limit_info": map[string]any{
			"status": "rejected", "resetsAt": resetsAt, "rateLimitType": "five_hour",
		},
	})
	res, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": true, "num_turns": 1,
		"api_error_status": 429, "api_error_type": "rate_limit_error", "api_error": "usage_limit_reached",
		"result": "You've hit your 5-hour limit",
	})
	return step{lines: []string{`{"type":"system","subtype":"init"}`, string(rl), string(res)}, code: 1}
}

// TestReconcileUsageLimitRefundsAndParks: a usage-limit refusal does not use
// up an attempt, even with --max-attempts 1, and neither the refused task nor
// the other pending task launches again before the reset.
func TestReconcileUsageLimitRefundsAndParks(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("1.b", 1))
	h.cfg.MaxAttempts = 1
	h.cfg.MaxParallel = 1
	h.cfg.UsageResetSlack = 200 * time.Millisecond
	reset := time.Now().Add(time.Second).Truncate(time.Second).Add(time.Second)
	refused := false
	h.l.script = func(id string, n int) step {
		if !refused {
			refused = true
			return usageLimitStep(reset.Unix())
		}
		return okStep
	}
	if got := h.run(); got != core.RunComplete {
		t.Fatalf("run status = %s, want complete (a usage limit must not fail the task)", got)
	}
	launches, _ := h.l.snapshot()
	if len(launches) != 3 {
		t.Fatalf("launches = %d, want 3 (one refused, then one per task)", len(launches))
	}
	first := launches[0].task
	until := reset.Add(h.cfg.UsageResetSlack)
	for _, l := range launches[1:] {
		if l.at.Before(until) {
			t.Errorf("%s attempt %d launched at %v, before the reset+slack %v", l.task, l.attempt, l.at, until)
		}
	}
	as := h.attempts(first)
	if len(as) != 2 || as[0].Outcome != "exited" || as[1].Outcome != "exited" {
		t.Fatalf("%s attempts = %+v", first, as)
	}
	if got := h.task(first).AttemptsUsed; got != 1 {
		t.Errorf("%s attempts_used = %d, want 1 (the refused attempt is refunded)", first, got)
	}
}

// TestReconcileUsageLimitNoResetBacksOff: a refusal whose stream names no
// future reset waits UsageLimitBackoff, then retries.
func TestReconcileUsageLimitNoResetBacksOff(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MaxAttempts = 1
	h.cfg.UsageLimitBackoff = 400 * time.Millisecond
	h.l.script = func(id string, n int) step {
		if n == 1 {
			return usageLimitStep(time.Now().Add(-time.Hour).Unix())
		}
		return okStep
	}
	if got := h.run(); got != core.RunComplete {
		t.Fatalf("run status = %s, want complete", got)
	}
	launches, _ := h.l.snapshot()
	if len(launches) != 2 {
		t.Fatalf("launches = %d, want 2", len(launches))
	}
	as := h.attempts("1.a")
	if gap := launches[1].at.Sub(as[0].EndedAt); gap < h.cfg.UsageLimitBackoff-50*time.Millisecond {
		t.Errorf("relaunched %v after the refusal, want >= %v", gap, h.cfg.UsageLimitBackoff)
	}
}

// TestReconcileErrorResultIsStillAFailure: an error result that is not a
// usage limit takes the normal retry path and uses up the attempt.
func TestReconcileErrorResultIsStillAFailure(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MaxAttempts = 1
	h.l.script = func(string, int) step {
		res, _ := json.Marshal(map[string]any{
			"type": "result", "subtype": "error_during_execution", "is_error": true, "num_turns": 3,
			"api_error_status": 500, "api_error_type": "api_error", "result": "boom",
		})
		return step{lines: []string{string(res)}, code: 1}
	}
	if got := h.run(); got != core.RunHalted {
		t.Fatalf("run status = %s, want halted", got)
	}
	if tr := h.task("1.a"); tr.Status != core.StatusFailed || !strings.Contains(tr.LastFailure, "attempt 1") {
		t.Errorf("task = %s (%q), want failed", tr.Status, tr.LastFailure)
	}
}
