package budget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// Property tests for Admit. Seeded generators (math/rand is
// allowed in _test.go files only); the pool views and estimates are built
// by hand, so nothing here depends on the view/estimate code of 2.a.

const propCases = 600

type propCase struct {
	S Snapshot            `json:"s"`
	V map[string]PoolView `json:"v"`
	E map[string]Estimate `json:"e"`
}

type genOpts struct {
	allStale  bool    // every pool stale (and at least one pool)
	staleProb float64 // per-pool stale probability otherwise
}

var propPools = []string{"five_hour", "seven_day", "week_fable", "session"}

func genView(r *rand.Rand, pool string, stale bool) PoolView {
	h := r.Float64() * 0.35
	v := tView(pool, h)
	obs := tNow.Add(-time.Duration(r.Int63n(int64(15 * time.Minute))))
	v.ObservedAt, v.AgeSeconds = &obs, tNow.Sub(obs).Seconds()
	if r.Intn(2) == 0 {
		v.ResetsAt = ip(tNow.Add(time.Minute + time.Duration(r.Int63n(int64(5*time.Hour)))).Unix())
		v.HorizonBasis = "reset"
	}
	if r.Intn(2) == 0 {
		v.Fit = Fit{Pool: pool, Basis: "fit", KPerToken: r.Float64() * 1e-6, N: 10}
	}
	if stale {
		v.Stale = true
		v.StaleReason = []string{"age", "past_reset", "no_rows"}[r.Intn(3)]
	}
	return v
}

func genReservation(r *rand.Rand, id int64, pools []string) Reservation {
	res := Reservation{ID: id, Run: fmt.Sprintf("x%d", r.Intn(3)), Task: fmt.Sprintf("o%d", id), Enforced: r.Float64() < 0.9,
		CreatedAt: tNow.Add(-time.Duration(r.Int63n(int64(time.Hour))))}
	for _, p := range pools {
		if r.Intn(3) > 0 {
			res.Pools = append(res.Pools, PoolReserve{Pool: p, Reserve: r.Float64() * 0.08, Basis: "unit"})
		}
	}
	return res
}

func genCase(r *rand.Rand, o genOpts) propCase {
	cfg := tCfg()
	cfg.RoutingEnabled = r.Intn(2) == 0
	cfg.StaleMaxOpen = 1 + r.Intn(3)
	vs := map[string]PoolView{}
	var pools []string
	for _, p := range propPools {
		if r.Intn(2) == 0 {
			pools = append(pools, p)
		}
	}
	if len(pools) == 0 && (o.allStale || r.Float64() < 0.9) {
		pools = append(pools, propPools[r.Intn(len(propPools))])
	}
	for _, p := range pools {
		vs[p] = genView(r, p, o.allStale || r.Float64() < o.staleProb)
	}
	est := map[string]Estimate{}
	for _, m := range []string{"sonnet", "fable"} {
		e := Estimate{Model: m, TokensBasis: "cold:unknown"}
		if r.Float64() < 0.7 {
			e.Tokens, e.TokensBasis = fp(1e4+r.Float64()*4e5), "p90"
		}
		est[m] = e
	}
	nRuns := 1 + r.Intn(3)
	var runs []RunState
	for i := 0; i < nRuns; i++ {
		rs := RunState{Run: fmt.Sprintf("r%d", i), Mode: ModeEnforce, SettledTokens: map[string]float64{}}
		for _, p := range pools {
			rs.SettledTokens[p] = float64(r.Intn(4)) * 1e5
		}
		if r.Float64() < 0.05 {
			rs.Halt = "drain"
		}
		runs = append(runs, rs)
	}
	modelSets := [][]string{{"sonnet"}, {"fable"}, {"sonnet", "fable"}, {"fable", "sonnet"}}
	classes := []string{ClassCritical, ClassNormal, ClassBestEffort}
	nC := 1 + r.Intn(6)
	var cands []Candidate
	for i := 0; i < nC; i++ {
		c := Candidate{
			Key:          Key{Run: fmt.Sprintf("r%d", r.Intn(nRuns)), Task: fmt.Sprintf("t%d", i), Attempt: 1 + r.Intn(2)},
			Class:        classes[r.Intn(3)],
			Models:       modelSets[r.Intn(len(modelSets))],
			RunStartedAt: tNow.Add(-time.Duration(r.Intn(4)) * time.Hour),
			Tier:         r.Intn(3),
		}
		cands = append(cands, c)
	}
	s := Snapshot{Now: tNow, Mode: ModeEnforce, Config: cfg, Candidates: cands, Runs: runs, For: cands[0].Key}
	for i := r.Intn(4); i > 0; i-- {
		s.Open = append(s.Open, genReservation(r, int64(100+i), pools))
	}
	for i := r.Intn(3); i > 0; i-- {
		rs := genReservation(r, int64(200+i), pools)
		at := tNow.Add(-time.Duration(r.Int63n(int64(10 * time.Minute))))
		rs.SettledAt = &at
		if r.Intn(2) == 0 {
			rs.ActualTokens = fp(r.Float64() * 3e5)
		}
		s.Recent = append(s.Recent, rs)
	}
	if r.Float64() < 0.1 {
		s.Breakers = []Breaker{{ID: 1, Kind: RetryStorm, Scope: ScopeRun, Run: runs[r.Intn(nRuns)].Run, State: "open", ClosesAt: tp(tNow.Add(time.Minute))}}
	}
	return propCase{S: s, V: vs, E: est}
}

func cloneViews(v map[string]PoolView) map[string]PoolView {
	out := make(map[string]PoolView, len(v))
	for k, x := range v {
		out[k] = x
	}
	return out
}

func sortedKeys(v map[string]PoolView) []string { return sortedPools(v) }

// raise lowers pool p's projection by delta (raising its headroom).
func raise(v PoolView, delta float64) PoolView {
	v.Projected -= delta
	v.Headroom += delta
	if v.Level != nil {
		v.Level = fp(*v.Level - delta)
	}
	return v
}

func TestBudgetPropertyMonotone(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	var admits, refusals, raised, added int
	for i := 0; i < propCases; i++ {
		g := genCase(r, genOpts{staleProb: 0.15})
		for _, c := range g.S.Candidates {
			s := g.S
			s.For = c.Key
			d := Admit(s, g.V, g.E)
			if d.Admit {
				admits++
				if len(g.V) == 0 {
					continue
				}
				// Raise headroom of one pool, stale status unchanged.
				keys := sortedKeys(g.V)
				p := keys[r.Intn(len(keys))]
				v2 := cloneViews(g.V)
				v2[p] = raise(v2[p], r.Float64()*v2[p].Projected)
				raised++
				if d2 := Admit(s, v2, g.E); !d2.Admit {
					t.Fatalf("case %d for %v: raising %s headroom %.4f -> %.4f turned admit into %s: %s",
						i, c.Key, p, g.V[p].Headroom, v2[p].Headroom, d2.Rule, d2.Reason)
				}
				continue
			}
			refusals++
			// Add an open reservation of this decision's kind.
			var pools []string
			pools = append(pools, sortedKeys(g.V)...)
			extra := genReservation(r, 999, pools)
			extra.Enforced = true
			s.Open = append(append([]Reservation(nil), s.Open...), extra)
			added++
			if d2 := Admit(s, g.V, g.E); d2.Admit {
				t.Fatalf("case %d for %v: adding open reservation %+v turned refusal (%s) into admit", i, c.Key, extra.Pools, d.Rule)
			}
		}
	}
	if admits < 200 || refusals < 200 || raised < 100 || added < 100 {
		t.Fatalf("generator too narrow: admits %d refusals %d raised %d added %d", admits, refusals, raised, added)
	}
}

// TestBudgetPropertyMonotoneStaleException is the documented case: a
// level drop that splits a segment turns a past_reset (stale) pool fresh,
// and the stale rule ignored the stale pool's level, so the decision can
// tighten. The generator makes both kinds of change; only the flip may
// violate monotonicity, and it must actually be found.
func TestBudgetPropertyMonotoneStaleException(t *testing.T) {
	// math.md's counterexample, by hand: alias session = five_hour, rows 0.97
	// then 0.99 past the reset admit under stale; lowering the second to 0.95
	// splits the segment, the pool is fresh, and it refuses.
	c := tCand("r1", "t1", ClassNormal, "sonnet")
	s := tSnap(c.Key, []Candidate{c}, tRun("r1"))
	st := tView("five_hour", 0.01)
	st.Stale, st.StaleReason = true, "past_reset"
	e := map[string]Estimate{"sonnet": {Model: "sonnet"}}
	if d := Admit(s, views(st), e); !d.Admit || d.Rule != RuleStale {
		t.Fatalf("stale pool, nothing open: admit=%v rule %s, want an admit under rule stale", d.Admit, d.Rule)
	}
	fresh := raise(st, 0.03)
	fresh.Stale, fresh.StaleReason = false, ""
	if d := Admit(s, views(fresh), e); d.Admit || d.Rule != RuleHeadroom {
		t.Fatalf("after the split: rule %s, want headroom", d.Rule)
	}

	r := rand.New(rand.NewSource(1))
	flips, violations := 0, 0
	for i := 0; i < propCases; i++ {
		g := genCase(r, genOpts{staleProb: 0.5})
		for _, p := range sortedKeys(g.V) { // sorted: map order must not steer r
			if v := g.V[p]; v.Stale && r.Intn(2) == 0 {
				h := r.Float64() * 0.06 // nearly full, past its reset
				v.StaleReason, v.Headroom, v.Projected, v.Level = "past_reset", h, 1-h, fp(1-h)
				g.V[p] = v
			}
		}
		keys := sortedKeys(g.V)
		if len(keys) == 0 {
			continue
		}
		p := keys[r.Intn(len(keys))]
		v2 := cloneViews(g.V)
		v2[p] = raise(v2[p], r.Float64()*0.05)
		flip := g.V[p].Stale && g.V[p].StaleReason == "past_reset" && r.Intn(2) == 0
		if flip {
			v2[p] = func(v PoolView) PoolView { v.Stale, v.StaleReason = false, ""; return v }(v2[p])
			flips++
		}
		for _, c := range g.S.Candidates {
			s := g.S
			s.For = c.Key
			if !Admit(s, g.V, g.E).Admit || Admit(s, v2, g.E).Admit {
				continue
			}
			if !flip {
				t.Fatalf("case %d for %v: violation without a stale->fresh flip of %s", i, c.Key, p)
			}
			violations++
		}
	}
	if flips < 50 || violations == 0 {
		t.Fatalf("documented exception not exercised: flips %d violations %d", flips, violations)
	}
}

func TestBudgetPropertyConservation(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	admitted := 0
	for i := 0; i < propCases; i++ {
		g := genCase(r, genOpts{staleProb: 0.15})
		s := g.S
		s.Open = append([]Reservation(nil), s.Open...)
		initOpen := map[string]float64{}
		for _, o := range s.Open {
			if o.Enforced {
				for _, pr := range o.Pools {
					initOpen[pr.Pool] += pr.Reserve
				}
			}
		}
		order := Admit(s, g.V, g.E).Order
		for _, k := range order {
			s.For = k
			d := Admit(s, g.V, g.E)
			if !d.Admit {
				continue
			}
			admitted++
			for _, pr := range d.Pools {
				if !pr.Stale && pr.OpenReserved+pr.Ahead+pr.Reserve > pr.Headroom+1e-9 {
					t.Fatalf("case %d %v: pool %s open %.4f + ahead %.4f + reserve %.4f > headroom %.4f",
						i, k, pr.Pool, pr.OpenReserved, pr.Ahead, pr.Reserve, pr.Headroom)
				}
			}
			s.Open = append(s.Open, Reservation{ID: int64(1000 + len(s.Open)), Run: k.Run, Task: k.Task, Enforced: true, Pools: d.Reserve})
			var rest []Candidate
			for _, c := range s.Candidates {
				if c.Key != k {
					rest = append(rest, c)
				}
			}
			s.Candidates = rest
		}
		final := map[string]float64{}
		for _, o := range s.Open {
			if o.Enforced {
				for _, pr := range o.Pools {
					final[pr.Pool] += pr.Reserve
				}
			}
		}
		for p, v := range g.V {
			if v.Stale {
				continue
			}
			limit := v.Headroom
			if initOpen[p] > limit {
				limit = initOpen[p]
			}
			if final[p] > limit+1e-9 {
				t.Fatalf("case %d: pool %s open after the walk %.4f > max(headroom %.4f, initial open %.4f)", i, p, final[p], v.Headroom, initOpen[p])
			}
		}
	}
	if admitted < 200 {
		t.Fatalf("generator too narrow: %d admits", admitted)
	}
}

func TestBudgetPropertyStaleNoFlood(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	admits, atLimit := 0, 0
	for i := 0; i < propCases; i++ {
		g := genCase(r, genOpts{allStale: true})
		s := g.S
		s.Open = nil
		for j := r.Intn(5); j > 0; j-- {
			o := genReservation(r, int64(j), sortedKeys(g.V))
			o.Enforced = true
			s.Open = append(s.Open, o)
		}
		for _, c := range s.Candidates {
			s.For = c.Key
			d := Admit(s, g.V, g.E)
			if len(s.Open) == s.Config.StaleMaxOpen {
				atLimit++
			}
			if !d.Admit {
				continue
			}
			admits++
			// Every pool is stale, so an admit is under the stale rule, or
			// under bootstrap when no pool applies to the model.
			if d.Rule != RuleStale && d.Rule != RuleBootstrap {
				t.Fatalf("case %d: admitted with rule %s, want stale or bootstrap", i, d.Rule)
			}
			if len(s.Open) >= s.Config.StaleMaxOpen {
				t.Fatalf("case %d %v: admitted with %d open >= stale_max_open %d (%s)", i, c.Key, len(s.Open), s.Config.StaleMaxOpen, d.Reason)
			}
		}
	}
	if admits < 50 || atLimit < 50 {
		t.Fatalf("generator too narrow: admits %d at-limit decisions %d", admits, atLimit)
	}
}

func TestBudgetPropertyDecisionRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < propCases; i++ {
		g := genCase(r, genOpts{staleProb: 0.2})
		g.S.For = g.S.Candidates[r.Intn(len(g.S.Candidates))].Key
		if r.Intn(2) == 0 {
			g.S.Mode = ModeObserve
		}
		b, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		var rt propCase
		if err := json.Unmarshal(b, &rt); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		d1, _ := json.Marshal(Admit(g.S, g.V, g.E))
		d2, _ := json.Marshal(Admit(g.S, g.V, g.E))
		d3, _ := json.Marshal(Admit(rt.S, rt.V, rt.E))
		if !bytes.Equal(d1, d2) {
			t.Fatalf("case %d: Admit not deterministic:\n%s\n%s", i, d1, d2)
		}
		if !bytes.Equal(d1, d3) {
			t.Fatalf("case %d: Admit differs after a JSON round trip:\n%s\n%s", i, d1, d3)
		}
	}
}
