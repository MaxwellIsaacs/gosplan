package budget

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strings"
	"time"
)

// Admission. Everything here is pure: the
// decision is a function of the Snapshot, the pool views and the estimates.
// Rule order: halt, deadline, cap, breaker, bootstrap/stale,
// class_hold, headroom, fairness. An attempt admitted under the bootstrap or
// stale rule carries that rule, with Admit = true. In observe
// mode only the run flags (halt, deadline, cap) are enforced;
// every other rule's outcome is recorded as it is (Admit = false on a
// would-be refusal) with a note, and the gate proceeds without applying it.

// Order sorts candidates by class rank (desc), the run's settled tokens in
// the binding pool's current segment (asc), run start, tier, task id;
// run id and attempt break any remaining tie. Without pool views the
// binding pool is unknown, so key 2 is the largest SettledTokens entry of the
// run (Admit orders with its views; see orderWith). A class missing from the
// config ranks 0, below every configured class (policy).
func Order(cands []Candidate, runs []RunState, c Config) []Candidate {
	return orderWith(cands, runs, c, nil, time.Time{})
}

// orderWith is Order with pool views: a candidate's binding pool is the
// applicable pool (for its first model) with the earliest known reset after
// now; with none known, key 2 is the largest SettledTokens entry among the
// applicable pools (all of the run's pools when none applies).
func orderWith(cands []Candidate, runs []RunState, c Config, views map[string]PoolView, now time.Time) []Candidate {
	byRun := make(map[string]RunState, len(runs))
	for _, r := range runs {
		byRun[r.Run] = r
	}
	type keyed struct {
		c    Candidate
		rank int
		used float64
	}
	ks := make([]keyed, len(cands))
	for i, cd := range cands {
		rank, _ := c.ClassRank(cd.Class)
		ks[i] = keyed{c: cd, rank: rank, used: settledUse(cd, byRun[cd.Run], c, views, now)}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		a, b := ks[i], ks[j]
		switch {
		case a.rank != b.rank:
			return a.rank > b.rank
		case a.used != b.used:
			return a.used < b.used
		case !a.c.RunStartedAt.Equal(b.c.RunStartedAt):
			return a.c.RunStartedAt.Before(b.c.RunStartedAt)
		case a.c.Tier != b.c.Tier:
			return a.c.Tier < b.c.Tier
		case a.c.Task != b.c.Task:
			return a.c.Task < b.c.Task
		case a.c.Run != b.c.Run:
			return a.c.Run < b.c.Run
		}
		return a.c.Attempt < b.c.Attempt
	})
	out := make([]Candidate, len(ks))
	for i, k := range ks {
		out[i] = k.c
	}
	return out
}

// settledUse is fairness key 2 for cd.
func settledUse(cd Candidate, rs RunState, c Config, views map[string]PoolView, now time.Time) float64 {
	model := firstModel(cd)
	var pools []string
	if views != nil {
		var best string
		var bestReset int64
		for _, p := range sortedPools(views) {
			if !poolApplies(p, model, c.ModelFamilies) {
				continue
			}
			pools = append(pools, p)
			if ra := views[p].ResetsAt; ra != nil && *ra > now.Unix() && (best == "" || *ra < bestReset) {
				best, bestReset = p, *ra
			}
		}
		if best != "" {
			return rs.SettledTokens[best]
		}
	}
	if len(pools) == 0 {
		for p := range rs.SettledTokens {
			pools = append(pools, p)
		}
	}
	used := 0.0
	for _, p := range pools {
		used = math.Max(used, rs.SettledTokens[p])
	}
	return used
}

func firstModel(c Candidate) string {
	if len(c.Models) == 0 {
		return ""
	}
	return c.Models[0]
}

func sortedPools(views map[string]PoolView) []string {
	out := make([]string, 0, len(views))
	for p := range views {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// poolApplies: a pool applies to every model unless
// its name ends with "_<family>" for a configured family; then it applies
// only to models whose name contains the family (case-insensitive). Admit
// uses this directly rather than pools.go's Applies so that admission is
// decided by one known rule; both implement the same rule.
func poolApplies(pool, model string, families []string) bool {
	lp, lm := strings.ToLower(pool), strings.ToLower(model)
	for _, f := range families {
		lf := strings.ToLower(f)
		if lf != "" && strings.HasSuffix(lp, "_"+lf) {
			return strings.Contains(lm, lf)
		}
	}
	return true
}

// Jitter is a deterministic delay in [0, max) derived from key:
// max × (first 8 bytes of sha256(key) as a big-endian uint64) / 2^64. max ≤ 0
// gives 0.
func Jitter(key string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(key))
	hi, _ := bits.Mul64(binary.BigEndian.Uint64(sum[:8]), uint64(max))
	return time.Duration(hi)
}

// Belt is the --max-budget-usd belt for an estimate: the USD estimate times
// factor, rounded up to cents; nil when the USD estimate is unknown or the
// factor is not positive. It is a client-side list-price
// estimate and does not cap plan usage.
func Belt(e Estimate, factor float64) *float64 {
	if e.USD == nil || factor <= 0 {
		return nil
	}
	// The 1e-9 guards against binary-float noise (0.07×100 = 7.000000000000001)
	// rounding a whole number of cents up by one.
	v := math.Ceil(*e.USD*factor*100-1e-9) / 100
	return &v
}

// Admit decides for s.For given precomputed pool views and per-model
// estimates. Steps: find the candidate (admission.found), the
// run rules (checkHalt, checkDeadline, checkCap, checkBreaker), the unknown-
// data rules (checkUnknown), the pool check with the fairness walk
// (checkPools, walkAhead), routing (route) and the recheck time (finish,
// notBefore).
func Admit(s Snapshot, views map[string]PoolView, est map[string]Estimate) Decision {
	a := newAdmission(s, views, est)
	models := Models(a.cand, a.cfg.RoutingEnabled, "")
	d, tried := route(models, func(m string) Decision { return a.decideModel(m) })
	if len(models) > 1 {
		if d.Admit {
			d.Notes = append(d.Notes, fmt.Sprintf("routing: tried %s; admitted %s", strings.Join(tried, ", "), d.Model))
		} else {
			d.Notes = append(d.Notes, fmt.Sprintf("routing: tried %s; none admitted, reporting %s", strings.Join(tried, ", "), d.Model))
		}
	}
	return a.finish(d)
}

// admission is the shared state of one Admit call.
type admission struct {
	s       Snapshot
	cfg     Config
	views   map[string]PoolView
	est     map[string]Estimate
	pools   []string // sorted view names
	enforce bool
	runs    map[string]RunState
	order   []Candidate
	cand    Candidate // s.For's candidate (zero when not found)
	run     RunState  // s.For's run (zero when not found)
	found   bool      // both the candidate and its run exist
}

func newAdmission(s Snapshot, views map[string]PoolView, est map[string]Estimate) *admission {
	a := &admission{
		s: s, cfg: s.Config, views: views, est: est,
		pools:   sortedPools(views),
		enforce: s.Mode == ModeEnforce,
		runs:    make(map[string]RunState, len(s.Runs)),
	}
	for _, r := range s.Runs {
		a.runs[r.Run] = r
	}
	a.order = orderWith(s.Candidates, s.Runs, a.cfg, views, s.Now)
	// Step 1: s.For's candidate and run state.
	var candOK bool
	for _, c := range s.Candidates {
		if c.Key == s.For {
			a.cand, candOK = c, true
			break
		}
	}
	a.run, a.found = a.runs[s.For.Run]
	a.found = a.found && candOK
	return a
}

// refusal is the first failing rule of one model's check.
type refusal struct {
	rule    string
	reason  string
	admitAt *time.Time
	breaker string
}

// decideModel runs steps 1–4 for one model and returns a Decision whose
// Rule and Admit are what enforce mode would do (an admit under the
// bootstrap or stale rule keeps that rule); NotBefore, Reserve and the
// observe-mode notes are set by finish.
func (a *admission) decideModel(model string) Decision {
	e := a.estimate(model)
	d := Decision{For: a.s.For, Model: model, Estimate: e}
	pc := a.checkPools(a.cand, model, e)
	d.Pools, d.StalePools = pc.results, pc.stale
	var notes []string
	r := a.firstRefusal(model, e, pc, &notes)
	if pc.unitOnly {
		notes = append(notes, "every applicable pool uses unit reserves (cold.unit_fraction): admission uses headroom and the count of open reservations only")
	}
	d.Notes = notes
	if r == nil {
		d.Admit = true
		d.Rule = RuleAdmit
		if pc.unknown != "" {
			d.Rule = pc.unknown // admitted under rule bootstrap / stale
		}
		d.Reason = pc.admitReason(model)
		d.Reserve = pc.reserves
		return d
	}
	d.Rule, d.Reason, d.AdmitAt, d.Breaker = r.rule, r.reason, r.admitAt, r.breaker
	d.Reserve = pc.reserves // kept when observe mode proceeds anyway; finish clears it on an applied refusal
	return d
}

// firstRefusal applies the rules in spec order and returns the first that
// refuses, or nil.
func (a *admission) firstRefusal(model string, e Estimate, pc poolCheck, notes *[]string) *refusal {
	if !a.found {
		return &refusal{rule: RuleHalt, reason: fmt.Sprintf("halt: %s/%s attempt %d is not a candidate (no live, unhalted run lists it as dispatchable)", a.s.For.Run, a.s.For.Task, a.s.For.Attempt)}
	}
	if r := a.checkHalt(a.run); r != nil {
		return r
	}
	if r := a.checkDeadline(a.run, e, notes); r != nil {
		return r
	}
	if r := a.checkCap(a.run, model, e); r != nil {
		return r
	}
	if r := a.checkBreaker(a.cand.Key); r != nil {
		return r
	}
	if r := a.checkUnknown(model, pc); r != nil {
		return r
	}
	return pc.fresh
}

func (a *admission) estimate(model string) Estimate {
	if e, ok := a.est[model]; ok {
		return e
	}
	return Estimate{Model: model, TokensBasis: "missing", DurationBasis: "missing", USDBasis: "missing"}
}

// checkHalt: the run has a budget halt set.
func (a *admission) checkHalt(run RunState) *refusal {
	if run.Halt == "" {
		return nil
	}
	reason := fmt.Sprintf("halt: run %s is halted (%s)", run.Run, run.Halt)
	if run.HaltReason != "" {
		reason += ": " + run.HaltReason
	}
	return &refusal{rule: RuleHalt, reason: reason}
}

// checkDeadline: now ≥ deadline, or now + duration estimate >
// deadline. Unknown duration before the deadline admits with a note.
func (a *admission) checkDeadline(run RunState, e Estimate, notes *[]string) *refusal {
	if run.Deadline == nil {
		return nil
	}
	now, dl := a.s.Now, *run.Deadline
	if !now.Before(dl) {
		return &refusal{rule: RuleDeadline, reason: fmt.Sprintf("deadline: now %s is at or past the deadline %s", fmtTime(now), fmtTime(dl))}
	}
	if e.DurationMS == nil {
		*notes = append(*notes, "duration unknown; the deadline is enforced by stopping at the deadline")
		return nil
	}
	dur := time.Duration(*e.DurationMS) * time.Millisecond
	if end := now.Add(dur); end.After(dl) {
		return &refusal{rule: RuleDeadline, reason: fmt.Sprintf("deadline: duration estimate %.0fs (%s) from now %s ends at %s, after the deadline %s", dur.Seconds(), e.DurationBasis, fmtTime(now), fmtTime(end), fmtTime(dl))}
	}
	return nil
}

// checkCap: the USD estimate cap and the per-pool window caps.
// The USD amount of the attempt is its belt when belt.usd_factor is set,
// else its USD estimate rounded up to cents; unknown counts 0 (the run is
// still refused once spent alone reaches the cap).
func (a *admission) checkCap(run RunState, model string, e Estimate) *refusal {
	if run.CapUSD != nil {
		factor := 1.0
		what := "usd estimate"
		if a.cfg.BeltUSDFactor != nil {
			factor, what = *a.cfg.BeltUSDFactor, "belt"
		}
		amt := 0.0
		if b := Belt(e, factor); b != nil {
			amt = *b
		} else {
			what = "unknown usd estimate (0)"
		}
		if run.SpentUSD >= *run.CapUSD || run.SpentUSD+amt > *run.CapUSD {
			return &refusal{rule: RuleCap, reason: fmt.Sprintf("cap: usd estimate spent %.2f + %s %.2f = %.2f > cap_usd_estimate %.2f (client-side estimates, not plan usage)", run.SpentUSD, what, amt, run.SpentUSD+amt, *run.CapUSD)}
		}
	}
	caps := make([]string, 0, len(run.CapWindow))
	for p := range run.CapWindow {
		caps = append(caps, p)
	}
	sort.Strings(caps)
	for _, p := range caps {
		if !poolApplies(p, model, a.cfg.ModelFamilies) {
			continue
		}
		v, ok := a.views[p]
		if !ok {
			v = PoolView{Pool: p}
		}
		r, basis := a.reserveFor(v, e)
		att, limit := run.Attributable[p], run.CapWindow[p]
		if att+r > limit {
			return &refusal{rule: RuleCap, reason: fmt.Sprintf("cap: pool %s attributable %.4f + reserve %.4f (%s) = %.4f > cap_window %.4f", p, att, r, basis, att+r, limit)}
		}
	}
	return nil
}

// blockingBreaker returns the first open breaker (by id) whose scope covers
// k. VERIFIER_ERROR is the reconciler's and never blocks here.
func (a *admission) blockingBreaker(k Key) *Breaker {
	var best *Breaker
	for i := range a.s.Breakers {
		b := &a.s.Breakers[i]
		if b.State != "open" || b.Kind == VerifierError {
			continue
		}
		covers := b.Scope == ScopeGlobal ||
			(b.Scope == ScopeRun && b.Run == k.Run) ||
			(b.Scope == ScopeTask && b.Run == k.Run && b.Task == k.Task)
		if covers && (best == nil || b.ID < best.ID) {
			best = b
		}
	}
	return best
}

// checkBreaker: any open breaker covering the run or the task.
func (a *admission) checkBreaker(k Key) *refusal {
	b := a.blockingBreaker(k)
	if b == nil {
		return nil
	}
	until := "until a human clears it"
	if b.ClosesAt != nil {
		until = "until " + fmtTime(*b.ClosesAt)
	}
	scope := b.Scope
	switch b.Scope {
	case ScopeRun:
		scope += " " + b.Run
	case ScopeTask:
		scope += " " + b.Run + "/" + b.Task
	}
	reason := fmt.Sprintf("breaker: %s (%s, count %d) open %s", b.Kind, scope, b.Count, until)
	if b.Reason != "" {
		reason += ": " + b.Reason
	}
	return &refusal{rule: RuleBreaker, reason: reason, admitAt: copyTime(b.ClosesAt), breaker: b.Kind}
}

// poolCheck is the pool part of one model's decision (steps 3 and 4).
type poolCheck struct {
	results  []PoolResult
	reserves []PoolReserve
	stale    []string
	unknown  string // "", RuleBootstrap or RuleStale
	why      string // the bootstrap cause
	unitOnly bool
	fresh    *refusal // class_hold / headroom / fairness, or nil
}

func (pc poolCheck) admitReason(model string) string {
	switch pc.unknown {
	case RuleBootstrap:
		return fmt.Sprintf("bootstrap: %s; admitted as the one attempt at a time", pc.why)
	case RuleStale:
		return fmt.Sprintf("stale: admitted under the stale rule (stale pools %s); fresh pools fit%s", strings.Join(pc.stale, ", "), pc.freshSummary())
	}
	return fmt.Sprintf("admit: %s fits every applicable pool%s", model, pc.freshSummary())
}

func (pc poolCheck) freshSummary() string {
	var parts []string
	for _, r := range pc.results {
		if r.Stale {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: headroom %.4f - reserved %.4f - ahead %.4f - hold %.4f = %.4f >= reserve %.4f (%s)",
			r.Pool, r.Headroom, r.OpenReserved, r.Ahead, r.Hold, r.Headroom-r.OpenReserved-r.Ahead-r.Hold, r.Reserve, r.ReserveBasis))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// applicable returns the sorted applicable pools of model and the bootstrap
// cause ("" when some pool applies).
func (a *admission) applicable(model string) ([]string, string) {
	if len(a.pools) == 0 {
		return nil, "no pool has been observed in this state dir"
	}
	var out []string
	for _, p := range a.pools {
		if poolApplies(p, model, a.cfg.ModelFamilies) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Sprintf("no observed pool applies to model %q", model)
	}
	return out, ""
}

// unknownKind is the unknown-pool rule a model's decision falls under: bootstrap
// (no applicable pool), stale (an applicable pool is stale) or "".
func (a *admission) unknownKind(model string) string {
	pools, _ := a.applicable(model)
	if len(pools) == 0 {
		return RuleBootstrap
	}
	for _, p := range pools {
		if a.views[p].Stale {
			return RuleStale
		}
	}
	return ""
}

// reserveFor: k × est with basis "ratio" when the pool's fit
// passed its gate, the estimate is known and 0 < k × est ≤ 1; else the unit
// fraction (cold.unit_fraction, policy) with basis "unit".
func (a *admission) reserveFor(v PoolView, e Estimate) (float64, string) {
	if v.Fit.Basis == "fit" && e.Tokens != nil {
		if r := v.Fit.KPerToken * *e.Tokens; r > 0 && r <= 1 {
			return r, "ratio"
		}
	}
	return a.cfg.ColdUnitFraction, "unit"
}

// holdFraction is Σ reserve_fraction of the configured classes ranked above
// class.
func (a *admission) holdFraction(class string) float64 {
	rank, _ := a.cfg.ClassRank(class)
	names := make([]string, 0, len(a.cfg.Classes))
	for n := range a.cfg.Classes {
		names = append(names, n)
	}
	sort.Strings(names)
	f := 0.0
	for _, n := range names {
		if cl := a.cfg.Classes[n]; cl.Rank > rank {
			f += cl.ReserveFraction
		}
	}
	return f
}

func (a *admission) hold(class string, headroom float64) float64 {
	return math.Max(headroom, 0) * a.holdFraction(class)
}

// sameKind reports whether a reservation counts in this decision: enforce
// decisions count enforced reservations, observe decisions observe-mode
// ones.
func (a *admission) sameKind(r Reservation) bool { return r.Enforced == a.enforce }

// openReserved is the open + settled-after-observation part of R_p:
// Σ reserve of live open reservations, plus Σ max(k × actual,
// reserve) of Recent reservations settled after the pool's newest
// observation (actual unknown → reserve).
func (a *admission) openReserved(p string) float64 {
	v := a.views[p]
	sum := 0.0
	for _, r := range a.s.Open {
		if !a.sameKind(r) {
			continue
		}
		for _, pr := range r.Pools {
			if pr.Pool == p {
				sum += pr.Reserve
			}
		}
	}
	k := 0.0
	if v.Fit.Basis == "fit" {
		k = v.Fit.KPerToken
	}
	for _, r := range a.s.Recent {
		if !a.sameKind(r) || r.SettledAt == nil || (v.ObservedAt != nil && !r.SettledAt.After(*v.ObservedAt)) {
			continue
		}
		for _, pr := range r.Pools {
			if pr.Pool != p {
				continue
			}
			x := pr.Reserve
			if r.ActualTokens != nil {
				x = math.Max(k**r.ActualTokens, pr.Reserve)
			}
			sum += x
		}
	}
	return sum
}

// openCount is the number of live open reservations of this decision's
// enforcement kind (bootstrap/stale; never Recent).
func (a *admission) openCount() int {
	n := 0
	for _, r := range a.s.Open {
		if a.sameKind(r) {
			n++
		}
	}
	return n
}

// canStart reports whether a candidate ahead in the order could be admitted
// at all as far as its run is concerned: its run is known and not halted,
// its deadline has not passed and (enforce mode) no breaker blocks it.
// Candidates that cannot start take no virtual reservation and are not
// "first" for the bootstrap/stale rules.
func (a *admission) canStart(c Candidate) bool {
	rs, ok := a.runs[c.Run]
	if !ok || rs.Halt != "" {
		return false
	}
	if rs.Deadline != nil && !a.s.Now.Before(*rs.Deadline) {
		return false
	}
	return !a.enforce || a.blockingBreaker(c.Key) == nil
}

// fitsFresh checks c's model against every fresh applicable pool with
// extra already virtually reserved on top of the open part; it returns the
// per-pool reserves when it fits.
func (a *admission) fitsFresh(c Candidate, model string, open, extra map[string]float64) (map[string]float64, bool) {
	pools, _ := a.applicable(model)
	e := a.estimate(model)
	res := map[string]float64{}
	for _, p := range pools {
		v := a.views[p]
		if v.Stale {
			continue
		}
		r, _ := a.reserveFor(v, e)
		if v.Headroom-open[p]-extra[p]-a.hold(c.Class, v.Headroom) < r {
			return nil, false
		}
		res[p] = r
	}
	return res, true
}

// walkAhead is the strict-priority walk: candidates ahead of
// s.For that fit are virtually reserved; the first one ahead that does not
// fit stops the walk and is returned. A candidate ahead is checked with its
// first model only, even under routing: letting it switch models as
// headroom grows would change its virtual reserves and break monotonicity.
// It is stricter, never looser: a routed candidate ahead that
// fits only with a later model blocks until its own decision admits it.
func (a *admission) walkAhead(open map[string]float64) (map[string]float64, *Candidate) {
	virtual := map[string]float64{}
	for i := range a.order {
		c := a.order[i]
		if c.Key == a.s.For {
			break
		}
		if !a.canStart(c) {
			continue
		}
		res, ok := a.fitsFresh(c, firstModel(c), open, virtual)
		if !ok {
			return virtual, &c
		}
		for p, r := range res {
			virtual[p] += r
		}
	}
	return virtual, nil
}

// checkPools is step 3's classification and step 4's fresh-pool check for
// s.For's candidate with model.
func (a *admission) checkPools(c Candidate, model string, e Estimate) poolCheck {
	var pc poolCheck
	pools, why := a.applicable(model)
	if why != "" {
		pc.unknown, pc.why = RuleBootstrap, why
		return pc
	}
	open := make(map[string]float64, len(a.pools))
	for _, p := range a.pools {
		open[p] = a.openReserved(p)
	}
	virtual, blocker := a.walkAhead(open)
	pc.unitOnly = true
	fitsAlone, fitsAll := true, blocker == nil
	var firstFull, firstAlone *PoolResult
	for _, p := range pools {
		v := a.views[p]
		r, basis := a.reserveFor(v, e)
		if basis != "unit" {
			pc.unitOnly = false
		}
		pr := PoolResult{PoolView: v, OpenReserved: open[p], Ahead: virtual[p], Hold: a.hold(c.Class, v.Headroom), Reserve: r, ReserveBasis: basis}
		pr.Fits = v.Headroom-pr.OpenReserved-pr.Ahead-pr.Hold >= r
		pc.reserves = append(pc.reserves, PoolReserve{Pool: p, Reserve: r, Basis: basis})
		pc.results = append(pc.results, pr)
		if v.Stale {
			pc.stale = append(pc.stale, p)
			continue
		}
		if v.Headroom-pr.OpenReserved-pr.Hold < r {
			fitsAlone = false
		}
		if !pr.Fits {
			fitsAll = false
		}
	}
	if len(pc.stale) > 0 {
		pc.unknown = RuleStale
	}
	for i := range pc.results {
		pr := &pc.results[i]
		if pr.Stale {
			continue
		}
		if firstFull == nil && !pr.Fits {
			firstFull = pr
		}
		if firstAlone == nil && pr.Headroom-pr.OpenReserved-pr.Hold < pr.Reserve {
			firstAlone = pr
		}
	}
	switch {
	case fitsAll:
	case fitsAlone:
		at := a.s.Now.Add(a.cfg.FairnessRecheck)
		var reason string
		if blocker != nil {
			reason = fmt.Sprintf("fairness: fits alone, but %s/%s (class %s) ahead in fairness order does not fit and blocks everyone behind it (strict priority)", blocker.Run, blocker.Task, blocker.Class)
		} else {
			pr := firstFull
			reason = fmt.Sprintf("fairness: pool %s: headroom %.4f - reserved %.4f - ahead %.4f - hold %.4f = %.4f < reserve %.4f (%s); fits alone without the candidates ahead",
				pr.Pool, pr.Headroom, pr.OpenReserved, pr.Ahead, pr.Hold, pr.Headroom-pr.OpenReserved-pr.Ahead-pr.Hold, pr.Reserve, pr.ReserveBasis)
		}
		pc.fresh = &refusal{rule: RuleFairness, reason: reason, admitAt: &at}
	default:
		pr := firstAlone
		left := pr.Headroom - pr.OpenReserved
		if left >= pr.Reserve {
			pc.fresh = &refusal{rule: RuleClassHold, reason: fmt.Sprintf("class_hold: pool %s: headroom %.4f - reserved %.4f = %.4f fits reserve %.4f (%s), but hold %.4f for classes above %q leaves %.4f",
				pr.Pool, pr.Headroom, pr.OpenReserved, left, pr.Reserve, pr.ReserveBasis, pr.Hold, c.Class, left-pr.Hold)}
		} else {
			pc.fresh = &refusal{rule: RuleHeadroom, reason: fmt.Sprintf("headroom: pool %s: headroom %.4f (at horizon, %s) - reserved %.4f = %.4f < reserve %.4f (%s)",
				pr.Pool, pr.Headroom, pr.HorizonBasis, pr.OpenReserved, left, pr.Reserve, pr.ReserveBasis)}
		}
		pc.fresh.admitAt = a.earliestReset(pc.results)
	}
	return pc
}

// earliestReset is the earliest future resets_at among the fresh pools that
// do not fit alone (headroom/class_hold admit_at), or nil.
func (a *admission) earliestReset(results []PoolResult) *time.Time {
	var best *time.Time
	for _, pr := range results {
		if pr.Stale || pr.ResetsAt == nil || pr.Headroom-pr.OpenReserved-pr.Hold >= pr.Reserve {
			continue
		}
		t := time.Unix(*pr.ResetsAt, 0).UTC()
		if t.After(a.s.Now) && (best == nil || t.Before(*best)) {
			best = &t
		}
	}
	return best
}

// checkUnknown is the bootstrap/stale rule: admitted only while
// live open reservations of this kind are below the limit (1 for bootstrap,
// stale_max_open for stale) and s.For is the first startable candidate in
// fairness order among those under the same rule (bootstrap: among those
// with the lowest token estimate, unknown counting highest).
func (a *admission) checkUnknown(model string, pc poolCheck) *refusal {
	if pc.unknown == "" {
		return nil
	}
	at := a.s.Now.Add(a.cfg.StaleRecheck)
	limit, what := 1, "bootstrap: "+pc.why+"; one attempt at a time state-dir-wide"
	if pc.unknown == RuleStale {
		limit = a.cfg.StaleMaxOpen
		var parts []string
		for _, r := range pc.results {
			if r.Stale {
				parts = append(parts, fmt.Sprintf("%s (age %.0fs, %s)", r.Pool, r.AgeSeconds, r.StaleReason))
			}
		}
		what = fmt.Sprintf("stale: pools %s are stale; stale_max_open %d", strings.Join(parts, ", "), limit)
	}
	if n := a.openCount(); n >= limit {
		return &refusal{rule: pc.unknown, admitAt: &at, reason: fmt.Sprintf("%s; %d live open reservation(s) >= limit %d", what, n, limit)}
	}
	var first *Candidate
	bestTok := math.Inf(1)
	for i := range a.order {
		c := a.order[i]
		if !a.canStart(c) {
			continue
		}
		m := model
		if c.Key != a.s.For {
			m = firstModel(c)
		}
		if a.unknownKind(m) != pc.unknown {
			continue
		}
		if pc.unknown == RuleStale {
			first = &c
			break
		}
		tok := math.Inf(1)
		if e := a.estimate(m); e.Tokens != nil {
			tok = *e.Tokens
		}
		if first == nil || tok < bestTok {
			first, bestTok = &c, tok
		}
	}
	if first != nil && first.Key != a.s.For {
		return &refusal{rule: pc.unknown, admitAt: &at, reason: fmt.Sprintf("%s; %d live open reservation(s) < limit %d, but %s/%s is first in fairness order under this rule", what, a.openCount(), limit, first.Run, first.Task)}
	}
	return nil
}

// runFlagRule reports whether rule is a run-flag rule, enforced in observe
// mode too.
func runFlagRule(rule string) bool {
	return rule == RuleHalt || rule == RuleDeadline || rule == RuleCap
}

// finish is steps 5–6 after routing: the observe-mode notes, the fairness
// order, NotBefore, Reserve and BeltUSD.
func (a *admission) finish(d Decision) Decision {
	d.Order = make([]Key, len(a.order))
	for i, c := range a.order {
		d.Order[i] = c.Key
	}
	switch {
	case d.Admit:
		if !a.enforce {
			d.Notes = append(d.Notes, "observe: decision not enforced")
		}
	case !a.enforce && !runFlagRule(d.Rule):
		// Observe mode: the refusal is recorded, not applied.
		// Reserve stays: the gate starts the attempt anyway.
		d.Notes = append(d.Notes, fmt.Sprintf("observe: rule %s not enforced (enforce mode would refuse)", d.Rule))
	default:
		d.Reserve = nil
		if !a.enforce {
			d.Notes = append(d.Notes, fmt.Sprintf("observe: run flag rule %s enforced", d.Rule))
		}
	}
	if !d.Admit {
		nb := notBefore(a.s.Now, d.AdmitAt, a.cfg, a.s.For)
		d.NotBefore = &nb
	}
	if a.enforce && a.cfg.BeltUSDFactor != nil {
		d.BeltUSD = Belt(d.Estimate, *a.cfg.BeltUSDFactor)
	}
	sort.Strings(d.Notes)
	return d
}

// notBefore is max(now + 1s, min(admit_at, now + recheck_max) +
// Jitter(run|task|attempt|admit_at, jitter)); admit_at unknown → now +
// recheck_max (and "" in the jitter key).
func notBefore(now time.Time, admitAt *time.Time, c Config, k Key) time.Time {
	target := now.Add(c.RecheckMax)
	at := ""
	if admitAt != nil {
		at = fmtTime(*admitAt)
		if admitAt.Before(target) {
			target = *admitAt
		}
	}
	nb := target.Add(Jitter(fmt.Sprintf("%s|%s|%d|%s", k.Run, k.Task, k.Attempt, at), c.Jitter))
	if floor := now.Add(time.Second); nb.Before(floor) {
		nb = floor
	}
	return nb
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
