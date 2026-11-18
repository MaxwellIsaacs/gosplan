package budget

import "testing"

func TestBudgetQuantileNearestRank(t *testing.T) {
	xs := []float64{300, 100, 600, 200, 500, 400}
	orig := append([]float64(nil), xs...)
	if g := Quantile(xs, 0.9); g != 600 {
		t.Fatalf("p90 = %v", g)
	}
	if g := Quantile(xs, 0.5); g != 300 {
		t.Fatalf("p50 = %v", g)
	}
	for i := range xs {
		if xs[i] != orig[i] {
			t.Fatal("input modified")
		}
	}
	if g := Quantile([]float64{42}, 0.9); g != 42 {
		t.Fatalf("n=1: %v", g)
	}
	// n = 10, q = 0.9: the 9th smallest, not the max.
	ten := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if g := Quantile(ten, 0.9); g != 9 {
		t.Fatalf("n=10 p90 = %v", g)
	}
	// n < 10: the maximum.
	if g := Quantile(ten[:7], 0.9); g != 7 {
		t.Fatalf("n=7 p90 = %v", g)
	}
	if g := Quantile(nil, 0.9); g != 0 {
		t.Fatalf("empty: %v", g)
	}
}

func smp(id int64, model string, tokens float64, turns int) Sample {
	return Sample{AttemptID: id, Model: model, Usage: Tokens{Input: tokens / 2, Output: tokens / 2},
		NumTurns: turns, DurationMS: int64(tokens), CostUSD: tokens / 1000}
}

func TestBudgetEstimateColdAndBound(t *testing.T) {
	c := DefaultConfig()
	var ss []Sample
	for i := 1; i <= 4; i++ {
		ss = append(ss, smp(int64(i), "m", 1000, 10))
	}
	// cold:unknown
	e := EstimateFor("m", ss, 50, c)
	if e.Tokens != nil || e.TokensBasis != "cold:unknown" || e.DurationBasis != "unknown" || e.DurationMS != nil ||
		e.USD != nil || e.USDBasis != "cold:unknown" || e.N != 4 {
		t.Fatalf("cold unknown: %+v", e)
	}
	// cold:config
	c.EstimateColdTokens = 10000
	c.EstimateColdDuration = 90e9
	e = EstimateFor("m", ss, 50, c)
	if e.Tokens == nil || *e.Tokens != 15000 || e.TokensBasis != "cold:config" ||
		e.DurationMS == nil || *e.DurationMS != 90000 || e.DurationBasis != "cold:config" {
		t.Fatalf("cold config: %+v", e)
	}
	// Other models' samples are ignored: 4 of "m" + 10 of "x" is still cold.
	for i := 10; i < 20; i++ {
		ss = append(ss, smp(int64(i), "x", 5, 1))
	}
	if e = EstimateFor("m", ss, 50, c); e.TokensBasis != "cold:config" || e.N != 4 {
		t.Fatalf("other models counted: %+v", e)
	}
	// History, no bound (max_turns large): p90 = max of 5.
	ss = append(ss[:4], smp(5, "m", 3000, 10))
	e = EstimateFor("m", ss, 1000, c)
	if e.Tokens == nil || *e.Tokens != 3000 || e.TokensBasis != "history:p90,n=5" {
		t.Fatalf("history: %+v", e)
	}
	if e.DurationBasis != "history:p90,n=5" || *e.DurationMS != 3000 || e.USDBasis != "history:p90,n=5" || *e.USD != 3 {
		t.Fatalf("history dur/usd: %+v", e)
	}
	// Turn bound applies when max_turns × p90(tokens/turn) is lower:
	// per-turn tokens: 100,100,100,100,300 → p90 = 300; max_turns 5 → 1500 < 3000.
	e = EstimateFor("m", ss, 5, c)
	if *e.Tokens != 1500 || e.TokensBasis != "history:p90,n=5+turn_bound" {
		t.Fatalf("turn bound: %+v", e)
	}
	// Samples without turns give no bound.
	nt := make([]Sample, len(ss))
	copy(nt, ss)
	for i := range nt {
		nt[i].NumTurns = 0
	}
	if e = EstimateFor("m", nt, 5, c); *e.Tokens != 3000 || e.TokensBasis != "history:p90,n=5" {
		t.Fatalf("no turns: %+v", e)
	}
	// Window: only the newest `window` by AttemptID.
	c.EstimateWindow = 5
	big := []Sample{smp(1, "m", 99999, 10)}
	for i := 2; i <= 6; i++ {
		big = append(big, smp(int64(i), "m", 100, 10))
	}
	if e = EstimateFor("m", big, 1000, c); *e.Tokens != 100 || e.N != 5 {
		t.Fatalf("window: %+v", e)
	}
}
