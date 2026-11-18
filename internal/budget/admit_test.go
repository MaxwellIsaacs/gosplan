package budget

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Fixed clock for every admission test.
var tNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func fp(x float64) *float64 { return &x }
func ip(x int64) *int64     { return &x }
func tp(t time.Time) *time.Time {
	return &t
}

// tCfg is an enforce-mode config with every policy default and a typed
// unit fraction of 0.05.
func tCfg() Config {
	c := DefaultConfig()
	c.Mode = ModeEnforce
	c.ColdUnitFraction = 0.05
	return c
}

// tView is a fresh pool view observed a minute ago with the given headroom
// at the horizon and no fit (unit reserves).
func tView(pool string, headroom float64) PoolView {
	obs := tNow.Add(-time.Minute)
	return PoolView{
		Pool: pool, Level: fp(1 - headroom), ObservedAt: &obs, AgeSeconds: 60,
		Source: "rate_limit_event", Status: "allowed", HorizonSeconds: 7200,
		HorizonBasis: "max_horizon", Projected: 1 - headroom, Headroom: headroom,
		Fit: Fit{Pool: pool, Basis: "none:too_few_points"},
	}
}

func tCand(run, task, class string, models ...string) Candidate {
	return Candidate{Key: Key{Run: run, Task: task, Attempt: 1}, Class: class, Models: models,
		RunStartedAt: tNow.Add(-time.Hour), Tier: 1}
}

func tRun(run string) RunState { return RunState{Run: run, Mode: ModeEnforce} }

func tSnap(forKey Key, cands []Candidate, runs ...RunState) Snapshot {
	return Snapshot{Now: tNow, Mode: ModeEnforce, Config: tCfg(), Candidates: cands, Runs: runs, For: forKey}
}

func tOpen(id int64, pool string, reserve float64) Reservation {
	return Reservation{ID: id, Run: "other", Task: fmt.Sprintf("o%d", id), Enforced: true,
		Pools: []PoolReserve{{Pool: pool, Reserve: reserve, Basis: "unit"}}, CreatedAt: tNow.Add(-10 * time.Minute)}
}

func views(vs ...PoolView) map[string]PoolView {
	m := map[string]PoolView{}
	for _, v := range vs {
		m[v.Pool] = v
	}
	return m
}

// assertNotBefore checks NotBefore: in [target, target +
// jitter) where target = min(admitAt, now + recheck_max), and ≥ now + 1s.
func assertNotBefore(t *testing.T, d Decision, s Snapshot, target time.Time) {
	t.Helper()
	if d.NotBefore == nil {
		t.Fatalf("NotBefore nil on refusal (rule %s)", d.Rule)
	}
	nb := *d.NotBefore
	if nb.Before(s.Now.Add(time.Second)) {
		t.Fatalf("NotBefore %s < now + 1s", nb)
	}
	floor := target
	if floor.Before(s.Now.Add(time.Second)) {
		floor = s.Now.Add(time.Second)
	}
	if nb.Before(floor) || !nb.Before(target.Add(s.Config.Jitter)) && !nb.Equal(floor) {
		t.Fatalf("NotBefore %s not in [%s, %s)", nb, floor, target.Add(s.Config.Jitter))
	}
}

func TestBudgetAdmitRules(t *testing.T) {
	type tc struct {
		name    string
		build   func() (Snapshot, map[string]PoolView, map[string]Estimate)
		admit   bool
		rule    string
		reason  []string // substrings of Reason
		notes   []string // substrings of some note
		target  func(s Snapshot) time.Time
		breaker string
	}
	sonnet := Estimate{Model: "sonnet", TokensBasis: "cold:unknown"}
	ests := map[string]Estimate{"sonnet": sonnet}
	recheck := func(s Snapshot) time.Time { return s.Now.Add(s.Config.RecheckMax) }
	one := func(mod func(s *Snapshot, run *RunState), vs ...PoolView) func() (Snapshot, map[string]PoolView, map[string]Estimate) {
		return func() (Snapshot, map[string]PoolView, map[string]Estimate) {
			c := tCand("r1", "t1", ClassNormal, "sonnet")
			run := tRun("r1")
			s := tSnap(c.Key, []Candidate{c})
			if mod != nil {
				mod(&s, &run)
			}
			s.Runs = append(s.Runs, run)
			return s, views(vs...), ests
		}
	}
	cases := []tc{
		{
			name: "not a candidate",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				s := tSnap(Key{Run: "r1", Task: "ghost", Attempt: 2}, []Candidate{tCand("r1", "t1", ClassNormal, "sonnet")}, tRun("r1"))
				return s, views(tView("five_hour", 0.5)), ests
			},
			rule: RuleHalt, reason: []string{"r1/ghost attempt 2", "not a candidate"}, target: recheck,
		},
		{
			name:  "halt drain",
			build: one(func(s *Snapshot, r *RunState) { r.Halt, r.HaltReason = "drain", "cap:usd_estimate" }, tView("five_hour", 0.5)),
			rule:  RuleHalt, reason: []string{"run r1 is halted (drain)", "cap:usd_estimate"}, target: recheck,
		},
		{
			name:  "deadline passed",
			build: one(func(s *Snapshot, r *RunState) { r.Deadline = tp(tNow) }, tView("five_hour", 0.5)),
			rule:  RuleDeadline, reason: []string{"at or past the deadline", "2026-11-05T12:00:00Z"}, target: recheck,
		},
		{
			name: "deadline duration estimate overruns",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				s, v, _ := one(func(s *Snapshot, r *RunState) { r.Deadline = tp(tNow.Add(10 * time.Minute)) }, tView("five_hour", 0.5))()
				e := sonnet
				e.DurationMS, e.DurationBasis = ip(1_200_000), "p90"
				return s, v, map[string]Estimate{"sonnet": e}
			},
			rule: RuleDeadline, reason: []string{"duration estimate 1200s", "2026-11-05T12:20:00Z", "after the deadline 2026-11-05T12:10:00Z"}, target: recheck,
		},
		{
			name:  "deadline unknown duration admits with note",
			build: one(func(s *Snapshot, r *RunState) { r.Deadline = tp(tNow.Add(10 * time.Minute)) }, tView("five_hour", 0.5)),
			admit: true, rule: RuleAdmit, reason: []string{"five_hour"},
			notes: []string{"duration unknown; the deadline is enforced by stopping at the deadline"},
		},
		{
			name: "cap usd estimate with belt",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				s, v, _ := one(func(s *Snapshot, r *RunState) { r.CapUSD, r.SpentUSD = fp(5), 4.5 }, tView("five_hour", 0.5))()
				s.Config.BeltUSDFactor = fp(2)
				e := sonnet
				e.USD = fp(0.501)
				return s, v, map[string]Estimate{"sonnet": e}
			},
			rule: RuleCap, reason: []string{"spent 4.50", "belt 1.01", "= 5.51", "> cap_usd_estimate 5.00"}, target: recheck,
		},
		{
			name: "cap window",
			build: one(func(s *Snapshot, r *RunState) {
				r.CapWindow, r.Attributable = map[string]float64{"five_hour": 0.2}, map[string]float64{"five_hour": 0.18}
			}, tView("five_hour", 0.5)),
			rule: RuleCap, reason: []string{"pool five_hour", "attributable 0.1800", "reserve 0.0500 (unit)", "= 0.2300", "> cap_window 0.2000"}, target: recheck,
		},
		{
			name: "cap window on a pool that does not apply is ignored",
			build: one(func(s *Snapshot, r *RunState) {
				r.CapWindow, r.Attributable = map[string]float64{"week_fable": 0.1}, map[string]float64{"week_fable": 0.1}
			}, tView("five_hour", 0.5)),
			admit: true, rule: RuleAdmit, reason: []string{"five_hour"},
		},
		{
			name: "breaker retry storm on the run",
			build: one(func(s *Snapshot, r *RunState) {
				s.Breakers = []Breaker{{ID: 7, Kind: RetryStorm, Scope: ScopeRun, Run: "r1", State: "open", Count: 6, ClosesAt: tp(tNow.Add(2 * time.Minute)), Reason: "6 retries"}}
			}, tView("five_hour", 0.5)),
			rule: RuleBreaker, breaker: RetryStorm, reason: []string{"RETRY_STORM", "run r1", "count 6", "until 2026-11-05T12:02:00Z"},
			target: func(s Snapshot) time.Time { return tNow.Add(2 * time.Minute) },
		},
		{
			name: "breaker of another task does not block",
			build: one(func(s *Snapshot, r *RunState) {
				s.Breakers = []Breaker{{ID: 8, Kind: SameFailure, Scope: ScopeTask, Run: "r1", Task: "t9", State: "open"}}
			}, tView("five_hour", 0.5)),
			admit: true, rule: RuleAdmit, reason: []string{"five_hour"},
		},
		{
			name: "verifier error row never blocks",
			build: one(func(s *Snapshot, r *RunState) {
				s.Breakers = []Breaker{{ID: 9, Kind: VerifierError, Scope: ScopeRun, Run: "r1", State: "open"}}
			}, tView("five_hour", 0.5)),
			admit: true, rule: RuleAdmit,
		},
		{
			name:  "bootstrap admits one when nothing is open",
			build: one(nil),
			admit: true, rule: RuleBootstrap, reason: []string{"no pool has been observed", "bootstrap"},
		},
		{
			name:  "bootstrap refuses with an open reservation",
			build: one(func(s *Snapshot, r *RunState) { s.Open = []Reservation{{ID: 1, Run: "r2", Enforced: true}} }),
			rule:  RuleBootstrap, reason: []string{"no pool has been observed", "1 live open reservation(s) >= limit 1"},
			target: func(s Snapshot) time.Time { return s.Now.Add(s.Config.StaleRecheck) },
		},
		{
			name: "bootstrap counts only same-kind open reservations",
			build: one(func(s *Snapshot, r *RunState) {
				s.Open = []Reservation{{ID: 1, Run: "r2", Enforced: false}}
			}),
			admit: true, rule: RuleBootstrap,
		},
		{
			name: "bootstrap goes to the lowest estimate first",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				a := tCand("r1", "t1", ClassCritical, "opus")
				b := tCand("r1", "t2", ClassNormal, "haiku")
				s := tSnap(a.Key, []Candidate{a, b}, tRun("r1"))
				return s, nil, map[string]Estimate{"opus": {Model: "opus", Tokens: fp(300000)}, "haiku": {Model: "haiku", Tokens: fp(20000)}}
			},
			rule: RuleBootstrap, reason: []string{"r1/t2 is first in fairness order under this rule", "0 live open reservation(s) < limit 1"},
			target: func(s Snapshot) time.Time { return s.Now.Add(s.Config.StaleRecheck) },
		},
		{
			name: "no applicable pool is bootstrap",
			build: one(func(s *Snapshot, r *RunState) {
				s.Open = []Reservation{tOpen(1, "week_fable", 0.05)}
			}, tView("week_fable", 0.9)),
			rule: RuleBootstrap, reason: []string{`no observed pool applies to model "sonnet"`},
			target: func(s Snapshot) time.Time { return s.Now.Add(s.Config.StaleRecheck) },
		},
		{
			name: "stale refuses at stale_max_open",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				st := tView("seven_day", 0.9)
				st.Stale, st.StaleReason, st.AgeSeconds = true, "past_reset", 1500
				st2 := tView("five_hour", 0.9)
				st2.Stale, st2.StaleReason, st2.AgeSeconds = true, "age", 1300
				s, _, e := one(func(s *Snapshot, r *RunState) { s.Open = []Reservation{tOpen(1, "seven_day", 0.05)} })()
				return s, views(st, st2), e
			},
			rule:   RuleStale,
			reason: []string{"five_hour (age 1300s, age)", "seven_day (age 1500s, past_reset)", "1 live open reservation(s) >= limit 1"},
			target: func(s Snapshot) time.Time { return s.Now.Add(s.Config.StaleRecheck) },
		},
		{
			name: "stale admits below stale_max_open and still checks fresh pools",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				st := tView("seven_day", 0.9)
				st.Stale, st.StaleReason = true, "age"
				s, _, e := one(nil)()
				return s, views(st, tView("five_hour", 0.03)), e
			},
			rule: RuleHeadroom, reason: []string{"pool five_hour", "headroom 0.0300", "< reserve 0.0500"}, target: recheck,
		},
		{
			name: "headroom with a known reset",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				v := tView("five_hour", 0.06)
				v.ResetsAt, v.HorizonBasis = ip(tNow.Add(90*time.Second).Unix()), "reset"
				s, _, e := one(func(s *Snapshot, r *RunState) { s.Open = []Reservation{tOpen(1, "five_hour", 0.03)} })()
				return s, views(v, tView("seven_day", 0.5)), e
			},
			rule:   RuleHeadroom,
			reason: []string{"pool five_hour", "headroom 0.0600 (at horizon, reset)", "reserved 0.0300", "= 0.0300 < reserve 0.0500 (unit)"},
			target: func(s Snapshot) time.Time { return tNow.Add(90 * time.Second) },
		},
		{
			name:   "class hold: normal fits headroom but not the critical hold",
			build:  one(nil, tView("five_hour", 0.054)),
			rule:   RuleClassHold,
			reason: []string{"pool five_hour", "headroom 0.0540", "fits reserve 0.0500 (unit)", "hold 0.0054", `classes above "normal"`, "leaves 0.0486"},
			target: recheck,
		},
		{
			name: "class hold: best_effort holds critical + normal",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				c := tCand("r1", "t1", ClassBestEffort, "sonnet")
				return tSnap(c.Key, []Candidate{c}, tRun("r1")), views(tView("five_hour", 0.06)), ests
			},
			rule: RuleClassHold, reason: []string{"pool five_hour", "hold 0.0150", "leaves 0.0450"}, target: recheck,
		},
		{
			name: "critical has no hold",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				c := tCand("r1", "t1", ClassCritical, "sonnet")
				return tSnap(c.Key, []Candidate{c}, tRun("r1")), views(tView("five_hour", 0.05)), ests
			},
			admit: true, rule: RuleAdmit, reason: []string{"five_hour", "hold 0.0000"},
		},
		{
			name: "fairness: a critical candidate ahead takes the room",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				ahead := tCand("r2", "a1", ClassCritical, "sonnet")
				me := tCand("r1", "t1", ClassNormal, "sonnet")
				return tSnap(me.Key, []Candidate{me, ahead}, tRun("r1"), tRun("r2")), views(tView("five_hour", 0.08)), ests
			},
			rule:   RuleFairness,
			reason: []string{"pool five_hour", "headroom 0.0800", "ahead 0.0500", "hold 0.0080", "< reserve 0.0500", "fits alone"},
			target: func(s Snapshot) time.Time { return s.Now.Add(s.Config.FairnessRecheck) },
		},
		{
			name: "fairness: a candidate ahead that does not fit blocks (no backfill)",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				ahead := tCand("r2", "a1", ClassCritical, "fable")
				me := tCand("r1", "t1", ClassNormal, "sonnet")
				s := tSnap(me.Key, []Candidate{me, ahead}, tRun("r1"), tRun("r2"))
				return s, views(tView("five_hour", 0.5), tView("week_fable", 0.01)), map[string]Estimate{"sonnet": sonnet, "fable": {Model: "fable"}}
			},
			rule:   RuleFairness,
			reason: []string{"r2/a1 (class critical)", "does not fit", "strict priority"},
			target: func(s Snapshot) time.Time { return s.Now.Add(s.Config.FairnessRecheck) },
		},
		{
			name: "a halted run's candidate ahead takes nothing",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				ahead := tCand("r2", "a1", ClassCritical, "sonnet")
				me := tCand("r1", "t1", ClassNormal, "sonnet")
				r2 := tRun("r2")
				r2.Halt = "drain"
				return tSnap(me.Key, []Candidate{me, ahead}, tRun("r1"), r2), views(tView("five_hour", 0.08)), ests
			},
			admit: true, rule: RuleAdmit, reason: []string{"ahead 0.0000"},
		},
		{
			name: "admit reports every pool",
			build: func() (Snapshot, map[string]PoolView, map[string]Estimate) {
				s, _, e := one(nil)()
				return s, views(tView("seven_day", 0.7), tView("five_hour", 0.4)), e
			},
			admit: true, rule: RuleAdmit, reason: []string{"five_hour: headroom 0.4000", "seven_day: headroom 0.7000"},
			notes: []string{"every applicable pool uses unit reserves"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, v, e := c.build()
			d := Admit(s, v, e)
			if d.Admit != c.admit || d.Rule != c.rule {
				t.Fatalf("admit=%v rule=%q, want admit=%v rule=%q (reason %q)", d.Admit, d.Rule, c.admit, c.rule, d.Reason)
			}
			if d.Reason == "" {
				t.Fatal("empty reason")
			}
			for _, sub := range c.reason {
				if !strings.Contains(d.Reason, sub) {
					t.Errorf("reason %q lacks %q", d.Reason, sub)
				}
			}
			for _, sub := range c.notes {
				if !strings.Contains(strings.Join(d.Notes, "\n"), sub) {
					t.Errorf("notes %q lack %q", d.Notes, sub)
				}
			}
			if d.Breaker != c.breaker {
				t.Errorf("breaker %q, want %q", d.Breaker, c.breaker)
			}
			if d.For != s.For || len(d.Order) != len(s.Candidates) {
				t.Errorf("for %v order %v", d.For, d.Order)
			}
			if c.admit {
				if d.NotBefore != nil || d.AdmitAt != nil {
					t.Errorf("admit with NotBefore %v AdmitAt %v", d.NotBefore, d.AdmitAt)
				}
				if len(d.Reserve) != len(d.Pools) {
					t.Errorf("reserve %v for pools %d", d.Reserve, len(d.Pools))
				}
				return
			}
			if d.Reserve != nil {
				t.Errorf("refusal with reserve %v", d.Reserve)
			}
			// Every applicable pool's numbers on refusal (zero for bootstrap).
			pools, _ := newAdmission(s, v, e).applicable(d.Model)
			if len(d.Pools) != len(pools) {
				t.Errorf("pools %d, want %d", len(d.Pools), len(pools))
			}
			assertNotBefore(t, d, s, c.target(s))
		})
	}
}

func TestBudgetObserveEnforcesOnlyRunFlags(t *testing.T) {
	c := tCand("r1", "t1", ClassNormal, "sonnet")
	s := tSnap(c.Key, []Candidate{c}, tRun("r1"))
	s.Mode, s.Config.Mode = ModeObserve, ModeObserve
	full := views(tView("five_hour", 0.01))
	e := map[string]Estimate{"sonnet": {Model: "sonnet"}}

	d := Admit(s, full, e)
	if d.Admit || d.Rule != RuleHeadroom || d.NotBefore == nil {
		t.Fatalf("observe + full pool: admit=%v rule=%q not_before=%v, want the headroom refusal recorded", d.Admit, d.Rule, d.NotBefore)
	}
	if !strings.Contains(strings.Join(d.Notes, "\n"), "observe: rule headroom not enforced") {
		t.Fatalf("notes %q lack the not-enforced note", d.Notes)
	}
	if len(d.Reserve) != 1 || d.Reserve[0].Pool != "five_hour" {
		t.Fatalf("an unenforced refusal must carry its reservation (the gate starts the attempt): %v", d.Reserve)
	}
	// Observe-mode decisions count observe-mode reservations only.
	s.Open = []Reservation{tOpen(1, "five_hour", 0.05)} // enforced: ignored
	if d := Admit(s, views(tView("five_hour", 0.09)), e); d.Rule != RuleAdmit {
		t.Fatalf("observe counted an enforced reservation: rule %q %q", d.Rule, d.Reason)
	}
	s.Open[0].Enforced = false
	if d := Admit(s, views(tView("five_hour", 0.09)), e); d.Rule != RuleHeadroom || d.Admit {
		t.Fatalf("observe did not count its own reservation: admit=%v rule %q", d.Admit, d.Rule)
	}
	s.Open = nil

	// Breakers are not enforced in observe mode either.
	s.Breakers = []Breaker{{ID: 1, Kind: RateLimited, Scope: ScopeGlobal, State: "open"}}
	if d := Admit(s, views(tView("five_hour", 0.5)), e); d.Admit || d.Rule != RuleBreaker || len(d.Reserve) != 1 {
		t.Fatalf("observe + breaker: admit=%v rule %q", d.Admit, d.Rule)
	}
	s.Breakers = nil

	s.Runs[0].Deadline = tp(tNow.Add(-time.Minute))
	d = Admit(s, views(tView("five_hour", 0.5)), e)
	if d.Admit || d.Rule != RuleDeadline {
		t.Fatalf("observe + deadline: admit=%v rule=%q, want refusal", d.Admit, d.Rule)
	}
	if !strings.Contains(strings.Join(d.Notes, "\n"), "observe: run flag rule deadline enforced") || d.NotBefore == nil {
		t.Fatalf("notes %q notBefore %v", d.Notes, d.NotBefore)
	}
	if d.BeltUSD != nil {
		t.Fatalf("belt in observe mode: %v", *d.BeltUSD)
	}
}

func TestBudgetJitterDeterministicAndBounded(t *testing.T) {
	max := 30 * time.Second
	if Jitter("r|t|1|", max) != Jitter("r|t|1|", max) {
		t.Fatal("jitter not deterministic")
	}
	if Jitter("x", 0) != 0 || Jitter("x", -time.Second) != 0 {
		t.Fatal("non-positive max must give 0")
	}
	const n = 2000
	var sum time.Duration
	distinct := map[time.Duration]bool{}
	for i := 0; i < n; i++ {
		j := Jitter(fmt.Sprintf("run|task-%d|1|2026-11-05T12:00:00Z", i), max)
		if j < 0 || j >= max {
			t.Fatalf("jitter %s outside [0, %s)", j, max)
		}
		sum += j
		distinct[j] = true
	}
	mean := float64(sum) / n
	if mean < 0.45*float64(max) || mean > 0.55*float64(max) {
		t.Fatalf("mean %s not within 10%% of max/2", time.Duration(mean))
	}
	if len(distinct) < n/2 {
		t.Fatalf("only %d distinct jitters over %d keys", len(distinct), n)
	}
}

func TestBudgetBeltRoundsUp(t *testing.T) {
	cases := []struct {
		usd, factor float64
		want        float64
	}{{0.501, 2, 1.01}, {0.5, 2, 1.00}, {0.07, 1, 0.07}, {0.0001, 1, 0.01}, {1.234, 1.5, 1.86}}
	for _, c := range cases {
		b := Belt(Estimate{USD: fp(c.usd)}, c.factor)
		if b == nil || *b != c.want {
			t.Errorf("Belt(%v × %v) = %v, want %v", c.usd, c.factor, b, c.want)
		}
	}
	if Belt(Estimate{}, 2) != nil {
		t.Error("belt with unknown USD must be nil")
	}
	if Belt(Estimate{USD: fp(1)}, 0) != nil {
		t.Error("belt with factor 0 must be nil")
	}
	// Enforce decisions carry the belt only when usd_factor is set.
	c := tCand("r1", "t1", ClassNormal, "sonnet")
	s := tSnap(c.Key, []Candidate{c}, tRun("r1"))
	e := map[string]Estimate{"sonnet": {Model: "sonnet", USD: fp(0.501)}}
	if d := Admit(s, views(tView("five_hour", 0.5)), e); d.BeltUSD != nil {
		t.Errorf("belt without usd_factor: %v", *d.BeltUSD)
	}
	s.Config.BeltUSDFactor = fp(2)
	if d := Admit(s, views(tView("five_hour", 0.5)), e); d.BeltUSD == nil || *d.BeltUSD != 1.01 {
		t.Errorf("enforce belt = %v, want 1.01", d.BeltUSD)
	}
}

func TestBudgetRatioBounds(t *testing.T) {
	cfg := tCfg()
	a := &admission{cfg: cfg}
	fit := func(k float64) PoolView {
		v := tView("five_hour", 0.5)
		v.Fit = Fit{Pool: "five_hour", Basis: "fit", KPerToken: k}
		return v
	}
	cases := []struct {
		name  string
		v     PoolView
		e     Estimate
		want  float64
		basis string
	}{
		{"ratio", fit(1e-7), Estimate{Tokens: fp(200000)}, 0.02, "ratio"},
		{"k = 0", fit(0), Estimate{Tokens: fp(200000)}, 0.05, "unit"},
		{"k × est > 1", fit(1e-5), Estimate{Tokens: fp(200000)}, 0.05, "unit"},
		{"k × est = 1 is still ratio", fit(5e-6), Estimate{Tokens: fp(200000)}, 1, "ratio"},
		{"estimate unknown", fit(1e-7), Estimate{}, 0.05, "unit"},
		{"fit failed its gate", func() PoolView { v := fit(1e-7); v.Fit.Basis = "none:poor_fit"; return v }(), Estimate{Tokens: fp(200000)}, 0.05, "unit"},
	}
	for _, c := range cases {
		r, basis := a.reserveFor(c.v, c.e)
		if basis != c.basis || r < c.want-1e-12 || r > c.want+1e-12 {
			t.Errorf("%s: reserve %v (%s), want %v (%s)", c.name, r, basis, c.want, c.basis)
		}
	}
	// Through Admit: the decision's reservation carries the basis.
	cd := tCand("r1", "t1", ClassCritical, "sonnet")
	s := tSnap(cd.Key, []Candidate{cd}, tRun("r1"))
	d := Admit(s, views(fit(0)), map[string]Estimate{"sonnet": {Model: "sonnet", Tokens: fp(200000)}})
	if !d.Admit || len(d.Reserve) != 1 || d.Reserve[0].Basis != "unit" || d.Reserve[0].Reserve != 0.05 {
		t.Fatalf("k = 0 reservation %v (rule %s)", d.Reserve, d.Rule)
	}
	d = Admit(s, views(fit(1e-7)), map[string]Estimate{"sonnet": {Model: "sonnet", Tokens: fp(200000)}})
	if !d.Admit || d.Reserve[0].Basis != "ratio" || d.Pools[0].ReserveBasis != "ratio" {
		t.Fatalf("ratio reservation %v", d.Reserve)
	}
}

func TestBudgetRecentSettled(t *testing.T) {
	cd := tCand("r1", "t1", ClassCritical, "sonnet")
	s := tSnap(cd.Key, []Candidate{cd}, tRun("r1"))
	v := tView("five_hour", 0.07) // observed at now − 1m
	v.Fit = Fit{Pool: "five_hour", Basis: "fit", KPerToken: 1e-7}
	e := map[string]Estimate{"sonnet": {Model: "sonnet"}} // unknown: unit 0.05
	settled := func(at time.Time, actual *float64) Reservation {
		r := tOpen(1, "five_hour", 0.03)
		r.SettledAt, r.ActualTokens = &at, actual
		return r
	}
	// Settled before the newest observation: already in the level.
	s.Recent = []Reservation{settled(tNow.Add(-2*time.Minute), nil)}
	if d := Admit(s, views(v), e); !d.Admit || d.Pools[0].OpenReserved != 0 {
		t.Fatalf("settled before observation counted: rule %s open %v", d.Rule, d.Pools[0].OpenReserved)
	}
	// Settled after it, actual unknown: counted at its reserve.
	s.Recent = []Reservation{settled(tNow.Add(-30*time.Second), nil)}
	d := Admit(s, views(v), e)
	if d.Admit || d.Rule != RuleHeadroom || d.Pools[0].OpenReserved != 0.03 || !strings.Contains(d.Reason, "reserved 0.0300") {
		t.Fatalf("settled after observation: rule %s open %v reason %q", d.Rule, d.Pools[0].OpenReserved, d.Reason)
	}
	// Settled after it with a known actual: max(k × actual, reserve).
	s.Recent = []Reservation{settled(tNow.Add(-30*time.Second), fp(600000))} // k × actual = 0.06
	if d := Admit(s, views(v), e); d.Pools[0].OpenReserved < 0.06-1e-12 || d.Pools[0].OpenReserved > 0.06+1e-12 {
		t.Fatalf("k × actual not used: %v", d.Pools[0].OpenReserved)
	}
	s.Recent = []Reservation{settled(tNow.Add(-30*time.Second), fp(1000))} // k × actual = 0.0001 < reserve
	if d := Admit(s, views(v), e); d.Pools[0].OpenReserved != 0.03 {
		t.Fatalf("reserve floor not used: %v", d.Pools[0].OpenReserved)
	}
}

func TestBudgetNotBeforeFloor(t *testing.T) {
	cd := tCand("r1", "t1", ClassNormal, "sonnet")
	e := map[string]Estimate{"sonnet": {Model: "sonnet"}}

	// admit_at unknown (headroom, no reset) → now + recheck_max + jitter.
	s := tSnap(cd.Key, []Candidate{cd}, tRun("r1"))
	d := Admit(s, views(tView("five_hour", 0.01)), e)
	if d.AdmitAt != nil {
		t.Fatalf("admit_at %v, want unknown", d.AdmitAt)
	}
	want := tNow.Add(s.Config.RecheckMax).Add(Jitter("r1|t1|1|", s.Config.Jitter))
	if !d.NotBefore.Equal(want) {
		t.Fatalf("NotBefore %s, want %s", d.NotBefore, want)
	}
	// A reset far beyond recheck_max is capped at recheck_max.
	v := tView("five_hour", 0.01)
	v.ResetsAt = ip(tNow.Add(3 * time.Hour).Unix())
	d = Admit(s, views(v), e)
	if d.AdmitAt == nil || !d.AdmitAt.Equal(tNow.Add(3*time.Hour)) {
		t.Fatalf("admit_at %v, want the reset", d.AdmitAt)
	}
	assertNotBefore(t, d, s, tNow.Add(s.Config.RecheckMax))

	// A breaker that should already have closed: admit_at in the past → floor.
	s.Breakers = []Breaker{{ID: 1, Kind: RateLimited, Scope: ScopeGlobal, State: "open", ClosesAt: tp(tNow.Add(-time.Hour))}}
	d = Admit(s, views(tView("five_hour", 0.5)), e)
	if d.Rule != RuleBreaker || !d.NotBefore.Equal(tNow.Add(time.Second)) {
		t.Fatalf("rule %s NotBefore %v, want now + 1s", d.Rule, d.NotBefore)
	}
	// Zero recheck intervals and no jitter still give now + 1s.
	s.Breakers = nil
	s.Config.Jitter, s.Config.FairnessRecheck, s.Config.RecheckMax = 0, 0, 0
	d = Admit(s, views(tView("five_hour", 0.01)), e)
	if !d.NotBefore.Equal(tNow.Add(time.Second)) {
		t.Fatalf("NotBefore %v, want now + 1s", d.NotBefore)
	}
}

func TestBudgetOrder(t *testing.T) {
	cfg := tCfg()
	base := tNow.Add(-time.Hour)
	mk := func(run, task, class string, start time.Time, tier int) Candidate {
		c := tCand(run, task, class, "sonnet")
		c.RunStartedAt, c.Tier = start, tier
		return c
	}
	cands := []Candidate{
		mk("rB", "z", ClassNormal, base, 1),
		mk("rA", "b", ClassNormal, base, 2),
		mk("rA", "a", ClassNormal, base, 2),
		mk("rA", "c", ClassNormal, base, 1),
		mk("rC", "x", ClassNormal, base.Add(-time.Minute), 9),
		mk("rA", "e", ClassBestEffort, base, 0),
		mk("rB", "k", ClassCritical, base, 5),
	}
	runs := []RunState{
		{Run: "rA", SettledTokens: map[string]float64{"five_hour": 100}},
		{Run: "rB", SettledTokens: map[string]float64{"five_hour": 500}},
		{Run: "rC", SettledTokens: map[string]float64{"five_hour": 500}},
	}
	got := Order(cands, runs, cfg)
	var ids []string
	for _, c := range got {
		ids = append(ids, c.Run+"/"+c.Task)
	}
	// critical first; then normal by settled tokens (rA 100 before rB/rC
	// 500), then run start (rC earlier), tier, task id; best_effort last.
	want := "rB/k rA/c rA/a rA/b rC/x rB/z rA/e"
	if strings.Join(ids, " ") != want {
		t.Fatalf("order %s, want %s", strings.Join(ids, " "), want)
	}
	if len(cands) != 7 || cands[0].Task != "z" {
		t.Fatal("Order mutated its input")
	}

	// With views, key 2 uses the binding pool (earliest known reset).
	vs := views(tView("five_hour", 0.5), tView("seven_day", 0.5))
	fh, sd := vs["five_hour"], vs["seven_day"]
	fh.ResetsAt, sd.ResetsAt = ip(tNow.Add(3*time.Hour).Unix()), ip(tNow.Add(time.Hour).Unix())
	vs["five_hour"], vs["seven_day"] = fh, sd
	r1 := []RunState{
		{Run: "rA", SettledTokens: map[string]float64{"five_hour": 10, "seven_day": 900}},
		{Run: "rB", SettledTokens: map[string]float64{"five_hour": 800, "seven_day": 20}},
	}
	two := []Candidate{mk("rA", "a", ClassNormal, base, 1), mk("rB", "b", ClassNormal, base, 1)}
	if o := orderWith(two, r1, cfg, vs, tNow); o[0].Run != "rB" {
		t.Fatalf("binding pool seven_day (earliest reset) not used: first %s", o[0].Run)
	}
	if o := Order(two, r1, cfg); o[0].Run != "rB" { // max: rA 900, rB 800
		t.Fatalf("without views: first %s, want rB", o[0].Run)
	}
	// Admit's Order field is the fairness order of all candidates.
	s := tSnap(two[0].Key, two, tRun("rA"), tRun("rB"))
	s.Runs = r1
	d := Admit(s, vs, map[string]Estimate{})
	if len(d.Order) != 2 || d.Order[0] != two[1].Key {
		t.Fatalf("decision order %v", d.Order)
	}
}
