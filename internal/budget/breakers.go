package budget

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Breakers. EvalBreakers is pure: every input is in the
// BreakerInput, so `gosplan replay --check` can recompute each recorded change.
// VERIFIER_ERROR is not evaluated here: the reconciler owns that halt
// and the gate only logs it. Every threshold, window and cooldown is
// policy (Config).

// EvalBreakers is every breaker change one evaluation asks for,
// sorted by kind, then action.
func EvalBreakers(in BreakerInput) []BreakerChange {
	var out []BreakerChange
	if in.Run != "" {
		out = append(out, evalRetryStorm(in)...)
		if in.Task != "" {
			out = append(out, evalSameFailure(in)...)
		}
	}
	out = append(out, evalRateLimited(in)...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Breaker.Kind != out[j].Breaker.Kind {
			return out[i].Breaker.Kind < out[j].Breaker.Kind
		}
		return out[i].Action < out[j].Action
	})
	return out
}

// Signature is the SAME_FAILURE signature of a finished attempt:
// a refunded attempt is "other:<id>"; else "no-commit" when the head
// is empty or equals the base; else "gates:<sorted failed gates,
// comma-joined>" when the verdict is fail; else "other:<id>", which never
// equals another signature.
func Signature(a AttemptFact) string {
	other := "other:" + strconv.FormatInt(a.AttemptID, 10)
	switch {
	case a.Refunded:
		return other
	case a.HeadSHA == "" || a.HeadSHA == a.BaseSHA:
		return "no-commit"
	case a.Verdict == "fail":
		g := append([]string(nil), a.FailedGates...)
		sort.Strings(g)
		return "gates:" + strings.Join(g, ",")
	}
	return other
}

// current returns the current row of kind matching scope keys, or nil.
func current(rows []Breaker, kind, scope, run, task string) *Breaker {
	var best *Breaker
	for i := range rows {
		b := &rows[i]
		if b.Kind != kind || b.Scope != scope {
			continue
		}
		if scope != ScopeGlobal && b.Run != run {
			continue
		}
		if scope == ScopeTask && b.Task != task {
			continue
		}
		// Prefer an open row; among equals, the highest id.
		open, bestOpen := b.State == "open", best != nil && best.State == "open"
		if best == nil || (open && !bestOpen) || (open == bestOpen && b.ID > best.ID) {
			best = b
		}
	}
	return best
}

// closeIfDue closes an open breaker whose closes_at has been reached.
func closeIfDue(b *Breaker, now time.Time, enforced bool) []BreakerChange {
	if b == nil || b.State != "open" || b.ClosesAt == nil || now.Before(*b.ClosesAt) {
		return nil
	}
	c := *b
	c.State, c.ClosedAt, c.Enforced = "closed", timePtr(now), enforced
	c.Reason = fmt.Sprintf("%s closed: closes_at %s reached (now %s)", b.Kind, fmtTime(*b.ClosesAt), fmtTime(now))
	return []BreakerChange{{Action: "close", Breaker: c}}
}

// retries returns the attempts that are retries: consumed attempts started
// after a consumed attempt of the same task.
func retries(attempts []AttemptFact) []AttemptFact {
	sorted := append([]AttemptFact(nil), attempts...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].StartedAt.Equal(sorted[j].StartedAt) {
			return sorted[i].StartedAt.Before(sorted[j].StartedAt)
		}
		return sorted[i].AttemptID < sorted[j].AttemptID
	})
	consumed := map[string]bool{}
	var out []AttemptFact
	for _, a := range sorted {
		if a.Refunded {
			continue
		}
		if consumed[a.Task] {
			out = append(out, a)
		}
		consumed[a.Task] = true
	}
	return out
}

// evalRetryStorm: count retries started in [now − window,
// now] (and after the breaker's last close, so a close is not followed by an
// immediate re-open on the same retries); at ≥ retries open with closes_at
// = now + cooldown; close at closes_at.
func evalRetryStorm(in BreakerInput) []BreakerChange {
	c := in.Config
	enforced := in.Mode == ModeEnforce
	cur := current(in.Current, RetryStorm, ScopeRun, in.Run, "")
	if cur != nil && cur.State == "open" {
		return closeIfDue(cur, in.Now, enforced)
	}
	if c.BreakerRetryStormRetries <= 0 {
		return nil
	}
	from := in.Now.Add(-c.BreakerRetryStormWindow)
	if cur != nil && cur.ClosedAt != nil && cur.ClosedAt.After(from) {
		from = *cur.ClosedAt
	}
	n := 0
	for _, a := range retries(in.Attempts) {
		if !a.StartedAt.Before(from) && !a.StartedAt.After(in.Now) {
			n++
		}
	}
	if n < c.BreakerRetryStormRetries {
		return nil
	}
	closes := in.Now.Add(c.BreakerRetryStormCooldown)
	return []BreakerChange{{Action: "open", Breaker: Breaker{
		Kind: RetryStorm, Scope: ScopeRun, Run: in.Run, State: "open", Count: n,
		OpenedAt: timePtr(in.Now), ClosesAt: &closes, Enforced: enforced,
		Reason: fmt.Sprintf("RETRY_STORM: %d retries started since %s (window %s) >= %d; closes at %s (cooldown %s)",
			n, fmtTime(from), c.BreakerRetryStormWindow, c.BreakerRetryStormRetries, fmtTime(closes), c.BreakerRetryStormCooldown),
	}}}
}

// evalSameFailure evaluates in.Task: open (scope task, no
// closes_at, never closes on its own) when the last same_failure.attempts
// signatures of the task, by attempt id, are equal and not "other:".
func evalSameFailure(in BreakerInput) []BreakerChange {
	need := in.Config.BreakerSameFailureAttempts
	if need <= 0 {
		return nil
	}
	if cur := current(in.Current, SameFailure, ScopeTask, in.Run, in.Task); cur != nil && cur.State == "open" {
		return nil
	}
	var mine []AttemptFact
	for _, a := range in.Attempts {
		if a.Task == in.Task {
			mine = append(mine, a)
		}
	}
	if len(mine) < need {
		return nil
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].AttemptID < mine[j].AttemptID })
	last := mine[len(mine)-need:]
	sig := Signature(last[0])
	if strings.HasPrefix(sig, "other:") {
		return nil
	}
	ids := make([]string, len(last))
	for i, a := range last {
		if Signature(a) != sig {
			return nil
		}
		ids[i] = strconv.FormatInt(a.AttemptID, 10)
	}
	return []BreakerChange{{Action: "open", Breaker: Breaker{
		Kind: SameFailure, Scope: ScopeTask, Run: in.Run, Task: in.Task, State: "open", Count: need,
		OpenedAt: timePtr(in.Now), Enforced: in.Mode == ModeEnforce,
		Reason: fmt.Sprintf("SAME_FAILURE: %d attempts with %s (attempts %s)", need, sig, strings.Join(ids, ", ")),
	}}}
}

// evalRateLimited (global scope): open on a rejected or
// rate_limited status row newer than the current row's closed_at; closes_at
// = that row's resets_at (or the newest known future resets_at of its pool)
// + Jitter, else cooldown_base × 2^(count−1) capped at cooldown_max, where
// count is consecutive openings without an allowed row in between. Close at
// closes_at.
func evalRateLimited(in BreakerInput) []BreakerChange {
	c := in.Config
	enforced := in.Mode == ModeEnforce
	cur := current(in.Current, RateLimited, ScopeGlobal, "", "")
	if cur != nil && cur.State == "open" {
		return closeIfDue(cur, in.Now, enforced)
	}
	rows := append([]Obs(nil), in.Status...)
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].ObservedAt.Equal(rows[j].ObservedAt) {
			return rows[i].ObservedAt.Before(rows[j].ObservedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	var trig *Obs
	for i := range rows {
		o := &rows[i]
		if o.Status != "rejected" && o.Status != "rate_limited" || o.ObservedAt.After(in.Now) {
			continue
		}
		if cur != nil && cur.ClosedAt != nil && !o.ObservedAt.After(*cur.ClosedAt) {
			continue
		}
		trig = o // newest qualifying row wins (rows ascend)
	}
	if trig == nil {
		return nil
	}
	count := 1
	if cur != nil && cur.OpenedAt != nil {
		count = cur.Count + 1
		for _, o := range rows {
			if strings.HasPrefix(o.Status, "allowed") && o.ObservedAt.After(*cur.OpenedAt) && o.ObservedAt.Before(trig.ObservedAt) {
				count = 1
				break
			}
		}
	}
	var reset *int64
	if trig.ResetsAt != nil && *trig.ResetsAt > in.Now.Unix() {
		reset = trig.ResetsAt
	} else {
		for i := len(rows) - 1; i >= 0; i-- {
			o := rows[i]
			if o.Pool == trig.Pool && o.ResetsAt != nil && *o.ResetsAt > in.Now.Unix() {
				reset = o.ResetsAt
				break
			}
		}
	}
	var closes time.Time
	var basis string
	if reset != nil {
		at := time.Unix(*reset, 0).UTC()
		j := Jitter(fmt.Sprintf("%s|%d|%s", RateLimited, trig.ID, fmtTime(at)), c.Jitter)
		closes = at.Add(j)
		basis = fmt.Sprintf("resets_at %s + jitter %s", fmtTime(at), j)
	} else {
		d := c.BreakerRateLimitedCooldownBase
		for i := 1; i < count && d < c.BreakerRateLimitedCooldownMax; i++ {
			d *= 2
		}
		if d > c.BreakerRateLimitedCooldownMax {
			d = c.BreakerRateLimitedCooldownMax
		}
		closes = in.Now.Add(d)
		basis = fmt.Sprintf("no resets_at known: cooldown %s (base %s x 2^%d, max %s)", d, c.BreakerRateLimitedCooldownBase, count-1, c.BreakerRateLimitedCooldownMax)
	}
	return []BreakerChange{{Action: "open", Breaker: Breaker{
		Kind: RateLimited, Scope: ScopeGlobal, State: "open", Count: count,
		OpenedAt: timePtr(in.Now), ClosesAt: &closes, Enforced: enforced,
		Reason: fmt.Sprintf("RATE_LIMITED: observation %d (pool %s, status %s, observed %s); opening %d; closes at %s (%s)",
			trig.ID, trig.Pool, trig.Status, fmtTime(trig.ObservedAt), count, fmtTime(closes), basis),
	}}}
}

func timePtr(t time.Time) *time.Time { return &t }
