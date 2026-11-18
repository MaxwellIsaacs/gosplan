package budget

import (
	"math"
	"time"
)

// Intervals builds fit intervals for pool from same-segment consecutive rows
// inside [now-lookback, now] and attempt spans. Only spans whose
// model the pool applies to count. An attempt contributes
// Usage.Total() × overlap/(End−Start); a zero-duration attempt contributes
// all its tokens to the interval (From, To] containing its end. An interval
// overlapping a span with no usage or no end is Complete == false.
func Intervals(pool string, rows []Obs, spans []Span, families []string, eps float64, now time.Time, lookback time.Duration) []Interval {
	var out []Interval
	samePairs(Segments(rows, eps), now, lookback, func(a, b Obs) {
		iv := Interval{
			From: a.ObservedAt, To: b.ObservedAt,
			DeltaU:   *b.Utilization - *a.Utilization,
			ByModel:  map[string]float64{},
			Complete: true,
		}
		for _, sp := range spans {
			if !Applies(pool, sp.Model, families) {
				continue
			}
			frac, overlaps := overlapFraction(sp, iv.From, iv.To)
			if !overlaps {
				continue
			}
			if sp.End == nil || sp.Usage == nil {
				iv.Complete = false
				continue
			}
			u := sp.Usage
			iv.Tokens += u.Total() * frac
			iv.ByClass.Input += u.Input * frac
			iv.ByClass.Output += u.Output * frac
			iv.ByClass.CacheRead += u.CacheRead * frac
			iv.ByClass.CacheCreate += u.CacheCreate * frac
			iv.ByModel[sp.Model] += u.Total() * frac
		}
		out = append(out, iv)
	})
	return out
}

// overlapFraction returns the share of span's duration inside (from, to] and
// whether the span touches the interval at all. A running span (End nil)
// touches it when it started before to.
func overlapFraction(sp Span, from, to time.Time) (float64, bool) {
	if sp.End == nil {
		return 0, sp.Start.Before(to)
	}
	dur := sp.End.Sub(sp.Start)
	if dur <= 0 {
		in := sp.End.After(from) && !sp.End.After(to)
		return 1, in
	}
	lo, hi := sp.Start, *sp.End
	if from.After(lo) {
		lo = from
	}
	if to.Before(hi) {
		hi = to
	}
	ov := hi.Sub(lo)
	if ov <= 0 {
		return 0, false
	}
	return float64(ov) / float64(dur), true
}

// Fit (the type, in types.go) is a *fitted ratio* of utilisation per token,
// a blend over models and token classes: it is not the plan's formula. It is
// biased when background usage depends on the time of day (a b pinned by
// daytime idle intervals biases k low at night); the quality gate does not
// remove that bias, calibrate's error measures do.

// FitRatio fits ΔU = k·T + b·Δt (Δt in hours) with k, b ≥ 0 by least squares,
// drops intervals longer than c.FitMaxInterval and incomplete
// ones, reports SEK, and gates the result: basis "fit" only when k > 0 and
// SEK/k ≤ c.FitMaxRelSE. Other bases: none:too_few_points,
// none:no_token_variation, none:singular, none:poor_fit.
func FitRatio(pool string, iv []Interval, c Config) Fit {
	f := Fit{Pool: pool}
	var use []Interval
	for _, x := range iv {
		if !x.Complete {
			continue
		}
		if c.FitMaxInterval > 0 && x.To.Sub(x.From) > c.FitMaxInterval {
			continue
		}
		if !x.To.After(x.From) {
			continue
		}
		use = append(use, x)
		f.SpanSeconds += x.To.Sub(x.From).Seconds()
	}
	f.N = len(use)
	minPts := c.FitMinPoints
	if minPts < 3 {
		minPts = 3
	}
	if f.N < minPts {
		f.Basis = "none:too_few_points"
		return f
	}
	withT := 0
	var stt, stz, szz, sty, szy, syy float64
	for _, x := range use {
		h := x.To.Sub(x.From).Hours()
		if x.Tokens > 0 {
			withT++
		}
		stt += x.Tokens * x.Tokens
		stz += x.Tokens * h
		szz += h * h
		sty += x.Tokens * x.DeltaU
		szy += h * x.DeltaU
		syy += x.DeltaU * x.DeltaU
	}
	if withT < 3 {
		f.Basis = "none:no_token_variation"
		return f
	}
	det := stt*szz - stz*stz
	if stt*szz <= 0 || det < 1e-12*stt*szz {
		f.Basis = "none:singular"
		return f
	}
	k := (sty*szz - szy*stz) / det
	b := (stt*szy - stz*sty) / det
	params := 2
	if k < 0 {
		k, params = 0, 1
		b = math.Max(szy/szz, 0)
	} else if b < 0 {
		b, params = 0, 1
		k = math.Max(sty/stt, 0)
	}
	var sse float64
	for _, x := range use {
		r := x.DeltaU - k*x.Tokens - b*x.To.Sub(x.From).Hours()
		sse += r * r
	}
	n := float64(f.N)
	f.KPerToken, f.BackgroundPerHour = k, b
	f.ResidualRMS = math.Sqrt(sse / n)
	if syy > 0 {
		f.R2 = 1 - sse/syy
	}
	sigma2 := sse / (n - float64(params))
	// (XᵀX)⁻¹ₖₖ is always the full 2×2 one, also after a constrained refit:
	// pinning b at 0 must not hide that k and b are not separately identified.
	f.SEK = math.Sqrt(sigma2 * szz / det)
	if k > 0 && f.SEK/k <= c.FitMaxRelSE {
		f.Basis = "fit"
	} else {
		f.Basis = "none:poor_fit"
	}
	return f
}
