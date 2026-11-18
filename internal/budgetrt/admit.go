package budgetrt

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/reconcile"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

var _ reconcile.Budget = (*Gate)(nil)

// runFlagRule: halt, deadline and cap are enforced in observe mode too,
// so their decisions carry enforced = true.
func runFlagRule(rule string) bool {
	return rule == budget.RuleHalt || rule == budget.RuleDeadline || rule == budget.RuleCap
}

// Admit implements reconcile.Budget: settle
// orphans, evaluate RETRY_STORM and RATE_LIMITED, load the snapshot in tx,
// decide (pure), log the inputs not yet in this run's file and the
// decision, and on an enforced refusal upsert the park row (never tasks).
func (g *Gate) Admit(tx *sql.Tx, req reconcile.AdmitRequest) (reconcile.AdmitResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var res reconcile.AdmitResult
	if err := g.resolve(tx); err != nil {
		return res, err
	}
	now := req.Now
	if now.IsZero() {
		now = g.Now()
	}
	if err := g.checkOwn(tx, true); err != nil {
		return res, err
	}
	// Our own heartbeat first, so our live reservations are not orphans.
	if err := state.HeartbeatBudgetRun(tx, g.o.Run, now); err != nil {
		return res, err
	}
	if err := g.settleOrphans(tx, now); err != nil {
		return res, err
	}
	s, err := g.snapshot(tx, req.Task, req.Attempt, req.Model, now)
	if err != nil {
		return res, err
	}
	// Breakers before the decision, so an admission never races a breaker
	// that should already be open.
	changed, err := g.evalBreakers(tx, now, s.Status, "", nil)
	if err != nil {
		return res, err
	}
	if changed {
		if s.Breakers, err = state.OpenBreakers(tx); err != nil {
			return res, err
		}
	}
	d := budget.Decide(s)
	if g.proceeds(d) {
		// The attempt this admission starts is itself a retry candidate:
		// Reserve would record it with StartedAt = now, so
		// count it now. If that opens RETRY_STORM, the storm-making retry is
		// refused (rule breaker) instead of slipping in before the next Tick
		// opens the breaker behind it.
		pf := budget.AttemptFact{AttemptID: math.MaxInt64, Task: req.Task, N: req.Attempt, StartedAt: now}
		opened, err := g.evalBreakers(tx, now, s.Status, "", func(ch budget.BreakerChange) bool {
			return ch.Action == "open" && ch.Breaker.Kind == budget.RetryStorm
		}, pf)
		if err != nil {
			return res, err
		}
		if opened {
			if s.Breakers, err = state.OpenBreakers(tx); err != nil {
				return res, err
			}
			d = budget.Decide(s)
		}
	}
	// In observe mode a refusal by any rule but the run flags is recorded
	// (decision.admit = false, enforced = false) and not applied.
	enforced := g.enforce() || runFlagRule(d.Rule)
	proceed := g.proceeds(d)
	ids := g.inputIDs(s)
	ld := g.m.last[req.Task]
	key := ""
	if !proceed {
		key = suppressKey(d, ids, s.Breakers)
	}
	dj, err := json.Marshal(d)
	if err != nil {
		return res, err
	}
	res = reconcile.AdmitResult{Admit: proceed, Model: d.Model, Rule: d.Rule, Reason: d.Reason, Decision: dj}
	if res.Model == "" {
		res.Model = req.Model
	}

	if !proceed && ld != nil && ld.key == key && ld.eid > 0 {
		// An identical refusal (same rule, reason, input ids and breakers):
		// not re-logged; the next logged decision carries suppressed: n.
		prev := *ld
		ld.suppressed++
		g.track(tx, func() { *ld = prev })
		if err := g.park(tx, req.Task, d, ld.eid, now); err != nil {
			return res, err
		}
		res.NotBefore = refusedNotBefore(d, now)
		return res, nil
	}

	if err := g.fitEvents(tx, now, s, nil); err != nil {
		return res, err
	}
	if err := g.logInputs(tx, now, s, ids); err != nil {
		return res, err
	}
	ev := DecisionEvent{
		Now: now, Mode: g.mode(), Enforced: enforced, Attempt: req.Attempt, ConfigSHA256: s.ConfigSHA256,
		ObsIDs: ids.obs, SpanIDs: ids.spans, SampleIDs: ids.samples,
		Floors: s.Floors, Open: s.Open, Recent: s.Recent, Breakers: s.Breakers, Candidates: s.Candidates, Runs: s.Runs,
		For: s.For, Decision: d,
	}
	if ld != nil {
		ev.Suppressed = ld.suppressed
	}
	eid, data, err := g.event(tx, now, req.Task, budgetlog.TypeDecision, ev)
	if err != nil {
		return res, err
	}
	dg, err := Digest([]byte(data))
	if err != nil {
		return res, err
	}
	g.m.digests[eid] = dg
	g.track(tx, func() { delete(g.m.digests, eid) })
	g.setLast(tx, req.Task, &lastDecision{key: key, rule: d.Rule, eid: eid})

	if !proceed {
		if err := g.park(tx, req.Task, d, eid, now); err != nil {
			return res, err
		}
		res.NotBefore = refusedNotBefore(d, now)
		return res, nil
	}
	if _, ok := g.m.parks[req.Task]; ok {
		if _, err := tx.Exec(`DELETE FROM budget_parks WHERE run_id = ? AND task_id = ?`, g.o.Run, req.Task); err != nil {
			return res, fmt.Errorf("budgetrt: unpark %s: %w", req.Task, err)
		}
		g.forgetPark(tx, req.Task)
	}
	if g.enforce() && d.BeltUSD != nil {
		// --max-budget-usd is a client-side list-price estimate; it does not
		// cap plan usage.
		res.ExtraArgv = []string{"--max-budget-usd", fmt.Sprintf("%.2f", *d.BeltUSD)}
	}
	prevStash, hadStash := g.m.admits[req.Task]
	g.m.admits[req.Task] = admitStash{eid: eid, decision: d, now: now}
	g.track(tx, func() {
		if hadStash {
			g.m.admits[req.Task] = prevStash
		} else {
			delete(g.m.admits, req.Task)
		}
	})
	return res, nil
}

// proceeds reports whether the attempt starts: the decision admits, or it
// refuses by a rule this mode does not enforce.
func (g *Gate) proceeds(d budget.Decision) bool {
	return d.Admit || !(g.enforce() || runFlagRule(d.Rule))
}

func refusedNotBefore(d budget.Decision, now time.Time) time.Time {
	nb := now.Add(time.Second)
	if d.NotBefore != nil && d.NotBefore.After(nb) {
		nb = *d.NotBefore
	}
	return nb
}

func (g *Gate) setLast(tx *sql.Tx, task string, ld *lastDecision) {
	prev, had := g.m.last[task]
	g.m.last[task] = ld
	g.track(tx, func() {
		if had {
			g.m.last[task] = prev
		} else {
			delete(g.m.last, task)
		}
	})
}

// park upserts the task's budget_parks row (never tasks) and remembers it.
func (g *Gate) park(tx *sql.Tx, task string, d budget.Decision, eid int64, now time.Time) error {
	if err := state.Park(tx, g.o.Run, task, d, eid, now); err != nil {
		return err
	}
	prev, had := g.m.parks[task]
	g.m.parks[task] = ownPark{rule: d.Rule, reason: d.Reason, eid: eid}
	g.track(tx, func() {
		if had {
			g.m.parks[task] = prev
		} else {
			delete(g.m.parks, task)
		}
	})
	return nil
}

func (g *Gate) forgetPark(tx *sql.Tx, task string) {
	prev, had := g.m.parks[task]
	if !had {
		return
	}
	delete(g.m.parks, task)
	g.track(tx, func() { g.m.parks[task] = prev })
}

// snapshot loads the decision inputs in tx and applies this process's
// memory: floors, its own tasks' class/prefer/max_turns from the
// spec (cross-check 1), its own spend from the usage it parsed.
func (g *Gate) snapshot(tx *sql.Tx, task string, attempt int, model string, now time.Time) (budget.Snapshot, error) {
	s, err := state.LoadSnapshot(tx, state.SnapshotRequest{
		Run: g.o.Run, Task: task, Attempt: attempt, Model: model, Mode: g.mode(), Config: g.o.Config, Now: now,
	})
	if err != nil {
		return s, err
	}
	s.ConfigSHA256 = g.o.ConfigSHA
	s.Floors = make(map[string]budget.Obs, len(g.m.floors))
	for p, o := range g.m.floors {
		s.Floors[p] = o
	}
	for i := range s.Candidates {
		c := &s.Candidates[i]
		if c.Run != g.o.Run {
			continue
		}
		meta, ok := g.m.tasks[c.Task]
		if !ok {
			meta = TaskMeta{Class: budget.ClassNormal}
		}
		c.Class = meta.Class
		c.MaxTurns = meta.MaxTurns
		if c.MaxTurns <= 0 {
			c.MaxTurns = g.o.MaxTurns
		}
		if c.Key == s.For {
			if g.o.Config.RoutingEnabled && len(meta.Prefer) > 0 {
				c.Models = append([]string(nil), meta.Prefer...)
			} else {
				c.Models = []string{model}
			}
		}
	}
	for i := range s.Runs {
		if s.Runs[i].Run == g.o.Run {
			s.Runs[i].SpentUSD = g.spentUSD()
		}
	}
	return s, nil
}

// spentUSD is the run's spend from memory: the cost estimates of the run's
// settled attempts (from the result lines this process parsed) plus the
// belts of its open reservations. mu is held.
func (g *Gate) spentUSD() float64 {
	ids := make([]int64, 0, len(g.m.cost))
	for id := range g.m.cost {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	total := 0.0
	for _, id := range ids {
		total += g.m.cost[id]
	}
	rids := make([]int64, 0, len(g.m.res))
	for id := range g.m.res {
		rids = append(rids, id)
	}
	sort.Slice(rids, func(i, j int) bool { return rids[i] < rids[j] })
	for _, id := range rids {
		if r := g.m.res[id]; r.open && r.belt != nil {
			total += *r.belt
		}
	}
	return total
}

// settleOrphans settles every open reservation that is not live,
// one budget_settle event each.
func (g *Gate) settleOrphans(tx *sql.Tx, now time.Time) error {
	orphans, err := state.SettleOrphans(tx, now, g.o.Config.RunTTL)
	if err != nil {
		return err
	}
	for _, r := range orphans {
		ev := SettleEvent{ReservationID: r.ID, AttemptID: r.AttemptID, ActualBasis: "orphan", Pools: r.Pools}
		if own := g.m.res[r.AttemptID]; own != nil && r.Run == g.o.Run {
			ev.EstTokens = own.est
			g.closeRes(tx, own)
		}
		if _, _, err := g.event(tx, now, r.Task, budgetlog.TypeSettle, ev); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gate) closeRes(tx *sql.Tx, r *ownRes) {
	if !r.open {
		return
	}
	r.open = false
	g.track(tx, func() { r.open = true })
}

// Reserve implements reconcile.Budget: the open reservation of the attempt
// Admit admitted, with the decision's eid.
func (g *Gate) Reserve(tx *sql.Tx, ref core.AttemptRef, res reconcile.AdmitResult) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.resolve(tx); err != nil {
		return err
	}
	st, ok := g.m.admits[ref.Task]
	if !ok {
		var d budget.Decision
		if err := json.Unmarshal(res.Decision, &d); err != nil {
			return fmt.Errorf("budgetrt: reserve %s: no admitting decision: %w", ref.Task, err)
		}
		st = admitStash{decision: d, now: g.Now()}
	}
	d := st.decision
	if err := state.InsertReservation(tx, ref, d, g.enforce(), st.eid, st.now); err != nil {
		return err
	}
	pj, err := poolsJSON(d.Reserve)
	if err != nil {
		return err
	}
	var belt *float64
	if d.BeltUSD != nil && g.enforce() {
		b := *d.BeltUSD
		belt = &b
	}
	prev, had := g.m.res[ref.AttemptID]
	g.m.res[ref.AttemptID] = &ownRes{pools: pj, enforced: g.enforce(), open: true, est: d.Estimate.Tokens, belt: belt, model: d.Model}
	delete(g.m.admits, ref.Task)
	g.track(tx, func() {
		if had {
			g.m.res[ref.AttemptID] = prev
		} else {
			delete(g.m.res, ref.AttemptID)
		}
		g.m.admits[ref.Task] = st
	})
	g.setFact(tx, budget.AttemptFact{AttemptID: ref.AttemptID, Task: ref.Task, N: ref.Attempt, StartedAt: st.now})
	return nil
}

// Settle implements reconcile.Budget: the attempt's reservation is settled
// with the usage this process parsed from the result line (cross-check 5),
// a budget_settle event is written, re-armable parks of every run are
// deleted, and the fit of the reservation's pools is rechecked.
func (g *Gate) Settle(tx *sql.Tx, ref core.AttemptRef, end state.AttemptEnd) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.resolve(tx); err != nil {
		return err
	}
	now := g.Now()
	f := budget.AttemptFact{AttemptID: ref.AttemptID, Task: ref.Task, N: ref.Attempt, StartedAt: now}
	if prev := g.m.facts[ref.AttemptID]; prev != nil {
		f = *prev
	}
	f.HeadSHA = end.HeadSHA
	f.Refunded = end.Outcome == "start_failed" || end.Outcome == "lost"
	g.setFact(tx, f)
	prevCost, hadCost := g.m.cost[ref.AttemptID]
	g.m.cost[ref.AttemptID] = end.CostUSDEstimate
	g.track(tx, func() {
		if hadCost {
			g.m.cost[ref.AttemptID] = prevCost
		} else {
			delete(g.m.cost, ref.AttemptID)
		}
	})

	var usage *events.Usage
	if u := end.Usage; u.Input != 0 || u.Output != 0 || u.CacheRead != 0 || u.CacheCreate != 0 {
		usage = &u
	}
	r, actual, ok, err := state.SettleReservation(tx, ref.AttemptID, usage, now)
	if err != nil {
		return err
	}
	own := g.m.res[ref.AttemptID]
	if !ok {
		if own != nil && own.open {
			return tampered("reservation of attempt %d is not open at settle", ref.AttemptID)
		}
		return nil // no reservation (attempt from before the gate, or none made)
	}
	ev := SettleEvent{ReservationID: r.ID, AttemptID: ref.AttemptID, ActualTokens: actual, ActualBasis: "unknown", Pools: r.Pools}
	if usage != nil {
		ev.ActualBasis = "usage"
	}
	if own != nil {
		ev.EstTokens = own.est
		g.closeRes(tx, own)
	}
	if ev.EstTokens != nil && actual != nil && float64(*actual) > *ev.EstTokens {
		ev.Overrun = true
	}
	if _, _, err := g.event(tx, now, ref.Task, budgetlog.TypeSettle, ev); err != nil {
		return err
	}
	if _, err := state.RearmParks(tx); err != nil {
		return err
	}
	if len(r.Pools) == 0 {
		return nil
	}
	pools := map[string]bool{}
	for _, p := range r.Pools {
		pools[p.Pool] = true
	}
	model := ""
	if own != nil {
		model = own.model
	}
	s, err := g.snapshot(tx, ref.Task, ref.Attempt, model, now)
	if err != nil {
		return err
	}
	return g.fitEvents(tx, now, s, pools)
}

// Retry implements reconcile.Budget: SAME_FAILURE for the task.
// In enforce mode an opened breaker returns its reason, and the reconciler
// blocks the task instead of retrying it.
func (g *Gate) Retry(tx *sql.Tx, t state.TaskRow) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.resolve(tx); err != nil {
		return "", err
	}
	now := g.Now()
	for _, f := range g.factsSorted() {
		if f.Task == t.ID && f.BaseSHA != t.BaseSHA {
			f.BaseSHA = t.BaseSHA
			g.setFact(tx, f)
		}
	}
	block := ""
	_, err := g.evalBreakers(tx, now, nil, t.ID, func(ch budget.BreakerChange) bool {
		if ch.Breaker.Kind != budget.SameFailure {
			return false
		}
		if ch.Action == "open" && g.enforce() {
			block = ch.Breaker.Reason
		}
		return true
	})
	return block, err
}

// Verdict implements reconcile.Budget: it records the verdict in the
// attempt's fact and logs the reconciler's VERIFIER_ERROR count (action
// count, or open when the reconciler is halting). It never halts.
func (g *Gate) Verdict(tx *sql.Tx, ref core.AttemptRef, result string, consecutive int, halting bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.resolve(tx); err != nil {
		return err
	}
	now := g.Now()
	f := budget.AttemptFact{AttemptID: ref.AttemptID, Task: ref.Task, N: ref.Attempt, StartedAt: now}
	if prev := g.m.facts[ref.AttemptID]; prev != nil {
		f = *prev
	}
	f.Verdict = result
	f.FailedGates = nil
	// The failed gates of the verdict just recorded, in this same tx (the
	// hook does not carry them; the row is the one this process wrote).
	var gates sql.NullString
	if err := tx.QueryRow(`SELECT failed_gates_json FROM attempts WHERE id = ?`, ref.AttemptID).Scan(&gates); err == nil && gates.Valid && gates.String != "" {
		_ = json.Unmarshal([]byte(gates.String), &f.FailedGates)
	}
	g.setFact(tx, f)
	if result != "error" && g.m.verrs == 0 {
		return nil
	}
	action, st := "count", "closed"
	if halting {
		action, st = "open", "open"
	}
	b := budget.Breaker{Kind: budget.VerifierError, Scope: budget.ScopeRun, Run: g.o.Run, State: st, Count: consecutive, Enforced: true,
		Reason: fmt.Sprintf("VERIFIER_ERROR: %d consecutive could-not-verify results (the reconciler halts verifier_error at its limit)", consecutive)}
	if halting {
		b.OpenedAt = &now
	}
	in := g.breakerInput(now, ref.Task, nil, nil)
	in.Verdict = result
	inj, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if err := state.ApplyBreakerChange(tx, budget.BreakerChange{Action: action, Breaker: b}, inj, now); err != nil {
		return err
	}
	if _, _, err := g.event(tx, now, ref.Task, budgetlog.TypeBreaker, BreakerEvent{Action: action, Breaker: b, Inputs: in}); err != nil {
		return err
	}
	prev := g.m.verrs
	g.m.verrs = consecutive
	g.track(tx, func() { g.m.verrs = prev })
	return nil
}

// breakerInput builds one EvalBreakers input from memory; prospective facts
// (an attempt being admitted, not yet reserved) are appended to the run's.
func (g *Gate) breakerInput(now time.Time, task string, status []budget.Obs, current []budget.Breaker, prospective ...budget.AttemptFact) budget.BreakerInput {
	if status == nil {
		status = []budget.Obs{}
	}
	if current == nil {
		current = []budget.Breaker{}
	}
	return budget.BreakerInput{Now: now, Mode: g.mode(), Config: g.o.Config, Run: g.o.Run, Task: task,
		Attempts: append(g.factsSorted(), prospective...), Status: status, Current: current}
}

// evalBreakers runs budget.EvalBreakers with the run's facts (memory), its
// own breaker rows (memory) and the global RATE_LIMITED rows (DB), applies
// each change keep accepts (nil = all) with a budget_breaker event, and
// reports whether anything changed. prospective facts are counted too (see
// Admit). mu is held.
func (g *Gate) evalBreakers(tx *sql.Tx, now time.Time, status []budget.Obs, task string, keep func(budget.BreakerChange) bool, prospective ...budget.AttemptFact) (bool, error) {
	rl, err := rateLimitedRows(tx)
	if err != nil {
		return false, err
	}
	cur := append([]budget.Breaker{}, rl...)
	ids := make([]int64, 0, len(g.m.breakers))
	for id := range g.m.breakers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		cur = append(cur, g.m.breakers[id])
	}
	in := g.breakerInput(now, task, status, cur, prospective...)
	changes := budget.EvalBreakers(in)
	any := false
	for _, ch := range changes {
		if keep != nil && !keep(ch) {
			continue
		}
		if err := g.applyBreaker(tx, now, ch, in); err != nil {
			return any, err
		}
		any = true
	}
	return any, nil
}

func (g *Gate) applyBreaker(tx *sql.Tx, now time.Time, ch budget.BreakerChange, in budget.BreakerInput) error {
	inj, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if err := state.ApplyBreakerChange(tx, ch, inj, now); err != nil {
		return err
	}
	b := ch.Breaker
	if b.Scope == "" {
		b.Scope = budget.ScopeRun
	}
	if ch.Action == "open" && b.ID == 0 {
		if err := tx.QueryRow(`SELECT id FROM budget_breakers WHERE kind = ? AND scope = ? AND run_id = ? AND task_id = ? ORDER BY id DESC LIMIT 1`,
			b.Kind, b.Scope, b.Run, b.Task).Scan(&b.ID); err != nil {
			return fmt.Errorf("budgetrt: breaker id: %w", err)
		}
	}
	if ch.Action == "open" && b.OpenedAt == nil {
		b.OpenedAt = &now
	}
	if ch.Action == "close" {
		b.State = "closed"
		if b.ClosedAt == nil {
			b.ClosedAt = &now
		}
	}
	ownKind := (b.Kind == budget.RetryStorm && b.Scope == budget.ScopeRun) || (b.Kind == budget.SameFailure && b.Scope == budget.ScopeTask)
	if ownKind && b.Run == g.o.Run && b.ID > 0 {
		prev, had := g.m.breakers[b.ID]
		g.m.breakers[b.ID] = b
		id := b.ID
		g.track(tx, func() {
			if had {
				g.m.breakers[id] = prev
			} else {
				delete(g.m.breakers, id)
			}
		})
	}
	task := b.Task
	_, _, err = g.event(tx, now, task, budgetlog.TypeBreaker, BreakerEvent{Action: ch.Action, Breaker: b, Inputs: in})
	return err
}

// fitEvents writes a budget_fit event for each pool (only those in filter
// when set) whose fit differs from the last one this run logged.
func (g *Gate) fitEvents(tx *sql.Tx, now time.Time, s budget.Snapshot, filter map[string]bool) error {
	c := g.o.Config
	pools := make([]string, 0, len(s.Obs))
	for p := range s.Obs {
		if filter == nil || filter[p] {
			pools = append(pools, p)
		}
	}
	sort.Strings(pools)
	for _, p := range pools {
		iv := budget.Intervals(p, s.Obs[p], s.Spans, c.ModelFamilies, c.ResetEpsilon, s.Now, c.FitLookback)
		fit := budget.FitRatio(p, iv, c)
		j, err := Canonical(fit)
		if err != nil {
			return err
		}
		if g.m.fits[p] == string(j) {
			continue
		}
		if iv == nil {
			iv = []budget.Interval{}
		}
		// Replay recomputes a fit with the newest config line before it, so
		// the config is logged first (once per file).
		if err := g.logInputs(tx, now, s, inputSet{rows: []inputRow{g.configInput(s)}}); err != nil {
			return err
		}
		if _, _, err := g.event(tx, now, "", budgetlog.TypeFit, FitEvent{Pool: p, Intervals: iv, Fit: fit}); err != nil {
			return err
		}
		prev, had := g.m.fits[p]
		g.m.fits[p] = string(j)
		pool := p
		g.track(tx, func() {
			if had {
				g.m.fits[pool] = prev
			} else {
				delete(g.m.fits, pool)
			}
		})
	}
	return nil
}

// inputSet is the ids of every input row one decision uses.
type inputSet struct {
	obs, spans, samples []int64
	rows                []inputRow
}

type inputRow struct {
	key, kind string
	id        json.RawMessage
	row       []byte
}

// inputIDs lists the decision's input rows: the config, every observation
// (pools sorted, rows in snapshot order), every span (by attempt id) and
// every sample (models sorted, rows in snapshot order). mu is held.
func (g *Gate) inputIDs(s budget.Snapshot) inputSet {
	var out inputSet
	out.obs, out.spans, out.samples = []int64{}, []int64{}, []int64{}
	out.rows = append(out.rows, g.configInput(s))
	add := func(kind string, id int64, v any) {
		b, _ := json.Marshal(v)
		out.rows = append(out.rows, inputRow{key: kind + "/" + strconv.FormatInt(id, 10), kind: kind, id: json.RawMessage(strconv.FormatInt(id, 10)), row: b})
	}
	pools := make([]string, 0, len(s.Obs))
	for p := range s.Obs {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	for _, p := range pools {
		for _, o := range s.Obs[p] {
			out.obs = append(out.obs, o.ID)
			add(InputObs, o.ID, o)
		}
	}
	for _, sp := range s.Spans {
		out.spans = append(out.spans, sp.AttemptID)
		add(InputSpan, sp.AttemptID, sp)
	}
	models := make([]string, 0, len(s.Samples))
	for m := range s.Samples {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		for _, sm := range s.Samples[m] {
			out.samples = append(out.samples, sm.AttemptID)
			add(InputSample, sm.AttemptID, sm)
		}
	}
	return out
}

// configInput is the budget_input row of the effective config (id = its
// sha256). mu is held.
func (g *Gate) configInput(s budget.Snapshot) inputRow {
	shaID, _ := json.Marshal(s.ConfigSHA256)
	return inputRow{key: InputConfig + "/" + s.ConfigSHA256, kind: InputConfig, id: shaID, row: []byte(g.cfgJSON)}
}

// logInputs writes a budget_input line for every row of ids not yet in this
// run's file (a span again when its row changed). mu is held.
func (g *Gate) logInputs(tx *sql.Tx, now time.Time, s budget.Snapshot, ids inputSet) error {
	for _, r := range ids.rows {
		dg := digestBytes(r.row)
		prev, had := g.m.logged[r.key]
		if had && prev == dg {
			continue
		}
		if had && r.kind != InputSpan {
			continue // obs, samples and the config are immutable: logged once
		}
		if _, _, err := g.event(tx, now, "", budgetlog.TypeInput, InputEvent{Kind: r.kind, ID: r.id, Row: json.RawMessage(r.row)}); err != nil {
			return err
		}
		g.m.logged[r.key] = dg
		key := r.key
		g.track(tx, func() {
			if had {
				g.m.logged[key] = prev
			} else {
				delete(g.m.logged, key)
			}
		})
	}
	return nil
}

// suppressKey names a refusal for the identical-refusal rule: rule, reason,
// input ids (spans with their row digest) and the open breakers.
func suppressKey(d budget.Decision, ids inputSet, brs []budget.Breaker) string {
	var b strings.Builder
	b.WriteString(d.Rule)
	b.WriteString("\x00")
	b.WriteString(d.Reason)
	b.WriteString("\x00")
	b.WriteString(d.Model)
	for _, r := range ids.rows {
		b.WriteString("\x00")
		b.WriteString(r.key)
		if r.kind == InputSpan {
			b.WriteString("=" + digestBytes(r.row))
		}
	}
	for _, br := range brs {
		fmt.Fprintf(&b, "\x00breaker/%d/%s", br.ID, br.State)
	}
	return digestBytes([]byte(b.String()))
}
