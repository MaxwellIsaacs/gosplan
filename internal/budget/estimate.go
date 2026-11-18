package budget

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Quantile is nearest-rank: the ceil(q*n)-th smallest (1-based) of xs; xs is
// not modified. Nearest rank; for q = 0.9 and n < 10 this is the sample
// maximum. Empty xs gives 0.
func Quantile(xs []float64, q float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	r := int(math.Ceil(q*float64(n) - 1e-9))
	if r < 1 {
		r = 1
	}
	if r > n {
		r = n
	}
	return s[r-1]
}

func historyBasis(q float64, n int) string {
	p := math.Round(q*100*1e6) / 1e6
	return fmt.Sprintf("history:p%s,n=%d", strconv.FormatFloat(p, 'f', -1, 64), n)
}

// EstimateFor builds the model's estimate from samples. It is an
// estimate of an attempt's tokens, never "the cost": how the plan weighs
// tokens is unknown. Only samples of model count, newest
// c.EstimateWindow by AttemptID.
func EstimateFor(model string, samples []Sample, maxTurns int, c Config) Estimate {
	var ss []Sample
	for _, s := range samples {
		if s.Model == model {
			ss = append(ss, s)
		}
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].AttemptID > ss[j].AttemptID })
	if c.EstimateWindow > 0 && len(ss) > c.EstimateWindow {
		ss = ss[:c.EstimateWindow]
	}
	n := len(ss)
	q := c.EstimateQuantile
	e := Estimate{Model: model, N: n, Quantile: q}
	if n < c.EstimateMinSamples || n == 0 {
		if c.EstimateColdTokens > 0 {
			t := c.EstimateColdTokens * c.EstimateSafetyFactor
			e.Tokens, e.TokensBasis = &t, "cold:config"
		} else {
			e.TokensBasis = "cold:unknown"
		}
		if c.EstimateColdDuration > 0 {
			d := c.EstimateColdDuration.Milliseconds()
			e.DurationMS, e.DurationBasis = &d, "cold:config"
		} else {
			e.DurationBasis = "unknown"
		}
		e.USDBasis = "cold:unknown"
		return e
	}
	tok := make([]float64, n)
	dur := make([]float64, n)
	usd := make([]float64, n)
	var perTurn []float64
	for i, s := range ss {
		tok[i] = s.Usage.Total()
		dur[i] = float64(s.DurationMS)
		usd[i] = s.CostUSD
		if s.NumTurns > 0 {
			perTurn = append(perTurn, tok[i]/float64(s.NumTurns))
		}
	}
	basis := historyBasis(q, n)
	est := Quantile(tok, q)
	if maxTurns > 0 && len(perTurn) >= c.EstimateMinSamples && len(perTurn) > 0 {
		if bound := float64(maxTurns) * Quantile(perTurn, q); bound < est {
			est = bound
			basis += "+turn_bound"
		}
	}
	e.Tokens, e.TokensBasis = &est, basis
	d := int64(math.Round(Quantile(dur, q)))
	e.DurationMS, e.DurationBasis = &d, historyBasis(q, n)
	u := Quantile(usd, q)
	e.USD, e.USDBasis = &u, historyBasis(q, n)
	return e
}
