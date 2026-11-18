package budget

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Calibrate is the one-week check: pure, every input is in the
// CalibrateInput. Nothing here says what the plan "really" does. Each number
// carries its n and its basis, and every threshold in the checks is either
// the spec's (±0.05, n >= 20) or a display limit named below.

// Calibration display limits (not budget policy: they only decide whether a
// check prints PASS/FAIL or "n too small").
const (
	// CalibrateMinOverrunN is the spec's n >= 20 for the overrun check.
	CalibrateMinOverrunN = 20
	// CalibrateOverrunTolerance is the spec's ±0.05 around 1 − q.
	CalibrateOverrunTolerance = 0.05
	// CalibrateMinProjectionN is the fewest projection-error items for check 3.
	CalibrateMinProjectionN = 5
	// AliasWindow is how close in time two pools' rows must be to pair.
	AliasWindow = 10 * time.Minute
)

// Check statuses.
const (
	CheckPass     = "PASS"
	CheckFail     = "FAIL"
	CheckTooSmall = "n too small"
)

// SettledRes is one settled reservation, for the overrun comparison.
type SettledRes struct {
	ID           int64         `json:"id"`
	Run          string        `json:"run"`
	Task         string        `json:"task"`
	Model        string        `json:"model"`
	SettledAt    time.Time     `json:"settled_at"`
	EstTokens    *float64      `json:"est_tokens"`    // nil = no estimate was known
	ActualTokens *float64      `json:"actual_tokens"` // nil = usage unknown
	Pools        []PoolReserve `json:"pools"`
}

// RecordedDecision is one budget_decision as logged: the inputs calibrate
// needs are inside Decision (pool views carry level, projection, reset).
type RecordedDecision struct {
	EID      int64     `json:"eid"`
	Run      string    `json:"run"`
	Task     string    `json:"task"`
	Now      time.Time `json:"now"`
	Decision Decision  `json:"decision"`
}

// CalibrateInput is every input of Calibrate. Since zero means all data.
// Now bounds "reset has passed". Samples are per model (newest-first window
// as the estimate saw them); Obs are per pool; Fits are computed by the
// caller (Intervals + FitRatio) so this function does no fitting.
type CalibrateInput struct {
	Now       time.Time
	Since     time.Time
	Config    Config
	Fits      []Fit
	Samples   map[string][]Sample
	Settled   []SettledRes
	Decisions []RecordedDecision
	Obs       map[string][]Obs
}

// EstimateCal is one model's estimate quality. Tokens are estimates of what
// an attempt uses, from this model's finished attempts only.
type EstimateCal struct {
	Model               string  `json:"model"`
	N                   int     `json:"n"` // samples
	P50                 float64 `json:"p50"`
	P90                 float64 `json:"p90"`
	P99                 float64 `json:"p99"`
	Quantile            float64 `json:"quantile"`
	SettledN            int     `json:"settled_n"` // settled reservations with an estimate and a known actual
	OverrunN            int     `json:"overrun_n"` // actual tokens > estimate tokens
	OverrunRate         float64 `json:"overrun_rate"`
	ExpectedOverrunRate float64 `json:"expected_overrun_rate"` // 1 − quantile
}

// ProjectionItem is one decision's projected level at a reset vs observed.
type ProjectionItem struct {
	Pool        string  `json:"pool"`
	DecisionEID int64   `json:"decision_eid"`
	Predicted   float64 `json:"predicted"`
	Observed    float64 `json:"observed"`
	ResetAt     int64   `json:"reset_at"`
}

// ProjectionError summarises predicted − observed. Mean > 0 means we
// over-projected (the admission was cautious there).
type ProjectionError struct {
	N       int              `json:"n"`
	Mean    float64          `json:"mean"`
	MeanAbs float64          `json:"mean_abs"`
	Items   []ProjectionItem `json:"items"`
}

// AliasPair is the evidence for aliasing pool names a and b.
type AliasPair struct {
	A           string  `json:"a"`
	B           string  `json:"b"`
	Pairs       int     `json:"pairs"`
	MeanAbsDiff float64 `json:"mean_abs_diff"`
}

// SegmentSplit is one segment boundary and the rule that made it.
type SegmentSplit struct {
	At   time.Time `json:"at"`
	Rule string    `json:"rule"` // drop | resets_at_jump | past_reset
}

// SegmentCal is one pool's segmentation over the data.
type SegmentCal struct {
	Pool   string         `json:"pool"`
	N      int            `json:"n"`
	Splits []SegmentSplit `json:"splits"`
}

// Check is one item of the one-week check.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // PASS | FAIL | n too small
	Detail string `json:"detail"`
}

// Calibration is the result of Calibrate.
type Calibration struct {
	Since           *time.Time      `json:"since"`
	Fits            []Fit           `json:"fits"`
	Estimates       []EstimateCal   `json:"estimates"`
	Refusals        map[string]int  `json:"refusals"`
	ProjectionError ProjectionError `json:"projection_error"`
	AliasEvidence   []AliasPair     `json:"alias_evidence"`
	Segments        []SegmentCal    `json:"segments"`
	Checks          []Check         `json:"checks"`
	Notes           []string        `json:"notes"`
}

// Calibrate checks whether the budget model is working.
func Calibrate(in CalibrateInput) Calibration {
	c := in.Config
	out := Calibration{Fits: in.Fits, Refusals: map[string]int{}}
	if out.Fits == nil {
		out.Fits = []Fit{}
	}
	if !in.Since.IsZero() {
		s := in.Since
		out.Since = &s
	}
	since := func(t time.Time) bool { return in.Since.IsZero() || !t.Before(in.Since) }

	// Estimates and overrun.
	models := map[string]bool{}
	for m := range in.Samples {
		models[m] = true
	}
	for _, r := range in.Settled {
		models[r.Model] = true
	}
	names := make([]string, 0, len(models))
	for m := range models {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		e := EstimateCal{Model: m, N: len(in.Samples[m]), Quantile: c.EstimateQuantile, ExpectedOverrunRate: 1 - c.EstimateQuantile}
		tok := make([]float64, len(in.Samples[m]))
		for i, s := range in.Samples[m] {
			tok[i] = s.Usage.Total()
		}
		e.P50, e.P90, e.P99 = Quantile(tok, 0.5), Quantile(tok, 0.9), Quantile(tok, 0.99)
		for _, r := range in.Settled {
			if r.Model != m || !since(r.SettledAt) || r.EstTokens == nil || r.ActualTokens == nil {
				continue
			}
			e.SettledN++
			if *r.ActualTokens > *r.EstTokens {
				e.OverrunN++
			}
		}
		if e.SettledN > 0 {
			e.OverrunRate = float64(e.OverrunN) / float64(e.SettledN)
		}
		out.Estimates = append(out.Estimates, e)
	}
	if out.Estimates == nil {
		out.Estimates = []EstimateCal{}
	}

	// Refusals by rule, and the decisions in range.
	var decs []RecordedDecision
	for _, d := range in.Decisions {
		if !since(d.Now) {
			continue
		}
		decs = append(decs, d)
		if !d.Decision.Admit {
			out.Refusals[d.Decision.Rule]++
		}
	}

	obs := map[string][]Obs{}
	for p, rows := range in.Obs {
		for _, o := range rows {
			if since(o.ObservedAt) {
				obs[p] = append(obs[p], o)
			}
		}
	}
	out.ProjectionError = projectionError(decs, obs, in.Now)
	out.AliasEvidence = aliasEvidence(obs)
	out.Segments = segmentCal(obs, c.ResetEpsilon)
	out.Checks = checks(c, out, decs)
	out.Notes = []string{
		"Token numbers are estimates of attempt size from this machine's finished attempts; a fitted ratio is a regression over observed intervals, not the plan's formula.",
		"Window utilisation from unifiedWindows, rateLimitType and /usage text is undocumented.",
		fmt.Sprintf("Overrun is actual tokens > estimate tokens, over settled reservations with both known (settled_n). The expected rate is 1 − quantile = %.2f; it is only meaningful at n >= %d.", 1-c.EstimateQuantile, CalibrateMinOverrunN),
		"Projection error uses decisions whose horizon ended at the pool's reset (horizon_basis reset) and a reset that has passed; decisions with a max_horizon basis are not compared.",
		fmt.Sprintf("Alias evidence pairs each row with the nearest row of the other pool within %s; small mean_abs_diff over many pairs is the case for an alias, one agreeing pair is not.", AliasWindow),
		"Class fractions and every budget.* default are policy, not measurement.",
	}
	return out
}

func projectionError(decs []RecordedDecision, obs map[string][]Obs, now time.Time) ProjectionError {
	pe := ProjectionError{Items: []ProjectionItem{}}
	for _, d := range decs {
		for _, p := range d.Decision.Pools {
			if p.ResetsAt == nil || p.HorizonBasis != "reset" || *p.ResetsAt > now.Unix() {
				continue
			}
			reset := time.Unix(*p.ResetsAt, 0).UTC()
			w := time.Duration(0.05 * p.HorizonSeconds * float64(time.Second))
			if w < 10*time.Minute {
				w = 10 * time.Minute
			}
			var last *Obs
			rows := sortedWithLevel(obs[p.Pool])
			for i := range rows {
				o := rows[i]
				if o.ObservedAt.After(reset) || o.ObservedAt.Before(reset.Add(-w)) || !o.ObservedAt.After(d.Now) {
					continue
				}
				// Same segment: the window the decision saw (resets_at
				// within the 60 s the segmenter tolerates).
				if o.ResetsAt == nil || absInt64(*o.ResetsAt-*p.ResetsAt) > 60 {
					continue
				}
				last = &rows[i]
			}
			if last == nil {
				continue
			}
			pe.Items = append(pe.Items, ProjectionItem{Pool: p.Pool, DecisionEID: d.EID, Predicted: p.Projected, Observed: *last.Utilization, ResetAt: *p.ResetsAt})
		}
	}
	for _, it := range pe.Items {
		pe.Mean += it.Predicted - it.Observed
		pe.MeanAbs += math.Abs(it.Predicted - it.Observed)
	}
	pe.N = len(pe.Items)
	if pe.N > 0 {
		pe.Mean /= float64(pe.N)
		pe.MeanAbs /= float64(pe.N)
	}
	return pe
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func aliasEvidence(obs map[string][]Obs) []AliasPair {
	pools := make([]string, 0, len(obs))
	for p := range obs {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	out := []AliasPair{}
	for i := range pools {
		for j := i + 1; j < len(pools); j++ {
			a, b := sortedWithLevel(obs[pools[i]]), sortedWithLevel(obs[pools[j]])
			n, sum := 0, 0.0
			for _, ra := range a {
				best := -1
				var bd time.Duration
				for k, rb := range b {
					d := ra.ObservedAt.Sub(rb.ObservedAt)
					if d < 0 {
						d = -d
					}
					if d <= AliasWindow && (best < 0 || d < bd) {
						best, bd = k, d
					}
				}
				if best >= 0 {
					n++
					sum += math.Abs(*ra.Utilization - *b[best].Utilization)
				}
			}
			if n > 0 {
				out = append(out, AliasPair{A: pools[i], B: pools[j], Pairs: n, MeanAbsDiff: sum / float64(n)})
			}
		}
	}
	return out
}

// segmentCal mirrors Segments' three split rules (same order of
// precedence: drop, then resets_at jump, then past reset) to name the rule.
func segmentCal(obs map[string][]Obs, eps float64) []SegmentCal {
	pools := make([]string, 0, len(obs))
	for p := range obs {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	out := []SegmentCal{}
	for _, p := range pools {
		s := sortedWithLevel(obs[p])
		if len(s) == 0 {
			continue
		}
		sc := SegmentCal{Pool: p, N: 1, Splits: []SegmentSplit{}}
		for i := 0; i+1 < len(s); i++ {
			a, b := s[i], s[i+1]
			rule := ""
			if *b.Utilization < *a.Utilization-eps {
				rule = "drop"
			}
			if a.ResetsAt != nil && b.ResetsAt != nil && *b.ResetsAt > *a.ResetsAt+60 {
				rule = "resets_at_jump"
			}
			if a.ResetsAt != nil && b.ObservedAt.Unix() >= *a.ResetsAt {
				rule = "past_reset"
			}
			if rule != "" {
				sc.N++
				sc.Splits = append(sc.Splits, SegmentSplit{At: b.ObservedAt, Rule: rule})
			}
		}
		out = append(out, sc)
	}
	return out
}

func checks(c Config, cal Calibration, decs []RecordedDecision) []Check {
	var out []Check
	// (1) overrun rate per model.
	for _, e := range cal.Estimates {
		ck := Check{Name: "overrun_rate " + e.Model}
		ck.Detail = fmt.Sprintf("overrun %d of %d settled = %.3f, expected %.3f (1 − q) ±%.2f", e.OverrunN, e.SettledN, e.OverrunRate, e.ExpectedOverrunRate, CalibrateOverrunTolerance)
		switch {
		case e.SettledN < CalibrateMinOverrunN:
			ck.Status = CheckTooSmall
			ck.Detail += fmt.Sprintf(" (needs n >= %d)", CalibrateMinOverrunN)
		case math.Abs(e.OverrunRate-e.ExpectedOverrunRate) <= CalibrateOverrunTolerance+1e-9:
			ck.Status = CheckPass
		default:
			ck.Status = CheckFail
		}
		out = append(out, ck)
	}
	// (2) fitted pools: residual_rms below the typical reservation.
	for _, f := range cal.Fits {
		if f.Basis != "fit" {
			continue
		}
		var res []float64
		for _, d := range decs {
			if !d.Decision.Admit {
				continue
			}
			for _, r := range d.Decision.Reserve {
				if r.Pool == f.Pool {
					res = append(res, r.Reserve)
				}
			}
		}
		ck := Check{Name: "fit_residual " + f.Pool}
		if len(res) == 0 {
			ck.Status = CheckTooSmall
			ck.Detail = fmt.Sprintf("residual_rms %.4f (n=%d); no admitted reservation in this pool to compare with", f.ResidualRMS, f.N)
		} else {
			typ := Quantile(res, 0.5)
			ck.Detail = fmt.Sprintf("residual_rms %.4f (n=%d) vs median reservation %.4f over %d admitted decisions", f.ResidualRMS, f.N, typ, len(res))
			ck.Status = CheckFail
			if f.ResidualRMS < typ {
				ck.Status = CheckPass
			}
		}
		out = append(out, ck)
	}
	// (3) projection error.
	pe := cal.ProjectionError
	ck := Check{Name: "projection_error"}
	ck.Detail = fmt.Sprintf("n=%d mean %+.4f mean_abs %.4f vs unit_fraction %.4f", pe.N, pe.Mean, pe.MeanAbs, c.ColdUnitFraction)
	switch {
	case pe.N < CalibrateMinProjectionN:
		ck.Status = CheckTooSmall
		ck.Detail += fmt.Sprintf(" (needs n >= %d)", CalibrateMinProjectionN)
	case pe.Mean >= 0 && pe.MeanAbs < c.ColdUnitFraction:
		ck.Status = CheckPass
	default:
		ck.Status = CheckFail
	}
	out = append(out, ck)
	// (4) refusals mostly headroom/class_hold.
	total, good := 0, 0
	for rule, n := range cal.Refusals {
		total += n
		if rule == RuleHeadroom || rule == RuleClassHold {
			good += n
		}
	}
	ck = Check{Name: "refusal_mix"}
	ck.Detail = fmt.Sprintf("%d of %d refusals are headroom/class_hold; stale %d, bootstrap %d", good, total, cal.Refusals[RuleStale], cal.Refusals[RuleBootstrap])
	switch {
	case total == 0:
		ck.Status = CheckTooSmall
	case good*2 > total:
		ck.Status = CheckPass
	default:
		ck.Status = CheckFail
		if cal.Refusals[RuleStale] > good {
			ck.Detail += "; stale dominates: the stream is not refreshing pools, set an alias or lower the run width"
		}
	}
	return append(out, ck)
}
