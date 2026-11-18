package budget

import (
	"testing"
	"time"
)

var calNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func calF(x float64) *float64 { return &x }
func calI(x int64) *int64     { return &x }

func calCfg() Config {
	c := DefaultConfig()
	c.EstimateQuantile = 0.9
	return c
}

func calSettled(model string, n, over int) []SettledRes {
	var out []SettledRes
	for i := 0; i < n; i++ {
		act := 90.0
		if i < over {
			act = 110
		}
		out = append(out, SettledRes{ID: int64(i + 1), Model: model, SettledAt: calNow.Add(-time.Hour),
			EstTokens: calF(100), ActualTokens: calF(act)})
	}
	return out
}

// TestBudgetCalibrateOverrun: 3 of 20 overrun at q = 0.9 is 0.15 against an
// expected 0.10, and the check passes (within ±0.05); 4 of 20 (0.20) fails;
// n < 20 is "n too small"; an equal actual is not an overrun.
func TestBudgetCalibrateOverrun(t *testing.T) {
	find := func(c Calibration, model string) EstimateCal {
		for _, e := range c.Estimates {
			if e.Model == model {
				return e
			}
		}
		t.Fatalf("no estimate for %s", model)
		return EstimateCal{}
	}
	cal := Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Settled: calSettled("sonnet", 20, 3)})
	e := find(cal, "sonnet")
	if e.OverrunN != 3 || e.SettledN != 20 || e.OverrunRate != 0.15 {
		t.Fatalf("overrun %d/%d = %v, want 3/20 = 0.15", e.OverrunN, e.SettledN, e.OverrunRate)
	}
	if d := e.ExpectedOverrunRate - 0.10; d > 1e-9 || d < -1e-9 {
		t.Fatalf("expected overrun %v, want 0.10", e.ExpectedOverrunRate)
	}
	if got := checkStatus(cal, "overrun_rate sonnet"); got != CheckPass {
		t.Errorf("0.15 vs 0.10: %s, want PASS", got)
	}
	cal = Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Settled: calSettled("sonnet", 20, 4)})
	if got := checkStatus(cal, "overrun_rate sonnet"); got != CheckFail {
		t.Errorf("0.20 vs 0.10: %s, want FAIL", got)
	}
	cal = Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Settled: calSettled("sonnet", 19, 2)})
	if got := checkStatus(cal, "overrun_rate sonnet"); got != CheckTooSmall {
		t.Errorf("n=19: %s, want n too small", got)
	}
	eq := calSettled("opus", 20, 0)
	for i := range eq {
		eq[i].ActualTokens = calF(100) // equal to the estimate: not an overrun
	}
	if e := find(Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Settled: eq}), "opus"); e.OverrunN != 0 {
		t.Errorf("actual == estimate counted as overrun: %d", e.OverrunN)
	}
	unk := calSettled("haiku", 20, 20)
	for i := range unk {
		unk[i].ActualTokens = nil // unknown actuals are not compared
	}
	if e := find(Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Settled: unk}), "haiku"); e.SettledN != 0 {
		t.Errorf("unknown actual counted: settled_n %d", e.SettledN)
	}
}

func checkStatus(c Calibration, name string) string {
	for _, k := range c.Checks {
		if k.Name == name {
			return k.Status
		}
	}
	return "missing"
}

// TestBudgetCalibrateEstimatePercentiles: p50/p90/p99 come from the model's
// samples only.
func TestBudgetCalibrateEstimatePercentiles(t *testing.T) {
	var ss []Sample
	for i := 1; i <= 100; i++ {
		ss = append(ss, Sample{AttemptID: int64(i), Model: "sonnet", Usage: Tokens{Input: float64(i)}})
	}
	cal := Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Samples: map[string][]Sample{"sonnet": ss}})
	e := cal.Estimates[0]
	if e.N != 100 || e.P50 != 50 || e.P90 != 90 || e.P99 != 99 {
		t.Fatalf("n=%d p50=%v p90=%v p99=%v", e.N, e.P50, e.P90, e.P99)
	}
}

func calDecision(eid int64, pool string, projected float64, reset int64, basis string) RecordedDecision {
	return RecordedDecision{EID: eid, Now: calNow.Add(-2 * time.Hour), Decision: Decision{
		Rule: RuleAdmit, Admit: true,
		Pools: []PoolResult{{PoolView: PoolView{Pool: pool, Projected: projected, ResetsAt: calI(reset), HorizonBasis: basis, HorizonSeconds: 3600}}},
	}}
}

func calObs(id int64, pool string, u float64, at time.Time, reset int64) Obs {
	return Obs{ID: id, Pool: pool, Utilization: calF(u), ResetsAt: calI(reset), ObservedAt: at, Status: "allowed"}
}

// TestBudgetCalibrateProjectionError: over-projection is positive, an
// under-projection negative; the observation must be inside
// max(10m, 5% of horizon) before the reset, in the same window, after the
// decision, and decisions with a max_horizon basis or a future reset are
// not compared.
func TestBudgetCalibrateProjectionError(t *testing.T) {
	reset := calNow.Add(-time.Hour).Unix()
	rt := time.Unix(reset, 0).UTC()
	obs := map[string][]Obs{"five_hour": {
		calObs(1, "five_hour", 0.30, rt.Add(-30*time.Minute), reset),     // too early: outside 10 min
		calObs(2, "five_hour", 0.50, rt.Add(-5*time.Minute), reset),      // the last one inside
		calObs(3, "five_hour", 0.90, rt.Add(-3*time.Minute), reset+7200), // other window: ignored
	}}
	decs := []RecordedDecision{
		calDecision(10, "five_hour", 0.60, reset, "reset"),                        // +0.10
		calDecision(11, "five_hour", 0.40, reset, "reset"),                        // −0.10
		calDecision(12, "five_hour", 0.90, reset, "max_horizon"),                  // skipped
		calDecision(13, "five_hour", 0.90, calNow.Add(time.Hour).Unix(), "reset"), // reset not passed
	}
	cal := Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Decisions: decs, Obs: obs})
	pe := cal.ProjectionError
	if pe.N != 2 {
		t.Fatalf("n=%d, want 2: %+v", pe.N, pe.Items)
	}
	if pe.Items[0].Predicted != 0.60 || pe.Items[0].Observed != 0.50 || pe.Items[0].DecisionEID != 10 {
		t.Errorf("item 0: %+v", pe.Items[0])
	}
	if pe.Mean > 1e-9 || pe.Mean < -1e-9 || pe.MeanAbs < 0.0999 || pe.MeanAbs > 0.1001 {
		t.Errorf("mean %v mean_abs %v, want 0 and 0.1", pe.Mean, pe.MeanAbs)
	}
	over := Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Decisions: decs[:1], Obs: obs}).ProjectionError
	if over.Mean < 0.0999 || over.Mean > 0.1001 {
		t.Errorf("over-projection mean %v, want +0.10", over.Mean)
	}
	under := Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Decisions: decs[1:2], Obs: obs}).ProjectionError
	if under.Mean > -0.0999 {
		t.Errorf("under-projection mean %v, want −0.10", under.Mean)
	}
}

// TestBudgetCalibrateAliasPairs: only rows of different pools within 10 min
// pair; the mean absolute difference is over those pairs.
func TestBudgetCalibrateAliasPairs(t *testing.T) {
	obs := map[string][]Obs{
		"five_hour": {calObs(1, "five_hour", 0.50, calNow, 0), calObs(2, "five_hour", 0.60, calNow.Add(time.Hour), 0)},
		"session": {
			calObs(3, "session", 0.46, calNow.Add(9*time.Minute), 0),            // pairs with row 1: 0.04
			calObs(4, "session", 0.90, calNow.Add(time.Hour+11*time.Minute), 0), // 11 min from row 2: no pair
		},
	}
	cal := Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Obs: obs})
	if len(cal.AliasEvidence) != 1 {
		t.Fatalf("alias evidence %+v", cal.AliasEvidence)
	}
	a := cal.AliasEvidence[0]
	if a.A != "five_hour" || a.B != "session" || a.Pairs != 1 || a.MeanAbsDiff < 0.0399 || a.MeanAbsDiff > 0.0401 {
		t.Errorf("alias %+v, want five_hour~session 1 pair 0.04", a)
	}
	none := Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Obs: map[string][]Obs{
		"a": {calObs(1, "a", 0.1, calNow, 0)}, "b": {calObs(2, "b", 0.1, calNow.Add(11*time.Minute), 0)}}})
	if len(none.AliasEvidence) != 0 {
		t.Errorf("rows 11 min apart paired: %+v", none.AliasEvidence)
	}
}

// TestBudgetCalibrateRefusalsByRule: refusals are counted per rule, admits
// are not, and a stale-dominated mix fails the refusal check naming it.
func TestBudgetCalibrateRefusalsByRule(t *testing.T) {
	mk := func(rule string, admit bool) RecordedDecision {
		return RecordedDecision{Now: calNow, Decision: Decision{Rule: rule, Admit: admit}}
	}
	decs := []RecordedDecision{mk(RuleHeadroom, false), mk(RuleHeadroom, false), mk(RuleStale, false), mk(RuleAdmit, true), mk(RuleStale, false), mk(RuleStale, false)}
	cal := Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Decisions: decs})
	if cal.Refusals[RuleHeadroom] != 2 || cal.Refusals[RuleStale] != 3 || len(cal.Refusals) != 2 {
		t.Fatalf("refusals %v", cal.Refusals)
	}
	if got := checkStatus(cal, "refusal_mix"); got != CheckFail {
		t.Errorf("stale-dominated mix: %s, want FAIL", got)
	}
	cal = Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Decisions: decs[:2]})
	if got := checkStatus(cal, "refusal_mix"); got != CheckPass {
		t.Errorf("headroom-only mix: %s, want PASS", got)
	}
}

// TestBudgetCalibrateSinceFilter: data older than Since is ignored.
func TestBudgetCalibrateSinceFilter(t *testing.T) {
	old := calSettled("sonnet", 20, 20)
	for i := range old {
		old[i].SettledAt = calNow.Add(-48 * time.Hour)
	}
	cal := Calibrate(CalibrateInput{Now: calNow, Since: calNow.Add(-24 * time.Hour), Config: calCfg(), Settled: old,
		Decisions: []RecordedDecision{{Now: calNow.Add(-48 * time.Hour), Decision: Decision{Rule: RuleStale}}}})
	if cal.Estimates[0].SettledN != 0 || len(cal.Refusals) != 0 || cal.Since == nil {
		t.Errorf("old data counted: %+v %v", cal.Estimates[0], cal.Refusals)
	}
}

// TestBudgetCalibrateSegmentRules: each split names the Segments rule that
// made it, and the count matches Segments.
func TestBudgetCalibrateSegmentRules(t *testing.T) {
	r := calNow.Unix() + 3600
	rows := []Obs{
		calObs(1, "p", 0.50, calNow, r),
		calObs(2, "p", 0.20, calNow.Add(time.Minute), r),                 // drop
		calObs(3, "p", 0.25, calNow.Add(2*time.Minute), r+18000),         // resets_at jump
		calObs(4, "p", 0.30, calNow.Add(3*time.Minute), r+18000),         // none
		calObs(5, "p", 0.31, time.Unix(r+18000, 0).UTC(), r+18000+18000), // past reset (and a jump: past_reset wins)
	}
	c := calCfg()
	cal := Calibrate(CalibrateInput{Now: calNow, Config: c, Obs: map[string][]Obs{"p": rows}})
	if len(cal.Segments) != 1 {
		t.Fatalf("segments %+v", cal.Segments)
	}
	sg := cal.Segments[0]
	if sg.N != len(Segments(rows, c.ResetEpsilon)) {
		t.Errorf("n %d != len(Segments) %d", sg.N, len(Segments(rows, c.ResetEpsilon)))
	}
	want := []string{"drop", "resets_at_jump", "past_reset"}
	if len(sg.Splits) != 3 {
		t.Fatalf("splits %+v", sg.Splits)
	}
	for i, w := range want {
		if sg.Splits[i].Rule != w {
			t.Errorf("split %d rule %s, want %s", i, sg.Splits[i].Rule, w)
		}
	}
}

// TestBudgetCalibrateProjectionCheck: mean < 0 fails check 3 even when
// mean_abs is small; mean >= 0 and small mean_abs passes; few items is "n
// too small".
func TestBudgetCalibrateProjectionCheck(t *testing.T) {
	reset := calNow.Add(-time.Hour).Unix()
	rt := time.Unix(reset, 0).UTC()
	obs := map[string][]Obs{"p": {calObs(1, "p", 0.50, rt.Add(-time.Minute), reset)}}
	run := func(predicted float64, n int) string {
		var decs []RecordedDecision
		for i := 0; i < n; i++ {
			decs = append(decs, calDecision(int64(i+1), "p", predicted, reset, "reset"))
		}
		return checkStatus(Calibrate(CalibrateInput{Now: calNow, Config: calCfg(), Decisions: decs, Obs: obs}), "projection_error")
	}
	if got := run(0.51, 5); got != CheckPass {
		t.Errorf("+0.01 over 5: %s", got)
	}
	if got := run(0.49, 5); got != CheckFail {
		t.Errorf("−0.01 over 5: %s, want FAIL (mean < 0)", got)
	}
	if got := run(0.90, 5); got != CheckFail {
		t.Errorf("+0.40 over 5: %s, want FAIL (mean_abs >= unit_fraction)", got)
	}
	if got := run(0.51, 4); got != CheckTooSmall {
		t.Errorf("n=4: %s, want n too small", got)
	}
}
