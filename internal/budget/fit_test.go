package budget

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func fitCfg() Config {
	c := DefaultConfig()
	c.FitMinPoints = 8
	c.FitMaxInterval = 30 * time.Minute
	c.FitMaxRelSE = 0.5
	return c
}

// synth builds n complete intervals of 10 min with ΔU = k·T + b·h + noise.
func synth(n int, k, b, noise float64, seed int64) []Interval {
	rng := rand.New(rand.NewSource(seed))
	var out []Interval
	for i := 0; i < n; i++ {
		from := t0.Add(time.Duration(i) * 15 * time.Minute)
		to := from.Add(10 * time.Minute)
		tok := float64(rng.Intn(5)) * 2e6 // 0, 2M .. 8M, varied
		h := to.Sub(from).Hours()
		du := k*tok + b*h + (rng.Float64()*2-1)*noise
		out = append(out, Interval{From: from, To: to, DeltaU: du, Tokens: tok, Complete: true, ByModel: map[string]float64{}})
	}
	return out
}

func TestBudgetFitRecoversKnownRatio(t *testing.T) {
	c := fitCfg()
	f := FitRatio("p", synth(40, 2e-7, 0.01, 0.0005, 1), c)
	if f.Basis != "fit" || f.N != 40 {
		t.Fatalf("basis: %+v", f)
	}
	if math.Abs(f.KPerToken-2e-7)/2e-7 > 0.05 {
		t.Fatalf("k = %v", f.KPerToken)
	}
	if f.R2 <= 0.95 || f.SEK <= 0 || f.ResidualRMS <= 0 || f.SpanSeconds != 40*600 {
		t.Fatalf("stats: %+v", f)
	}
	if f.BackgroundPerHour <= 0 {
		t.Fatalf("background: %+v", f)
	}

	// Negative slope: ΔU falls as T rises → k would be < 0; refit with k = 0.
	iv := synth(30, -1e-7, 0.05, 0.0002, 2)
	f = FitRatio("p", iv, c)
	if f.KPerToken != 0 || f.Basis != "none:poor_fit" {
		t.Fatalf("negative k: %+v", f)
	}
	if f.BackgroundPerHour < 0 {
		t.Fatalf("b must stay >= 0: %+v", f)
	}

	// Incomplete intervals are skipped.
	iv = synth(40, 2e-7, 0.01, 0.0005, 1)
	for i := 0; i < 33; i++ {
		iv[i].Complete = false
	}
	if f = FitRatio("p", iv, c); f.Basis != "none:too_few_points" || f.N != 7 {
		t.Fatalf("incomplete: %+v", f)
	}
	// Intervals longer than max_interval are dropped.
	iv = synth(40, 2e-7, 0.01, 0.0005, 1)
	for i := 0; i < 33; i++ {
		iv[i].To = iv[i].From.Add(31 * time.Minute)
	}
	if f = FitRatio("p", iv, c); f.Basis != "none:too_few_points" || f.N != 7 {
		t.Fatalf("long: %+v", f)
	}
	// Fewer than min_points.
	if f = FitRatio("p", synth(7, 2e-7, 0.01, 0.0005, 1), c); f.Basis != "none:too_few_points" {
		t.Fatalf("few: %+v", f)
	}
	// No token variation: fewer than 3 intervals with T > 0.
	iv = synth(20, 2e-7, 0.01, 0.0005, 1)
	for i := range iv {
		iv[i].Tokens = 0
	}
	iv[0].Tokens, iv[1].Tokens = 1e6, 2e6
	if f = FitRatio("p", iv, c); f.Basis != "none:no_token_variation" {
		t.Fatalf("no variation: %+v", f)
	}
	// Exactly collinear T = τ·Δt: singular.
	iv = synth(20, 2e-7, 0.01, 0, 3)
	for i := range iv {
		iv[i].Tokens = 1e7 * iv[i].To.Sub(iv[i].From).Hours()
	}
	if f = FitRatio("p", iv, c); f.Basis != "none:singular" {
		t.Fatalf("collinear: %+v", f)
	}
	// b < 0 is refit with b = 0.
	iv = synth(40, 2e-7, -0.05, 0.0001, 4)
	f = FitRatio("p", iv, c)
	if f.BackgroundPerHour != 0 || f.KPerToken <= 0 {
		t.Fatalf("b<0 refit: %+v", f)
	}
}

func TestBudgetFitQualityGate(t *testing.T) {
	c := fitCfg()
	rng := rand.New(rand.NewSource(7))
	var iv []Interval
	for i := 0; i < 40; i++ {
		from := t0.Add(time.Duration(i) * 15 * time.Minute)
		// Intervals of 8–12 minutes; constant token rate with 1% jitter,
		// ΔU quantised to whole percents.
		d := time.Duration(8+rng.Intn(5)) * time.Minute
		h := d.Hours()
		tok := 1.5e6 * h * (1 + (rng.Float64()-0.5)*0.02)
		du := math.Round((2e-7*tok+0.2*h)*100) / 100
		iv = append(iv, Interval{From: from, To: from.Add(d), DeltaU: du, Tokens: tok, Complete: true})
	}
	f := FitRatio("p", iv, c)
	if f.Basis != "none:poor_fit" {
		t.Fatalf("collinear fit passed the gate: %+v", f)
	}
	if f.KPerToken > 0 && f.SEK/f.KPerToken <= c.FitMaxRelSE {
		t.Fatalf("expected SEK/k > max_rel_se: %+v", f)
	}
	// The same data passes when the gate is relaxed: the gate is what rejects it.
	c.FitMaxRelSE = 1e9
	if f = FitRatio("p", iv, c); f.KPerToken > 0 && f.Basis != "fit" {
		t.Fatalf("relaxed gate: %+v", f)
	}
}

func TestBudgetIntervalsAttribution(t *testing.T) {
	fam := []string{"opus", "sonnet", "haiku", "fable"}
	// Rows at 0, 10, 20 min.
	rows := []Obs{row(1, 0, .10, nil), row(2, 10, .15, nil), row(3, 20, .25, nil)}
	now := t0.Add(30 * time.Minute)
	end := t0.Add(15 * time.Minute)
	// One attempt 5..15 min: half in each interval.
	span := Span{AttemptID: 1, Model: "sonnet", Start: t0.Add(5 * time.Minute), End: &end,
		Usage: &Tokens{Input: 600, Output: 400, CacheRead: 200, CacheCreate: 0}}
	iv := Intervals("five_hour", rows, []Span{span}, fam, 0.01, now, time.Hour)
	if len(iv) != 2 {
		t.Fatalf("intervals: %+v", iv)
	}
	for _, x := range iv {
		if !x.Complete || !approx(x.Tokens, 600, 1e-9) || !approx(x.ByModel["sonnet"], 600, 1e-9) ||
			!approx(x.ByClass.Input, 300, 1e-9) || !approx(x.ByClass.CacheRead, 100, 1e-9) {
			t.Fatalf("split: %+v", x)
		}
	}
	if !approx(iv[0].DeltaU, .05, 1e-12) || !approx(iv[1].DeltaU, .10, 1e-12) {
		t.Fatalf("delta u: %+v", iv)
	}
	// Uneven overlap: attempt 8..12 → 50/50; attempt 5..20 → 1/3, 2/3... check 2:1.
	end2 := t0.Add(20 * time.Minute)
	s2 := Span{Model: "sonnet", Start: t0.Add(5 * time.Minute), End: &end2, Usage: &Tokens{Output: 1500}}
	iv = Intervals("five_hour", rows, []Span{s2}, fam, 0.01, now, time.Hour)
	if !approx(iv[0].Tokens, 500, 1e-9) || !approx(iv[1].Tokens, 1000, 1e-9) {
		t.Fatalf("overlap: %+v", iv)
	}
	// Running attempt (no end) makes its interval incomplete, and later ones.
	run := Span{Model: "sonnet", Start: t0.Add(12 * time.Minute)}
	iv = Intervals("five_hour", rows, []Span{run}, fam, 0.01, now, time.Hour)
	if !iv[0].Complete || iv[1].Complete {
		t.Fatalf("running: %+v", iv)
	}
	// An ended attempt with no usage is incomplete too.
	nu := Span{Model: "sonnet", Start: t0.Add(1 * time.Minute), End: &end}
	iv = Intervals("five_hour", rows, []Span{nu}, fam, 0.01, now, time.Hour)
	if iv[0].Complete || iv[1].Complete {
		t.Fatalf("no usage: %+v", iv)
	}
	// Models the pool does not apply to are excluded (even running ones).
	op := Span{Model: "claude-opus-5-5", Start: t0.Add(1 * time.Minute), End: &end, Usage: &Tokens{Output: 900}}
	opRun := Span{Model: "claude-opus-5-5", Start: t0.Add(1 * time.Minute)}
	iv = Intervals("week_fable", rows, []Span{op, opRun}, fam, 0.01, now, time.Hour)
	for _, x := range iv {
		if x.Tokens != 0 || !x.Complete || len(x.ByModel) != 0 {
			t.Fatalf("excluded model counted: %+v", x)
		}
	}
	// Zero-duration attempt: all tokens to the interval containing its end.
	z := t0.Add(10 * time.Minute) // exactly on the boundary: (From, To] → first interval
	zs := Span{Model: "sonnet", Start: z, End: &z, Usage: &Tokens{Output: 70}}
	iv = Intervals("five_hour", rows, []Span{zs}, fam, 0.01, now, time.Hour)
	if iv[0].Tokens != 70 || iv[1].Tokens != 0 {
		t.Fatalf("zero duration: %+v", iv)
	}
	// Pairs across a reset or outside the lookback are not intervals.
	rows2 := []Obs{row(1, 0, .50, nil), row(2, 10, .02, nil), row(3, 20, .05, nil)}
	if iv = Intervals("five_hour", rows2, nil, fam, 0.01, now, time.Hour); len(iv) != 1 {
		t.Fatalf("reset: %+v", iv)
	}
	if iv = Intervals("five_hour", rows, nil, fam, 0.01, now, 25*time.Minute); len(iv) != 1 {
		t.Fatalf("lookback: %+v", iv)
	}
}
