package budgetrt

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// Memory cross-checks. A difference is an
// error wrapping reconcile.ErrBudgetTampered. mu is held.

// checkOwn runs cross-checks 2–4 (and 1 when tasks is set) in tx.
func (g *Gate) checkOwn(tx *sql.Tx, tasks bool) error {
	if tasks {
		if err := g.checkTasks(tx); err != nil {
			return err
		}
	}
	if err := g.checkRun(tx); err != nil {
		return err
	}
	if err := g.checkReservations(tx); err != nil {
		return err
	}
	if err := g.checkParks(tx); err != nil {
		return err
	}
	return g.checkBreakers(tx)
}

// checkTasks is cross-check 1: the run's budget_tasks rows equal the spec in memory.
func (g *Gate) checkTasks(tx *sql.Tx) error {
	db, err := readBudgetTasks(tx, g.o.Run)
	if err != nil {
		return err
	}
	for id, m := range g.m.tasks {
		d, ok := db[id]
		if !ok {
			return tampered("budget_tasks row %s/%s is gone", g.o.Run, id)
		}
		if !reflect.DeepEqual(d, m) {
			return tampered("budget_tasks row %s/%s changed: db=%+v mem=%+v", g.o.Run, id, d, m)
		}
	}
	for id := range db {
		if _, ok := g.m.tasks[id]; !ok {
			return tampered("budget_tasks row %s/%s appeared", g.o.Run, id)
		}
	}
	return nil
}

// checkRun is cross-check 2: own budget_runs row (heartbeat excluded).
func (g *Gate) checkRun(tx *sql.Tx) error {
	r, err := state.GetBudgetRun(tx, g.o.Run)
	if errors.Is(err, sql.ErrNoRows) {
		return tampered("budget_runs row %s is gone", g.o.Run)
	}
	if err != nil {
		return err
	}
	m := g.m.run
	var diffs []string
	field := func(name string, db, mem any) {
		if !reflect.DeepEqual(db, mem) {
			diffs = append(diffs, fmt.Sprintf("%s db=%v mem=%v", name, db, mem))
		}
	}
	field("owner", r.Owner, m.Owner)
	field("mode", r.Mode, m.Mode)
	field("max_parallel", r.MaxParallel, m.MaxParallel)
	if !sameTime(r.Deadline, m.Deadline) {
		diffs = append(diffs, fmt.Sprintf("deadline db=%v mem=%v", r.Deadline, m.Deadline))
	}
	field("caps_json", canonString(r.CapsJSON), canonString(m.CapsJSON))
	field("config_sha256", r.ConfigSHA256, m.ConfigSHA256)
	field("config_json", r.ConfigJSON, m.ConfigJSON)
	field("halt", r.Halt, g.m.halt.Kind)
	if g.m.halt.Kind != "" {
		field("halt_reason", r.HaltReason, g.m.halt.Reason)
	}
	if len(diffs) > 0 {
		return tampered("budget_runs row %s changed: %s", g.o.Run, strings.Join(diffs, ", "))
	}
	return nil
}

// checkReservations is cross-check 3 (reservations): every own open
// reservation exists, is open (or was settled orphan by a run that found it
// not live), and keeps its pools_json and enforced flag; no own open row
// appears that this process did not write.
func (g *Gate) checkReservations(tx *sql.Tx) error {
	rows, err := state.RunReservations(tx, g.o.Run)
	if err != nil {
		return err
	}
	byAttempt := map[int64]state.ReservationRow{}
	for _, r := range rows {
		byAttempt[r.AttemptID] = r
		if r.State == "open" {
			if m, ok := g.m.res[r.AttemptID]; !ok || !m.open {
				return tampered("open reservation %d (attempt %d) appeared", r.ID, r.AttemptID)
			}
		}
	}
	for aid, m := range g.m.res {
		if !m.open {
			continue
		}
		r, ok := byAttempt[aid]
		if !ok {
			return tampered("reservation of attempt %d is gone", aid)
		}
		pj, err := poolsJSON(r.Pools)
		if err != nil {
			return err
		}
		if pj != m.pools || r.Enforced != m.enforced {
			return tampered("reservation %d (attempt %d) changed: pools db=%s mem=%s enforced db=%v mem=%v", r.ID, aid, pj, m.pools, r.Enforced, m.enforced)
		}
		if r.State != "open" {
			if r.ActualBasis != "orphan" {
				return tampered("reservation %d (attempt %d) settled %q behind this process", r.ID, aid, r.ActualBasis)
			}
			// Another run settled it as an orphan: accepted.
			m.open = false
		}
	}
	return nil
}

// checkParks is cross-check 3 (parks): a park row may disappear (any run's
// re-arm deletes it), never change or appear.
func (g *Gate) checkParks(tx *sql.Tx) error {
	rows, err := state.Parks(tx, g.o.Run)
	if err != nil {
		return err
	}
	for _, p := range rows {
		m, ok := g.m.parks[p.Task]
		if !ok {
			return tampered("park row %s/%s appeared", g.o.Run, p.Task)
		}
		if p.Rule != m.rule || p.Reason != m.reason || p.DecisionEID != m.eid {
			return tampered("park row %s/%s changed: rule db=%s mem=%s, eid db=%d mem=%d", g.o.Run, p.Task, p.Rule, m.rule, p.DecisionEID, m.eid)
		}
	}
	return nil
}

// checkBreakers is cross-check 4: own RETRY_STORM and SAME_FAILURE rows.
func (g *Gate) checkBreakers(tx *sql.Tx) error {
	rows, err := ownBreakerRows(tx, g.o.Run)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, b := range rows {
		seen[b.ID] = true
		m, ok := g.m.breakers[b.ID]
		if !ok {
			return tampered("breaker row %d (%s) appeared", b.ID, b.Kind)
		}
		if b.Kind != m.Kind || b.Scope != m.Scope || b.Task != m.Task || b.State != m.State || !sameTime(b.ClosesAt, m.ClosesAt) {
			return tampered("breaker row %d (%s) changed: state db=%s mem=%s", b.ID, b.Kind, b.State, m.State)
		}
	}
	for id, m := range g.m.breakers {
		if !seen[id] {
			return tampered("breaker row %d (%s) is gone", id, m.Kind)
		}
	}
	return nil
}

const breakerCols = `id, kind, scope, run_id, task_id, state, count, opened_at, closes_at, closed_at, reason, enforced`

// ownBreakerRows: the run's RETRY_STORM (run scope) and SAME_FAILURE (task
// scope) rows, by id.
func ownBreakerRows(q *sql.Tx, run string) ([]budget.Breaker, error) {
	return scanBreakers(q.Query(`SELECT `+breakerCols+` FROM budget_breakers
		WHERE run_id = ? AND ((kind = 'RETRY_STORM' AND scope = 'run') OR (kind = 'SAME_FAILURE' AND scope = 'task')) ORDER BY id`, run))
}

// rateLimitedRows: the newest global RATE_LIMITED rows, open or closed
// (DB-authoritative: shared across runs).
func rateLimitedRows(q *sql.Tx) ([]budget.Breaker, error) {
	return scanBreakers(q.Query(`SELECT ` + breakerCols + ` FROM budget_breakers
		WHERE kind = 'RATE_LIMITED' AND scope = 'global' ORDER BY id DESC LIMIT 16`))
}

func scanBreakers(rows *sql.Rows, err error) ([]budget.Breaker, error) {
	if err != nil {
		return nil, fmt.Errorf("budgetrt: breakers: %w", err)
	}
	defer rows.Close()
	out := []budget.Breaker{}
	for rows.Next() {
		var b budget.Breaker
		var opened, closes, closed sql.NullString
		if err := rows.Scan(&b.ID, &b.Kind, &b.Scope, &b.Run, &b.Task, &b.State, &b.Count, &opened, &closes, &closed, &b.Reason, &b.Enforced); err != nil {
			return nil, err
		}
		if b.OpenedAt, err = optTime(opened); err != nil {
			return nil, err
		}
		if b.ClosesAt, err = optTime(closes); err != nil {
			return nil, err
		}
		if b.ClosedAt, err = optTime(closed); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, rows.Err()
}

func optTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, ns.String)
	if err != nil {
		return nil, fmt.Errorf("budgetrt: bad timestamp %q: %w", ns.String, err)
	}
	return &t, nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func canonString(s string) string {
	if s == "" {
		s = "{}"
	}
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
