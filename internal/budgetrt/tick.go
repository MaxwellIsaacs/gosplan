package budgetrt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/driver"
	"github.com/MaxwellIsaacs/gosplan/internal/reconcile"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// Halt reasons the gate stores (the reconciler appends ": parked <ids>").
const (
	HaltCapUSD    = "cap:usd_estimate"
	HaltCapWindow = "cap:window:" // + pool
	HaltDeadline  = reconcile.ReasonDeadline
)

// Tick implements reconcile.Budget, outside any tx: in
// one tx it heartbeats budget_runs, runs the own-row cross-checks, closes
// due breakers and evaluates RETRY_STORM and RATE_LIMITED, applies the run
// flags (caps → drain, deadline → stop; the first halt wins and is stored in
// budget_runs.halt), and decides whether the /usage probe is wanted; then it
// flushes budget.jsonl and starts the probe (at most one, outside every tx).
// It returns the stored halt.
func (g *Gate) Tick(ctx context.Context, now time.Time) (budget.Halt, error) {
	if now.IsZero() {
		now = g.Now()
	}
	probe := false
	err := g.own(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		if err := state.HeartbeatBudgetRun(tx, g.o.Run, now); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return tampered("budget_runs row %s is gone", g.o.Run)
			}
			return err
		}
		if err := g.checkOwn(tx, true); err != nil {
			return err
		}
		s, err := g.snapshot(tx, "", 0, g.o.Model, now)
		if err != nil {
			return err
		}
		if _, err := g.evalBreakers(tx, now, s.Status, "", nil); err != nil {
			return err
		}
		if g.m.halt.Kind == "" {
			if h := g.runFlagHalt(s, now); h.Kind != "" {
				if err := state.SetHalt(tx, g.o.Run, h.Kind, h.Reason); err != nil {
					return err
				}
				h.Parked = ownPending(s, g.o.Run)
				if _, _, err := g.event(tx, now, "", budgetlog.TypeHalt, h); err != nil {
					return err
				}
				g.m.halt = h
				g.track(tx, func() { g.m.halt = budget.Halt{} })
			}
		}
		probe = g.prober != nil && g.m.halt.Kind == "" && g.staleApplicable(s, now)
		return nil
	})
	if err != nil {
		return g.Halt(), err
	}
	if ferr := g.Flush(context.WithoutCancel(ctx)); ferr != nil {
		fmt.Fprintf(g.o.Stderr, "gosplan run: warning: budget: flush %s: %v\n", g.logPath, ferr)
	}
	if probe {
		g.startProbe(ctx)
	}
	return g.Halt(), nil
}

// runFlagHalt applies the run flags: the USD estimate
// cap and the window caps drain; the deadline stops, and so does the early
// end (nothing running or verifying and every pending task's last decision
// refused it for the deadline). mu is held.
func (g *Gate) runFlagHalt(s budget.Snapshot, now time.Time) budget.Halt {
	if g.o.CapUSD != nil && g.spentUSD() >= *g.o.CapUSD {
		return budget.Halt{Kind: "drain", Reason: HaltCapUSD}
	}
	if len(g.o.CapWindow) > 0 {
		var rs *budget.RunState
		for i := range s.Runs {
			if s.Runs[i].Run == g.o.Run {
				rs = &s.Runs[i]
			}
		}
		pools := make([]string, 0, len(g.o.CapWindow))
		for p := range g.o.CapWindow {
			pools = append(pools, p)
		}
		sort.Strings(pools)
		for _, p := range pools {
			if rs != nil && rs.Attributable[p] >= g.o.CapWindow[p] {
				return budget.Halt{Kind: "drain", Reason: HaltCapWindow + p}
			}
		}
	}
	if g.o.Deadline != nil {
		if !now.Before(*g.o.Deadline) {
			return budget.Halt{Kind: "stop", Reason: HaltDeadline}
		}
		for _, rs := range s.Runs {
			if rs.Run != g.o.Run || rs.Running > 0 || len(rs.Pending) == 0 {
				continue
			}
			all := true
			for _, id := range rs.Pending {
				if ld := g.m.last[id]; ld == nil || ld.rule != budget.RuleDeadline {
					all = false
				}
			}
			if all {
				return budget.Halt{Kind: "stop", Reason: HaltDeadline}
			}
		}
	}
	return budget.Halt{}
}

func ownPending(s budget.Snapshot, run string) []string {
	for _, rs := range s.Runs {
		if rs.Run == run {
			return append([]string{}, rs.Pending...)
		}
	}
	return []string{}
}

// staleApplicable reports whether a pool that applies to a pending task of
// this run is stale, or no observed pool applies at all. The
// staleness itself is budget.ViewPool's. mu is held.
func (g *Gate) staleApplicable(s budget.Snapshot, now time.Time) bool {
	pending := ownPending(s, g.o.Run)
	if len(pending) == 0 {
		return false
	}
	models := map[string]bool{}
	if g.o.Model != "" {
		models[g.o.Model] = true
	}
	for _, c := range s.Candidates {
		if c.Run == g.o.Run && c.Key != s.For {
			for _, m := range c.Models {
				if m != "" {
					models[m] = true
				}
			}
		}
	}
	c := g.o.Config
	for m := range models {
		applicable := 0
		for pool, rows := range s.Obs {
			if !budget.Applies(pool, m, c.ModelFamilies) {
				continue
			}
			applicable++
			var floor *budget.Obs
			if f, ok := s.Floors[pool]; ok {
				floor = &f
			}
			if budget.ViewPool(pool, rows, floor, budget.Fit{}, now, c).Stale {
				return true
			}
		}
		if applicable == 0 {
			return true
		}
	}
	return false
}

// startProbe runs the /usage probe in the background when none is running
// and window.Prober says it is due (MaxAge = stale_after, MinInterval =
// probe_min_interval). Its rows feed the floors; a budget_probe event
// records it.
func (g *Gate) startProbe(ctx context.Context) {
	g.probeMu.Lock()
	if g.probing {
		g.probeMu.Unlock()
		return
	}
	g.probing = true
	g.probeWG.Add(1)
	g.probeMu.Unlock()
	pctx := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			g.probeMu.Lock()
			g.probing = false
			g.probeMu.Unlock()
			g.probeWG.Done()
		}()
		ev := ProbeEvent{}
		due, err := g.prober.Due(pctx, g.store)
		if err != nil {
			ev.Error = err.Error()
		} else if !due {
			return // the min interval (or a fresh row) holds it back: nothing to record
		} else {
			ev.Due = true
			rows, perr := g.prober.Probe(pctx, g.store)
			for _, r := range rows {
				if r.Window != "_probe" {
					ev.Rows++
				}
				g.Observed(r)
			}
			ev.Ran = perr == nil
			if perr != nil {
				ev.Error = perr.Error()
			}
		}
		now := g.Now()
		if err := g.own(pctx, func(tx *sql.Tx) error {
			_, _, err := g.event(tx, now, "", budgetlog.TypeProbe, ev)
			return err
		}); err != nil {
			fmt.Fprintf(g.o.Stderr, "gosplan run: warning: budget: probe event: %v\n", err)
		}
	}()
}

// Rearm implements reconcile.Budget: this run's parked tasks (memory) whose
// budget_parks row is gone (a settle in any run re-armed them) get not_before
// = now + Jitter and are forgotten. The reconciler writes not_before.
func (g *Gate) Rearm(tx *sql.Tx, now time.Time) (map[string]time.Time, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.resolve(tx); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(g.m.parks))
	for id := range g.m.parks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	gone, err := state.ParkedWithoutRow(tx, g.o.Run, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]time.Time, len(gone))
	for _, id := range gone {
		key := fmt.Sprintf("%s|%s|rearm|%s", g.o.Run, id, now.UTC().Format(time.RFC3339Nano))
		out[id] = now.Add(budget.Jitter(key, g.o.Config.Jitter))
		g.forgetPark(tx, id)
	}
	return out, nil
}

// Quiet implements reconcile.Budget: the api_retry quiet allowance, capped
// at --stall-quiet-max.
func (g *Gate) Quiet(line []byte) time.Duration {
	max := g.o.QuietMax
	if max <= 0 {
		max = driver.DefaultStallQuietMax
	}
	return driver.APIRetryQuiet(line, max)
}

// SelfCheck implements reconcile.Budget: flush, re-read
// budget.jsonl and compare its decision events with the digests of the
// decisions this process wrote. Any missing, extra or changed one is
// "state_inconsistent: budget.jsonl: …". An edit after the run ends is not
// detected.
func (g *Gate) SelfCheck(ctx context.Context) string {
	g.probeWG.Wait()
	ctx = context.WithoutCancel(ctx)
	if err := g.own(ctx, func(*sql.Tx) error { return nil }); err != nil {
		return fmt.Sprintf("%s: budget.jsonl: %v", reconcile.ReasonInconsistent, err)
	}
	if err := g.Flush(ctx); err != nil {
		return fmt.Sprintf("%s: budget.jsonl: flush: %v", reconcile.ReasonInconsistent, err)
	}
	evs, err := budgetlog.ReadFile(g.logPath)
	if err != nil {
		return fmt.Sprintf("%s: budget.jsonl: %v", reconcile.ReasonInconsistent, err)
	}
	g.mu.Lock()
	want := make(map[int64]string, len(g.m.digests))
	for k, v := range g.m.digests {
		want[k] = v
	}
	start := g.m.startEID
	g.mu.Unlock()
	return compareDigests(evs, g.o.Run, start, want)
}

func compareDigests(evs []budgetlog.Event, run string, start int64, want map[int64]string) string {
	got := map[int64]string{}
	var problems []string
	for _, ev := range evs {
		if ev.Type != budgetlog.TypeDecision || ev.Run != run {
			continue
		}
		var e struct {
			EID *int64 `json:"eid"`
		}
		if err := json.Unmarshal(ev.Data, &e); err != nil || e.EID == nil {
			problems = append(problems, fmt.Sprintf("decision seq %d has no eid", ev.Seq))
			continue
		}
		if *e.EID < start {
			continue // an earlier process's decision (resume)
		}
		dg, err := Digest(ev.Data)
		if err != nil {
			problems = append(problems, fmt.Sprintf("decision eid %d: %v", *e.EID, err))
			continue
		}
		if _, dup := got[*e.EID]; dup {
			problems = append(problems, fmt.Sprintf("decision eid %d appears twice", *e.EID))
		}
		got[*e.EID] = dg
	}
	ids := make([]int64, 0, len(want)+len(got))
	for id := range want {
		ids = append(ids, id)
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		w, inMem := want[id]
		f, inFile := got[id]
		switch {
		case !inFile:
			problems = append(problems, fmt.Sprintf("decision eid %d missing", id))
		case !inMem:
			problems = append(problems, fmt.Sprintf("decision eid %d was not written by this process", id))
		case w != f:
			problems = append(problems, fmt.Sprintf("decision eid %d changed", id))
		}
	}
	if len(problems) == 0 {
		return ""
	}
	if len(problems) > 5 {
		problems = append(problems[:5], fmt.Sprintf("and %d more", len(problems)-5))
	}
	return fmt.Sprintf("%s: budget.jsonl: %s", reconcile.ReasonInconsistent, strings.Join(problems, "; "))
}
