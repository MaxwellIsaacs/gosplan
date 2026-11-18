package budget

import (
	"math"
	"strings"
	"time"
)

// Applies reports whether pool applies to model. A pool applies
// to every model unless its name ends with _<family> for a family in
// families; then only to models whose name contains the family
// (case-insensitive).
func Applies(pool, model string, families []string) bool {
	p, m := strings.ToLower(pool), strings.ToLower(model)
	for _, f := range families {
		f = strings.ToLower(f)
		if f != "" && strings.HasSuffix(p, "_"+f) {
			return strings.Contains(m, f)
		}
	}
	return true
}

// BurnRate over same-segment pairs inside [now-lookback, now].
// With fit.Basis == "fit" the rate is the fitted background rate (basis
// "fit_background"); otherwise it is the observed total rate, which includes
// our own workers (basis "observed_total"), or "none" without a pair.
// Negative total rates are clamped to 0.
func BurnRate(segs [][]Obs, now time.Time, lookback time.Duration, fit Fit) Rate {
	var sumU, sumH float64
	type pr struct{ du, h float64 }
	var prs []pr
	samePairs(segs, now, lookback, func(a, b Obs) {
		h := b.ObservedAt.Sub(a.ObservedAt).Hours()
		du := *b.Utilization - *a.Utilization
		sumU += du
		sumH += h
		prs = append(prs, pr{du, h})
	})
	r := Rate{Pairs: len(prs), SpanSeconds: sumH * 3600, Basis: "none"}
	if len(prs) > 0 {
		mean := sumU / sumH
		var v float64
		for _, p := range prs {
			d := p.du/p.h - mean
			v += p.h * d * d
		}
		r.ResidualPerHour = math.Sqrt(v / sumH)
		r.PerHour = math.Max(mean, 0)
		r.Basis = "observed_total"
	}
	if fit.Basis == "fit" {
		r.PerHour = fit.BackgroundPerHour
		r.Basis = "fit_background"
	}
	return r
}

// ViewPool computes level, staleness, rate, horizon, projection and headroom.
// A stale pool still gets its numbers computed.
// Projected and Headroom are the projection at the horizon, not at the reset
// (see PoolView.HorizonBasis).
func ViewPool(pool string, rows []Obs, floor *Obs, fit Fit, now time.Time, c Config) PoolView {
	v := PoolView{Pool: pool, Fit: fit}
	lv := sortedWithLevel(rows)
	segs := Segments(rows, c.ResetEpsilon)
	if len(rows) > 0 {
		newest := rows[0]
		for _, r := range rows[1:] {
			if r.ObservedAt.After(newest.ObservedAt) || (r.ObservedAt.Equal(newest.ObservedAt) && r.ID > newest.ID) {
				newest = r
			}
		}
		v.Status = newest.Status
	}
	var level *Obs
	if len(lv) > 0 {
		l := lv[len(lv)-1]
		level = &l
		v.Source = l.Source
	}
	if floor != nil && floor.Utilization != nil &&
		(floor.ResetsAt == nil || *floor.ResetsAt > now.Unix()) &&
		(level == nil || *floor.Utilization > *level.Utilization) {
		f := *floor
		level = &f
		v.Source = "floor:" + floor.Source
	}
	if len(segs) > 0 {
		cur := segs[len(segs)-1]
		for i := len(cur) - 1; i >= 0; i-- {
			if cur[i].ResetsAt != nil {
				r := *cur[i].ResetsAt
				v.ResetsAt = &r
				break
			}
		}
	}
	v.Rate = BurnRate(segs, now, c.Lookback, fit)
	if level == nil {
		v.Stale, v.StaleReason = true, "no_level"
		v.HorizonBasis = "max_horizon"
		return v
	}
	u := *level.Utilization
	at := level.ObservedAt
	v.Level, v.ObservedAt, v.Documented = &u, &at, level.Documented
	v.AgeSeconds = now.Sub(at).Seconds()
	switch {
	case now.Sub(at) > c.StaleAfter:
		v.Stale, v.StaleReason = true, "age"
	case v.ResetsAt != nil && now.Unix() >= *v.ResetsAt:
		v.Stale, v.StaleReason = true, "past_reset"
	}

	capH := now.Sub(at) + c.MaxHorizon
	v.HorizonBasis = "max_horizon"
	h := capH
	if v.ResetsAt != nil && *v.ResetsAt > now.Unix() {
		toReset := time.Unix(*v.ResetsAt, 0).Sub(at)
		if toReset <= capH {
			h, v.HorizonBasis = toReset, "reset"
		}
	}
	if h < 0 {
		h = 0
	}
	v.HorizonSeconds = h.Seconds()
	v.Projected = math.Min(1, math.Max(0, u+v.Rate.PerHour*h.Hours()))
	v.Headroom = 1 - v.Projected
	return v
}
