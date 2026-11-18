package budget

import (
	"math"
	"testing"
	"time"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestBudgetBurnRateBases(t *testing.T) {
	now := t0.Add(3 * time.Hour)
	one := [][]Obs{{row(1, 150, .4, nil)}}
	r := BurnRate(one, now, 2*time.Hour, Fit{})
	if r.Basis != "none" || r.PerHour != 0 || r.Pairs != 0 {
		t.Fatalf("one row: %+v", r)
	}
	// +0.06/h linear: rows every 30 min, +0.03 each.
	lin := [][]Obs{{row(1, 60, .10, nil), row(2, 90, .13, nil), row(3, 120, .16, nil), row(4, 150, .19, nil)}}
	r = BurnRate(lin, now, 2*time.Hour, Fit{})
	if r.Basis != "observed_total" || !approx(r.PerHour, 0.06, 1e-9) || r.Pairs != 3 || !approx(r.SpanSeconds, 5400, 1e-6) {
		t.Fatalf("linear: %+v", r)
	}
	if !approx(r.ResidualPerHour, 0, 1e-9) {
		t.Fatalf("residual of a linear series: %v", r.ResidualPerHour)
	}
	// Pairs outside the lookback are not used.
	r = BurnRate(lin, now, 75*time.Minute, Fit{})
	if r.Pairs != 1 {
		t.Fatalf("lookback pairs: %+v", r)
	}
	// Negative totals clamp to zero.
	neg := [][]Obs{{row(1, 100, .30, nil), row(2, 130, .295, nil)}}
	if r = BurnRate(neg, now, 2*time.Hour, Fit{}); r.PerHour != 0 || r.Basis != "observed_total" {
		t.Fatalf("negative: %+v", r)
	}
	// fit_background only when the gate passed.
	r = BurnRate(lin, now, 2*time.Hour, Fit{Basis: "fit", BackgroundPerHour: 0.011})
	if r.Basis != "fit_background" || r.PerHour != 0.011 {
		t.Fatalf("fit: %+v", r)
	}
	r = BurnRate(lin, now, 2*time.Hour, Fit{Basis: "none:poor_fit", BackgroundPerHour: 0.011})
	if r.Basis != "observed_total" || !approx(r.PerHour, 0.06, 1e-9) {
		t.Fatalf("poor fit must not be used: %+v", r)
	}
	// Residual is nonzero for a bursty series.
	bursty := [][]Obs{{row(1, 60, .10, nil), row(2, 90, .20, nil), row(3, 120, .20, nil)}}
	if r = BurnRate(bursty, now, 2*time.Hour, Fit{}); r.ResidualPerHour <= 0 {
		t.Fatalf("residual: %+v", r)
	}
}

func testCfg() Config { return DefaultConfig() }

func TestBudgetViewPoolProjection(t *testing.T) {
	c := testCfg()
	c.StaleAfter = 2 * time.Hour
	// 0.40 at t0, 0.05/h, observed at t0, now = t0 (reset in 3h; max_horizon 4h).
	c.MaxHorizon = 4 * time.Hour
	c.Lookback = 4 * time.Hour
	reset := t0.Add(3 * time.Hour).Unix()
	rows := []Obs{row(1, -60, .35, ip(reset)), row(2, 0, .40, ip(reset))}
	v := ViewPool("p", rows, nil, Fit{}, t0, c)
	if v.Level == nil || *v.Level != .40 || !approx(v.Rate.PerHour, 0.05, 1e-9) {
		t.Fatalf("level/rate: %+v", v)
	}
	if v.HorizonBasis != "reset" || v.HorizonSeconds != 3*3600 {
		t.Fatalf("horizon: %v %s", v.HorizonSeconds, v.HorizonBasis)
	}
	if !approx(v.Projected, 0.55, 1e-9) || !approx(v.Headroom, 0.45, 1e-9) || v.Stale {
		t.Fatalf("projection: %+v", v)
	}
	if v.ResetsAt == nil || *v.ResetsAt != reset {
		t.Fatalf("resets_at: %v", v.ResetsAt)
	}

	// Reset unknown: horizon is capped at max_horizon.
	rows = []Obs{row(1, -60, .35, nil), row(2, 0, .40, nil)}
	v = ViewPool("p", rows, nil, Fit{}, t0, c)
	if v.HorizonBasis != "max_horizon" || v.HorizonSeconds != 4*3600 || !approx(v.Projected, 0.60, 1e-9) {
		t.Fatalf("unknown reset: %+v", v)
	}
	// Reset 4 days out is also max_horizon.
	far := t0.Add(96 * time.Hour).Unix()
	rows = []Obs{row(1, -60, .35, ip(far)), row(2, 0, .40, ip(far))}
	v = ViewPool("p", rows, nil, Fit{}, t0, c)
	if v.HorizonBasis != "max_horizon" {
		t.Fatalf("far reset basis: %s", v.HorizonBasis)
	}
	// The horizon is measured from the observation: observed 1h before now.
	rows = []Obs{row(1, -120, .35, nil), row(2, -60, .40, nil)}
	v = ViewPool("p", rows, nil, Fit{}, t0, c)
	if v.HorizonSeconds != 5*3600 {
		t.Fatalf("horizon from observation: %v", v.HorizonSeconds)
	}
	// Clamp at 1.
	rows = []Obs{row(1, -60, .80, nil), row(2, 0, .90, nil)}
	v = ViewPool("p", rows, nil, Fit{}, t0, c)
	if v.Projected != 1 || v.Headroom != 0 {
		t.Fatalf("clamp: %+v", v)
	}
	// Fitted background used when the gate passed.
	v = ViewPool("p", rows, nil, Fit{Basis: "fit", BackgroundPerHour: 0.001}, t0, c)
	if !approx(v.Projected, 0.904, 1e-9) || v.Rate.Basis != "fit_background" {
		t.Fatalf("fit background: %+v", v)
	}
}

func TestBudgetViewPoolStale(t *testing.T) {
	c := testCfg() // stale_after 20m
	rows := []Obs{row(1, -30, .3, nil)}
	v := ViewPool("p", rows, nil, Fit{}, t0, c)
	if !v.Stale || v.StaleReason != "age" || v.Level == nil {
		t.Fatalf("age: %+v", v)
	}
	if v.Projected == 0 && v.HorizonSeconds == 0 {
		t.Fatalf("a stale pool still gets numbers: %+v", v)
	}
	rows = []Obs{row(1, -5, .3, ip(t0.Add(-time.Minute).Unix()))}
	v = ViewPool("p", rows, nil, Fit{}, t0, c)
	if !v.Stale || v.StaleReason != "past_reset" {
		t.Fatalf("past_reset: %+v", v)
	}
	// Fresh.
	rows = []Obs{row(1, -5, .3, ip(t0.Add(time.Hour).Unix()))}
	if v = ViewPool("p", rows, nil, Fit{}, t0, c); v.Stale || v.StaleReason != "" {
		t.Fatalf("fresh: %+v", v)
	}
	// No utilisation.
	rows = []Obs{{ID: 1, Pool: "p", Status: "allowed", ObservedAt: t0}}
	v = ViewPool("p", rows, nil, Fit{}, t0, c)
	if !v.Stale || v.StaleReason != "no_level" || v.Level != nil || v.Status != "allowed" {
		t.Fatalf("no_level: %+v", v)
	}
	v = ViewPool("p", nil, nil, Fit{}, t0, c)
	if !v.Stale || v.StaleReason != "no_level" {
		t.Fatalf("empty: %+v", v)
	}
}

func TestBudgetViewPoolFloor(t *testing.T) {
	c := testCfg()
	rows := []Obs{row(1, -10, .30, nil), row(2, -2, .01, nil)}
	fl := row(9, -8, .60, ip(t0.Add(time.Hour).Unix()))
	fl.Source = "stream"
	v := ViewPool("p", rows, &fl, Fit{}, t0, c)
	if v.Level == nil || *v.Level != .60 || v.Source != "floor:stream" {
		t.Fatalf("floor: %+v", v)
	}
	// Same floor past its reset: the floor is ignored.
	fl.ResetsAt = ip(t0.Add(-time.Minute).Unix())
	v = ViewPool("p", rows, &fl, Fit{}, t0, c)
	if *v.Level != .01 || v.Source != "test" {
		t.Fatalf("expired floor: %+v", v)
	}
	// Floor with unknown reset applies; a lower floor does not.
	fl.ResetsAt = nil
	if v = ViewPool("p", rows, &fl, Fit{}, t0, c); *v.Level != .60 {
		t.Fatalf("floor no reset: %+v", v)
	}
	fl.Utilization = fp(0.005)
	if v = ViewPool("p", rows, &fl, Fit{}, t0, c); *v.Level != .01 {
		t.Fatalf("lower floor: %+v", v)
	}
	// HorizonBasis for a reset 4 days out.
	far := t0.Add(96 * time.Hour).Unix()
	rows = []Obs{row(1, -2, .01, ip(far))}
	if v = ViewPool("p", rows, nil, Fit{}, t0, c); v.HorizonBasis != "max_horizon" {
		t.Fatalf("basis: %s", v.HorizonBasis)
	}
}

func TestBudgetApplies(t *testing.T) {
	fam := []string{"opus", "sonnet", "haiku", "fable"}
	tests := []struct {
		pool, model string
		want        bool
	}{
		{"week_fable", "sonnet", false},
		{"week_fable", "claude-fable-5-1", true},
		{"week_fable", "Fable", true},
		{"seven_day_opus", "opus", true},
		{"seven_day_opus", "claude-sonnet-5-5", false},
		{"five_hour", "sonnet", true},
		{"five_hour", "anything", true},
		{"week_all", "claude-opus-5-5", true},
		{"week_other", "sonnet", true}, // not a configured family
	}
	for _, tc := range tests {
		if got := Applies(tc.pool, tc.model, fam); got != tc.want {
			t.Errorf("Applies(%s,%s)=%v want %v", tc.pool, tc.model, got, tc.want)
		}
	}
	if !Applies("week_fable", "sonnet", nil) {
		t.Error("no families: every pool applies")
	}
}
