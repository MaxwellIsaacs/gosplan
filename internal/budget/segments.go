package budget

import (
	"sort"
	"time"
)

// sortedWithLevel returns the rows that carry a utilisation, sorted by
// (ObservedAt, ID). The input is not modified.
func sortedWithLevel(rows []Obs) []Obs {
	out := make([]Obs, 0, len(rows))
	for _, r := range rows {
		if r.Utilization != nil {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].ObservedAt.Equal(out[j].ObservedAt) {
			return out[i].ObservedAt.Before(out[j].ObservedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Segments splits rows (one pool, any order; rows without Utilization are
// ignored) into reset segments, each sorted by (ObservedAt, ID).
// A new segment starts at row i+1 when u[i+1] < u[i]-eps, when both resets_at
// are known and resets_at[i+1] > resets_at[i]+60s, or when resets_at[i] is
// known and observed_at[i+1] >= resets_at[i].
func Segments(rows []Obs, eps float64) [][]Obs {
	s := sortedWithLevel(rows)
	if len(s) == 0 {
		return nil
	}
	var segs [][]Obs
	cur := []Obs{s[0]}
	for i := 0; i+1 < len(s); i++ {
		a, b := s[i], s[i+1]
		split := *b.Utilization < *a.Utilization-eps
		if a.ResetsAt != nil && b.ResetsAt != nil && *b.ResetsAt > *a.ResetsAt+60 {
			split = true
		}
		if a.ResetsAt != nil && b.ObservedAt.Unix() >= *a.ResetsAt {
			split = true
		}
		if split {
			segs = append(segs, cur)
			cur = nil
		}
		cur = append(cur, b)
	}
	return append(segs, cur)
}

// samePairs calls f for every consecutive same-segment pair whose rows both
// lie in [now-lookback, now] and whose Δt is positive.
func samePairs(segs [][]Obs, now time.Time, lookback time.Duration, f func(a, b Obs)) {
	lo := now.Add(-lookback)
	for _, seg := range segs {
		for i := 0; i+1 < len(seg); i++ {
			a, b := seg[i], seg[i+1]
			if a.ObservedAt.Before(lo) || b.ObservedAt.After(now) {
				continue
			}
			if !b.ObservedAt.After(a.ObservedAt) {
				continue
			}
			f(a, b)
		}
	}
}
