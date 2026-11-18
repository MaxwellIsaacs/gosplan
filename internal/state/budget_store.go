package state

// Budget store: the SQL behind the
// budget gate. Every writer takes the reconciler's *sql.Tx (it runs inside an
// apply); the read-only loaders take a budgetQuerier so `gosplan budget` can use
// them on a read-only store. Nothing here writes the tasks table: task
// changes (not_before included) go through the reconciler's txn only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

// budgetQuerier is *sql.Tx or *sql.DB.
type budgetQuerier interface {
	Query(q string, args ...any) (*sql.Rows, error)
	QueryRow(q string, args ...any) *sql.Row
	tasksQuerier
}

// Park rules that a settle re-arms.
var budgetRearmRules = []string{budget.RuleHeadroom, budget.RuleClassHold, budget.RuleFairness, budget.RuleBootstrap, budget.RuleStale}

// BudgetRunRow mirrors one budget_runs row.
type BudgetRunRow struct {
	Run          string
	Owner        string
	Mode         string
	MaxParallel  int
	Deadline     *time.Time
	CapsJSON     string // {"usd_estimate":x,"window":{"five_hour":0.3}}; "" = "{}"
	ConfigSHA256 string
	ConfigJSON   string
	HeartbeatAt  time.Time
	Halt         string // "", "drain", "stop"
	HaltReason   string
}

// ReservationRow mirrors one budget_reservations row.
type ReservationRow struct {
	ID           int64
	Run, Task    string
	AttemptID    int64
	Model        string
	Class        string
	CreatedAt    time.Time
	EstTokens    *float64
	EstBasis     string
	Pools        []budget.PoolReserve
	BeltUSD      *float64
	Enforced     bool
	State        string // open | settled
	SettledAt    *time.Time
	ActualTokens *int64
	Actual       *budget.Tokens
	ActualBasis  string // "", usage, unknown, orphan
	DecisionEID  *int64
}

// ParkRow mirrors one budget_parks row.
type ParkRow struct {
	Run, Task   string
	Rule        string
	Reason      string
	ParkedAt    time.Time
	AdmitAt     *time.Time
	NotBefore   time.Time
	DecisionEID int64
}

// SnapshotRequest names one decision: the candidate (Run, Task, Attempt,
// Model), the mode and effective config of the deciding run, and now.
type SnapshotRequest struct {
	Run, Task string
	Attempt   int
	Model     string
	Mode      string
	Config    budget.Config
	Now       time.Time
}

func budgetTS(t time.Time) string { return timestamp(t) }

func budgetParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("state: budget: bad timestamp %q: %w", s, err)
	}
	return t, nil
}

func budgetOptTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := budgetParseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func budgetOptTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return budgetTS(*t)
}

// budgetUsageTokens is usage as budget.Tokens; nil when usage is unknown
// (no result line: all four classes zero).
func budgetUsageTokens(u events.Usage) *budget.Tokens {
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreate == 0 {
		return nil
	}
	return &budget.Tokens{Input: float64(u.Input), Output: float64(u.Output), CacheRead: float64(u.CacheRead), CacheCreate: float64(u.CacheCreate)}
}

// budgetPool is window aliased to its pool.
func budgetPool(alias map[string]string, window string) string {
	if p, ok := alias[window]; ok && p != "" {
		return p
	}
	return window
}

// ---- runs ----

// UpsertBudgetRun inserts or replaces the run's budget_runs row (every
// column, halt included: a resumed run starts unhalted unless r says so).
func UpsertBudgetRun(tx *sql.Tx, r BudgetRunRow) error {
	caps := r.CapsJSON
	if caps == "" {
		caps = "{}"
	}
	var halt, reason any
	if r.Halt != "" {
		halt, reason = r.Halt, r.HaltReason
	}
	_, err := tx.Exec(`INSERT INTO budget_runs (run_id, owner, mode, max_parallel, deadline, caps_json, config_sha256, config_json, heartbeat_at, halt, halt_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET owner = excluded.owner, mode = excluded.mode, max_parallel = excluded.max_parallel,
			deadline = excluded.deadline, caps_json = excluded.caps_json, config_sha256 = excluded.config_sha256,
			config_json = excluded.config_json, heartbeat_at = excluded.heartbeat_at, halt = excluded.halt, halt_reason = excluded.halt_reason`,
		r.Run, r.Owner, r.Mode, r.MaxParallel, budgetOptTS(r.Deadline), caps, r.ConfigSHA256, r.ConfigJSON, budgetTS(r.HeartbeatAt), halt, reason)
	if err != nil {
		return fmt.Errorf("state: budget run %s: %w", r.Run, err)
	}
	return nil
}

// HeartbeatBudgetRun sets budget_runs.heartbeat_at. A missing row is an error.
func HeartbeatBudgetRun(tx *sql.Tx, run string, now time.Time) error {
	res, err := tx.Exec(`UPDATE budget_runs SET heartbeat_at = ? WHERE run_id = ?`, budgetTS(now), run)
	if err != nil {
		return fmt.Errorf("state: budget heartbeat %s: %w", run, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: budget heartbeat %s: %w", run, sql.ErrNoRows)
	}
	return nil
}

// SetHalt records a run halt (kind drain or stop). The first halt wins: a
// row that already has one is left as it is.
func SetHalt(tx *sql.Tx, run, kind, reason string) error {
	if kind != "drain" && kind != "stop" {
		return fmt.Errorf("state: budget halt %s: bad kind %q", run, kind)
	}
	if _, err := tx.Exec(`UPDATE budget_runs SET halt = ?, halt_reason = ? WHERE run_id = ? AND halt IS NULL`, kind, reason, run); err != nil {
		return fmt.Errorf("state: budget halt %s: %w", run, err)
	}
	return nil
}

// GetBudgetRun reads one budget_runs row (sql.ErrNoRows if none).
func GetBudgetRun(q budgetQuerier, run string) (BudgetRunRow, error) {
	rows, err := budgetLoadRuns(q, ` WHERE run_id = ?`, run)
	if err != nil {
		return BudgetRunRow{}, err
	}
	if len(rows) == 0 {
		return BudgetRunRow{}, fmt.Errorf("state: budget run %s: %w", run, sql.ErrNoRows)
	}
	return rows[0], nil
}

func budgetLoadRuns(q budgetQuerier, where string, args ...any) ([]BudgetRunRow, error) {
	rows, err := q.Query(`SELECT run_id, owner, mode, max_parallel, deadline, caps_json, config_sha256, config_json, heartbeat_at, halt, halt_reason
		FROM budget_runs`+where+` ORDER BY run_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: budget runs: %w", err)
	}
	defer rows.Close()
	var out []BudgetRunRow
	for rows.Next() {
		var r BudgetRunRow
		var deadline, halt, reason sql.NullString
		var hb string
		if err := rows.Scan(&r.Run, &r.Owner, &r.Mode, &r.MaxParallel, &deadline, &r.CapsJSON, &r.ConfigSHA256, &r.ConfigJSON, &hb, &halt, &reason); err != nil {
			return nil, err
		}
		if r.Deadline, err = budgetOptTime(deadline); err != nil {
			return nil, err
		}
		if r.HeartbeatAt, err = budgetParseTime(hb); err != nil {
			return nil, err
		}
		r.Halt, r.HaltReason = halt.String, reason.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- reservations ----

// InsertReservation writes the open reservation of an admitted attempt:
// pools and estimate from the decision, class from the run's
// budget_tasks row (default normal), eid = the admitting decision event
// (0 = none).
func InsertReservation(tx *sql.Tx, ref core.AttemptRef, d budget.Decision, enforced bool, eid int64, now time.Time) error {
	class, err := budgetTaskClass(tx, ref.Run, ref.Task)
	if err != nil {
		return err
	}
	pools := d.Reserve
	if pools == nil {
		pools = []budget.PoolReserve{}
	}
	pj, err := json.Marshal(pools)
	if err != nil {
		return err
	}
	basis := d.Estimate.TokensBasis
	if basis == "" {
		basis = "unknown"
	}
	var deid any
	if eid > 0 {
		deid = eid
	}
	if _, err := tx.Exec(`INSERT INTO budget_reservations (run_id, task_id, attempt_id, model, class, created_at, est_tokens, est_basis,
			pools_json, belt_usd, enforced, state, decision_eid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?)`,
		ref.Run, ref.Task, ref.AttemptID, d.Model, class, budgetTS(now), d.Estimate.Tokens, basis, string(pj), d.BeltUSD, enforced, deid); err != nil {
		return fmt.Errorf("state: budget reserve %s/%s#%d: %w", ref.Run, ref.Task, ref.Attempt, err)
	}
	return nil
}

func budgetTaskClass(q budgetQuerier, run, task string) (string, error) {
	var class string
	err := q.QueryRow(`SELECT class FROM budget_tasks WHERE run_id = ? AND task_id = ?`, run, task).Scan(&class)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return budget.ClassNormal, nil
	case err != nil:
		return "", fmt.Errorf("state: budget task %s/%s: %w", run, task, err)
	}
	return class, nil
}

// SettleReservation settles the open reservation of attemptID:
// actual tokens = the sum of the four usage classes (basis usage), or NULL
// with basis unknown when usage is nil. ok is false when the attempt has no
// open reservation (nothing changes). It returns the settled reservation
// and the actual tokens.
func SettleReservation(tx *sql.Tx, attemptID int64, usage *events.Usage, now time.Time) (budget.Reservation, *int64, bool, error) {
	rows, err := budgetLoadReservations(tx, ` WHERE attempt_id = ? AND state = 'open'`, attemptID)
	if err != nil || len(rows) == 0 {
		return budget.Reservation{}, nil, false, err
	}
	var actual *int64
	var actualJSON any
	basis := "unknown"
	if usage != nil {
		n := usage.Input + usage.Output + usage.CacheRead + usage.CacheCreate
		actual = &n
		b, err := json.Marshal(usage)
		if err != nil {
			return budget.Reservation{}, nil, false, err
		}
		actualJSON, basis = string(b), "usage"
	}
	if _, err := tx.Exec(`UPDATE budget_reservations SET state = 'settled', settled_at = ?, actual_tokens = ?, actual_json = ?, actual_basis = ?
		WHERE id = ? AND state = 'open'`, budgetTS(now), actual, actualJSON, basis, rows[0].ID); err != nil {
		return budget.Reservation{}, nil, false, fmt.Errorf("state: budget settle attempt %d: %w", attemptID, err)
	}
	r := rows[0]
	t := now
	r.State, r.SettledAt, r.ActualTokens, r.ActualBasis = "settled", &t, actual, basis
	return r.Reservation(), actual, true, nil
}

// SettleOrphans settles open reservations that are not live, actual_basis
// 'orphan', and returns them for the budget_settle events. A
// reservation is live while its attempt row has ended_at IS NULL and its
// run's budget_runs heartbeat is within runTTL of now.
func SettleOrphans(tx *sql.Tx, now time.Time, runTTL time.Duration) ([]budget.Reservation, error) {
	open, err := budgetLoadReservations(tx, ` WHERE state = 'open'`)
	if err != nil {
		return nil, err
	}
	live, err := budgetLiveFilter(tx, open, now, runTTL)
	if err != nil {
		return nil, err
	}
	var out []budget.Reservation
	for _, r := range open {
		if live[r.ID] {
			continue
		}
		if _, err := tx.Exec(`UPDATE budget_reservations SET state = 'settled', settled_at = ?, actual_tokens = NULL, actual_json = NULL,
			actual_basis = 'orphan' WHERE id = ? AND state = 'open'`, budgetTS(now), r.ID); err != nil {
			return nil, fmt.Errorf("state: budget orphan %d: %w", r.ID, err)
		}
		t := now
		r.State, r.SettledAt, r.ActualTokens, r.ActualBasis = "settled", &t, nil, "orphan"
		out = append(out, r.Reservation())
	}
	return out, nil
}

// budgetLiveFilter is the set of reservation ids (among rs) that are live.
func budgetLiveFilter(q budgetQuerier, rs []ReservationRow, now time.Time, runTTL time.Duration) (map[int64]bool, error) {
	hb, err := budgetHeartbeats(q)
	if err != nil {
		return nil, err
	}
	live := map[int64]bool{}
	for _, r := range rs {
		var ended sql.NullString
		err := q.QueryRow(`SELECT ended_at FROM attempts WHERE id = ?`, r.AttemptID).Scan(&ended)
		if errors.Is(err, sql.ErrNoRows) {
			continue // attempt row gone: not live
		}
		if err != nil {
			return nil, fmt.Errorf("state: budget live %d: %w", r.ID, err)
		}
		h, ok := hb[r.Run]
		if ended.Valid || !ok || now.Sub(h) > runTTL {
			continue
		}
		live[r.ID] = true
	}
	return live, nil
}

func budgetHeartbeats(q budgetQuerier) (map[string]time.Time, error) {
	runs, err := budgetLoadRuns(q, "")
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, r := range runs {
		out[r.Run] = r.HeartbeatAt
	}
	return out, nil
}

const budgetReservationSelect = `SELECT id, run_id, task_id, attempt_id, model, class, created_at, est_tokens, est_basis, pools_json,
	belt_usd, enforced, state, settled_at, actual_tokens, actual_json, actual_basis, decision_eid FROM budget_reservations`

func budgetLoadReservations(q budgetQuerier, where string, args ...any) ([]ReservationRow, error) {
	rows, err := q.Query(budgetReservationSelect+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: budget reservations: %w", err)
	}
	defer rows.Close()
	var out []ReservationRow
	for rows.Next() {
		var r ReservationRow
		var created, pools string
		var est, belt sql.NullFloat64
		var settled, actualJSON, basis sql.NullString
		var actual, eid sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Run, &r.Task, &r.AttemptID, &r.Model, &r.Class, &created, &est, &r.EstBasis, &pools,
			&belt, &r.Enforced, &r.State, &settled, &actual, &actualJSON, &basis, &eid); err != nil {
			return nil, err
		}
		if r.CreatedAt, err = budgetParseTime(created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(pools), &r.Pools); err != nil {
			return nil, fmt.Errorf("state: budget reservation %d: pools_json: %w", r.ID, err)
		}
		if est.Valid {
			v := est.Float64
			r.EstTokens = &v
		}
		if belt.Valid {
			v := belt.Float64
			r.BeltUSD = &v
		}
		if r.SettledAt, err = budgetOptTime(settled); err != nil {
			return nil, err
		}
		if actual.Valid {
			v := actual.Int64
			r.ActualTokens = &v
		}
		if actualJSON.Valid && actualJSON.String != "" {
			var u events.Usage
			if err := json.Unmarshal([]byte(actualJSON.String), &u); err != nil {
				return nil, fmt.Errorf("state: budget reservation %d: actual_json: %w", r.ID, err)
			}
			r.Actual = &budget.Tokens{Input: float64(u.Input), Output: float64(u.Output), CacheRead: float64(u.CacheRead), CacheCreate: float64(u.CacheCreate)}
		}
		r.ActualBasis = basis.String
		if eid.Valid {
			v := eid.Int64
			r.DecisionEID = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Reservation is the row as the pure package sees it.
func (r ReservationRow) Reservation() budget.Reservation {
	out := budget.Reservation{
		ID: r.ID, Run: r.Run, Task: r.Task, AttemptID: r.AttemptID, Model: r.Model, Class: r.Class,
		Pools: r.Pools, Enforced: r.Enforced, CreatedAt: r.CreatedAt, SettledAt: r.SettledAt,
	}
	if out.Pools == nil {
		out.Pools = []budget.PoolReserve{}
	}
	if r.ActualTokens != nil {
		v := float64(*r.ActualTokens)
		out.ActualTokens = &v
	}
	return out
}

// OpenReservations is every open reservation in the state dir (live or
// not), by id. Read-only view for the surfaces.
func OpenReservations(q budgetQuerier) ([]budget.Reservation, error) {
	rows, err := budgetLoadReservations(q, ` WHERE state = 'open'`)
	if err != nil {
		return nil, err
	}
	out := make([]budget.Reservation, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Reservation())
	}
	return out, nil
}

// RunReservations is every reservation row of run, by id.
func RunReservations(q budgetQuerier, run string) ([]ReservationRow, error) {
	return budgetLoadReservations(q, ` WHERE run_id = ?`, run)
}

// ---- parks ----

// Park upserts the task's budget_parks row from an enforced refusal. It
// writes budget_parks only; the reconciler sets tasks.not_before itself.
func Park(tx *sql.Tx, run, task string, d budget.Decision, eid int64, now time.Time) error {
	if d.NotBefore == nil {
		return fmt.Errorf("state: budget park %s/%s: decision has no not_before", run, task)
	}
	if _, err := tx.Exec(`INSERT INTO budget_parks (run_id, task_id, rule, reason, parked_at, admit_at, not_before, decision_eid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, task_id) DO UPDATE SET rule = excluded.rule, reason = excluded.reason, parked_at = excluded.parked_at,
			admit_at = excluded.admit_at, not_before = excluded.not_before, decision_eid = excluded.decision_eid`,
		run, task, d.Rule, d.Reason, budgetTS(now), budgetOptTS(d.AdmitAt), budgetTS(*d.NotBefore), eid); err != nil {
		return fmt.Errorf("state: budget park %s/%s: %w", run, task, err)
	}
	return nil
}

// RearmParks deletes the park rows of the re-armable rules (headroom,
// class_hold, fairness, bootstrap, stale), in every run. It never writes
// tasks. Returns how many rows it deleted.
func RearmParks(tx *sql.Tx) (int, error) {
	args := make([]any, len(budgetRearmRules))
	for i, r := range budgetRearmRules {
		args[i] = r
	}
	res, err := tx.Exec(`DELETE FROM budget_parks WHERE rule IN (?, ?, ?, ?, ?)`, args...)
	if err != nil {
		return 0, fmt.Errorf("state: budget rearm: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ParkedWithoutRow returns, in input order, the ids among tasks (the
// caller's own parked tasks of run) that have no budget_parks row any more.
func ParkedWithoutRow(q budgetQuerier, run string, tasks []string) ([]string, error) {
	var out []string
	for _, id := range tasks {
		var n int
		if err := q.QueryRow(`SELECT count(*) FROM budget_parks WHERE run_id = ? AND task_id = ?`, run, id).Scan(&n); err != nil {
			return nil, fmt.Errorf("state: budget parks %s/%s: %w", run, id, err)
		}
		if n == 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

// Parks lists the run's park rows by task id ("" = every run, by run then task).
func Parks(q budgetQuerier, run string) ([]ParkRow, error) {
	where, args := "", []any{}
	if run != "" {
		where, args = ` WHERE run_id = ?`, []any{run}
	}
	rows, err := q.Query(`SELECT run_id, task_id, rule, reason, parked_at, admit_at, not_before, decision_eid FROM budget_parks`+where+
		` ORDER BY run_id, task_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: budget parks: %w", err)
	}
	defer rows.Close()
	var out []ParkRow
	for rows.Next() {
		var p ParkRow
		var parked, nb string
		var admit sql.NullString
		if err := rows.Scan(&p.Run, &p.Task, &p.Rule, &p.Reason, &parked, &admit, &nb, &p.DecisionEID); err != nil {
			return nil, err
		}
		if p.ParkedAt, err = budgetParseTime(parked); err != nil {
			return nil, err
		}
		if p.AdmitAt, err = budgetOptTime(admit); err != nil {
			return nil, err
		}
		if p.NotBefore, err = budgetParseTime(nb); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- breakers ----

// ApplyBreakerChange writes one budget.EvalBreakers action. open: a new row
// (or the row ch.Breaker.ID names) set open with opened_at = now unless the
// breaker carries one. close: the row ch.Breaker.ID names, or the open row
// of the same kind/scope/run/task, set closed at now. count: the count of
// the row ch.Breaker.ID names (or the newest row of the same key), inserted
// when there is none. inputs is the BreakerInput JSON ("{}" when empty).
func ApplyBreakerChange(tx *sql.Tx, ch budget.BreakerChange, inputs []byte, now time.Time) error {
	b := ch.Breaker
	in := string(inputs)
	if in == "" {
		in = "{}"
	}
	scope := b.Scope
	if scope == "" {
		scope = budget.ScopeRun
	}
	find := func(where string) (int64, error) {
		if b.ID > 0 {
			return b.ID, nil
		}
		var id int64
		err := tx.QueryRow(`SELECT id FROM budget_breakers WHERE kind = ? AND scope = ? AND run_id = ? AND task_id = ?`+where+
			` ORDER BY id DESC LIMIT 1`, b.Kind, scope, b.Run, b.Task).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return id, err
	}
	insert := func(st string, opened *time.Time) error {
		_, err := tx.Exec(`INSERT INTO budget_breakers (kind, scope, run_id, task_id, state, count, opened_at, closes_at, closed_at, reason, enforced, inputs_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.Kind, scope, b.Run, b.Task, st, b.Count, budgetOptTS(opened), budgetOptTS(b.ClosesAt), budgetOptTS(b.ClosedAt), b.Reason, b.Enforced, in)
		return err
	}
	var err error
	switch ch.Action {
	case "open":
		opened := b.OpenedAt
		if opened == nil {
			opened = &now
		}
		if b.ID > 0 {
			_, err = tx.Exec(`UPDATE budget_breakers SET state = 'open', count = ?, opened_at = ?, closes_at = ?, closed_at = NULL, reason = ?,
				enforced = ?, inputs_json = ? WHERE id = ?`, b.Count, budgetTS(*opened), budgetOptTS(b.ClosesAt), b.Reason, b.Enforced, in, b.ID)
		} else {
			err = insert("open", opened)
		}
	case "close":
		var id int64
		if id, err = find(` AND state = 'open'`); err == nil && id > 0 {
			_, err = tx.Exec(`UPDATE budget_breakers SET state = 'closed', closed_at = ?, inputs_json = ? WHERE id = ?`, budgetTS(now), in, id)
		}
	case "count":
		var id int64
		if id, err = find(""); err == nil {
			if id > 0 {
				_, err = tx.Exec(`UPDATE budget_breakers SET count = ?, reason = ?, inputs_json = ? WHERE id = ?`, b.Count, b.Reason, in, id)
			} else {
				st := b.State
				if st != "open" {
					st = "closed"
				}
				err = insert(st, b.OpenedAt)
			}
		}
	default:
		return fmt.Errorf("state: budget breaker %s: unknown action %q", b.Kind, ch.Action)
	}
	if err != nil {
		return fmt.Errorf("state: budget breaker %s %s: %w", ch.Action, b.Kind, err)
	}
	return nil
}

// OpenBreakers lists every open breaker row, by id.
func OpenBreakers(q budgetQuerier) ([]budget.Breaker, error) {
	rows, err := q.Query(`SELECT id, kind, scope, run_id, task_id, state, count, opened_at, closes_at, closed_at, reason, enforced
		FROM budget_breakers WHERE state = 'open' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("state: budget breakers: %w", err)
	}
	defer rows.Close()
	out := []budget.Breaker{}
	for rows.Next() {
		var b budget.Breaker
		var opened, closes, closed sql.NullString
		if err := rows.Scan(&b.ID, &b.Kind, &b.Scope, &b.Run, &b.Task, &b.State, &b.Count, &opened, &closes, &closed, &b.Reason, &b.Enforced); err != nil {
			return nil, err
		}
		if b.OpenedAt, err = budgetOptTime(opened); err != nil {
			return nil, err
		}
		if b.ClosesAt, err = budgetOptTime(closes); err != nil {
			return nil, err
		}
		if b.ClosedAt, err = budgetOptTime(closed); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RunAttemptFacts is for resume only: the run's finished attempts as
// breaker facts, by id (live breaker inputs come from the gate's memory).
// start_failed and lost attempts are Refunded. BaseSHA is the
// task's base (every attempt of a task runs on the same base).
func RunAttemptFacts(q budgetQuerier, run string) ([]budget.AttemptFact, error) {
	rows, err := q.Query(`SELECT a.id, a.task_id, a.n, a.outcome, a.started_at, COALESCE(t.base_sha, ''), COALESCE(a.head_sha, ''),
			COALESCE(a.verdict_result, ''), a.failed_gates_json
		FROM attempts a JOIN tasks t ON t.run_id = a.run_id AND t.id = a.task_id
		WHERE a.run_id = ? AND a.ended_at IS NOT NULL ORDER BY a.id`, run)
	if err != nil {
		return nil, fmt.Errorf("state: budget attempt facts %s: %w", run, err)
	}
	defer rows.Close()
	var out []budget.AttemptFact
	for rows.Next() {
		var f budget.AttemptFact
		var outcome, gates sql.NullString
		var started string
		if err := rows.Scan(&f.AttemptID, &f.Task, &f.N, &outcome, &started, &f.BaseSHA, &f.HeadSHA, &f.Verdict, &gates); err != nil {
			return nil, err
		}
		if f.StartedAt, err = budgetParseTime(started); err != nil {
			return nil, err
		}
		f.Refunded = outcome.String == "start_failed" || outcome.String == "lost"
		if gates.Valid && gates.String != "" {
			if err := json.Unmarshal([]byte(gates.String), &f.FailedGates); err != nil {
				return nil, fmt.Errorf("state: budget attempt %d: failed_gates_json: %w", f.AttemptID, err)
			}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---- snapshot ----

// LoadSnapshot reads every input of one decision: obs per
// pool (aliases applied, max(lookback, fit.lookback)), newest status rows,
// spans and samples (all runs), live open reservations (open, attempt
// ended_at IS NULL, run heartbeat within run_ttl), Recent (settled within
// stale_after), open breakers, candidates across live, unhalted runs whatever
// their not_before, run states.
//
// Floors are the gate's memory (rows this process ingested itself), not the
// DB: LoadSnapshot leaves Floors empty and the caller fills it.
func LoadSnapshot(q budgetQuerier, req SnapshotRequest) (budget.Snapshot, error) {
	c := req.Config
	now := req.Now
	s := budget.Snapshot{
		Now: now, Mode: req.Mode, Config: c, ConfigSHA256: c.SHA256(),
		Obs: map[string][]budget.Obs{}, Samples: map[string][]budget.Sample{}, Floors: map[string]budget.Obs{},
		Status: []budget.Obs{}, Spans: []budget.Span{}, Open: []budget.Reservation{}, Recent: []budget.Reservation{},
		Candidates: []budget.Candidate{}, Runs: []budget.RunState{},
		For: budget.Key{Run: req.Run, Task: req.Task, Attempt: req.Attempt},
	}
	var err error
	if s.Obs, err = budgetLoadObs(q, c, now); err != nil {
		return s, err
	}
	if s.Status, err = budgetLoadStatus(q, c); err != nil {
		return s, err
	}
	if s.Spans, err = budgetLoadSpans(q, now, c.FitLookback); err != nil {
		return s, err
	}
	if s.Samples, err = budgetLoadSamples(q, c.EstimateWindow); err != nil {
		return s, err
	}
	open, err := budgetLoadReservations(q, ` WHERE state = 'open'`)
	if err != nil {
		return s, err
	}
	live, err := budgetLiveFilter(q, open, now, c.RunTTL)
	if err != nil {
		return s, err
	}
	for _, r := range open {
		if live[r.ID] {
			s.Open = append(s.Open, r.Reservation())
		}
	}
	settled, err := budgetLoadReservations(q, ` WHERE state = 'settled' AND actual_basis != 'orphan'`)
	if err != nil {
		return s, err
	}
	for _, r := range settled {
		if r.SettledAt != nil && !r.SettledAt.Before(now.Add(-c.StaleAfter)) && !r.SettledAt.After(now) {
			s.Recent = append(s.Recent, r.Reservation())
		}
	}
	budgetUnitForNewPools(s.Open, s.Obs, c)
	budgetUnitForNewPools(s.Recent, s.Obs, c)
	if s.Breakers, err = OpenBreakers(q); err != nil {
		return s, err
	}
	if err := budgetLoadRunsAndCandidates(q, req, &s, append(open, settled...)); err != nil {
		return s, err
	}
	return s, nil
}

// budgetUnitForNewPools gives a reservation a unit reserve in every pool that applies to its model
// and was first observed after the reservation was made. Such a pool did not
// exist when the attempt was admitted (the bootstrap rule admits with no pool
// at all), so the row has no entry for it, yet the attempt's use lands in it.
// Only the snapshot view is
// extended: the row (pools_json) is never rewritten, so the gate's own-row
// cross-check still compares what it wrote. rs is modified in place.
func budgetUnitForNewPools(rs []budget.Reservation, obs map[string][]budget.Obs, c budget.Config) {
	if c.ColdUnitFraction <= 0 {
		return
	}
	pools := make([]string, 0, len(obs))
	for p, rows := range obs {
		if len(rows) > 0 {
			pools = append(pools, p)
		}
	}
	sort.Strings(pools)
	for i := range rs {
		have := map[string]bool{}
		for _, pr := range rs[i].Pools {
			have[pr.Pool] = true
		}
		var add []budget.PoolReserve
		for _, p := range pools {
			if have[p] || !budget.Applies(p, rs[i].Model, c.ModelFamilies) || !obs[p][0].ObservedAt.After(rs[i].CreatedAt) {
				continue
			}
			add = append(add, budget.PoolReserve{Pool: p, Reserve: c.ColdUnitFraction, Basis: "unit"})
		}
		if len(add) == 0 {
			continue
		}
		ps := append(append([]budget.PoolReserve{}, rs[i].Pools...), add...)
		sort.Slice(ps, func(a, b int) bool { return ps[a].Pool < ps[b].Pool })
		rs[i].Pools = ps
	}
}

// budgetLoadObs: window_observations rows within max(lookback, fit.lookback),
// excluding unknown and _probe, keyed by pool, sorted by (observed_at, id).
func budgetLoadObs(q budgetQuerier, c budget.Config, now time.Time) (map[string][]budget.Obs, error) {
	lb := max(c.Lookback, c.FitLookback)
	rows, err := q.Query(`SELECT id, window, utilization, resets_at, status, source, documented, observed_at FROM window_observations
		WHERE window NOT IN ('unknown', '_probe') AND julianday(observed_at) >= julianday(?) AND julianday(observed_at) <= julianday(?)
		ORDER BY julianday(observed_at), id`, budgetTS(now.Add(-lb)), budgetTS(now))
	if err != nil {
		return nil, fmt.Errorf("state: budget obs: %w", err)
	}
	obs, err := budgetScanObs(rows, c)
	if err != nil {
		return nil, err
	}
	out := map[string][]budget.Obs{}
	for _, o := range obs {
		out[o.Pool] = append(out[o.Pool], o)
	}
	for p := range out {
		sort.SliceStable(out[p], func(i, j int) bool {
			a, b := out[p][i], out[p][j]
			if !a.ObservedAt.Equal(b.ObservedAt) {
				return a.ObservedAt.Before(b.ObservedAt)
			}
			return a.ID < b.ID
		})
	}
	return out, nil
}

// budgetLoadStatus: the newest row of every window (unknown included,
// _probe excluded), aliased, by (pool, id).
func budgetLoadStatus(q budgetQuerier, c budget.Config) ([]budget.Obs, error) {
	rows, err := q.Query(`SELECT id, window, utilization, resets_at, status, source, documented, observed_at FROM window_observations w
		WHERE window != '_probe' AND id = (SELECT id FROM window_observations WHERE window = w.window
			ORDER BY julianday(observed_at) DESC, id DESC LIMIT 1)`)
	if err != nil {
		return nil, fmt.Errorf("state: budget status: %w", err)
	}
	out, err := budgetScanObs(rows, c)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pool != out[j].Pool {
			return out[i].Pool < out[j].Pool
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func budgetScanObs(rows *sql.Rows, c budget.Config) ([]budget.Obs, error) {
	defer rows.Close()
	out := []budget.Obs{}
	for rows.Next() {
		var o budget.Obs
		var window, at string
		var util sql.NullFloat64
		var resets sql.NullInt64
		var status sql.NullString
		if err := rows.Scan(&o.ID, &window, &util, &resets, &status, &o.Source, &o.Documented, &at); err != nil {
			return nil, err
		}
		var err error
		if o.ObservedAt, err = budgetParseTime(at); err != nil {
			return nil, err
		}
		o.Pool = budgetPool(c.Alias, window)
		if util.Valid {
			v := util.Float64
			o.Utilization = &v
		}
		if resets.Valid {
			v := resets.Int64
			o.ResetsAt = &v
		}
		o.Status = status.String
		out = append(out, o)
	}
	return out, rows.Err()
}

// budgetLoadSpans: attempts of every run overlapping [now-lookback, now].
func budgetLoadSpans(q budgetQuerier, now time.Time, lookback time.Duration) ([]budget.Span, error) {
	rows, err := q.Query(`SELECT id, run_id, task_id, model, started_at, ended_at, usage_json FROM attempts
		WHERE julianday(started_at) <= julianday(?) AND (ended_at IS NULL OR julianday(ended_at) >= julianday(?)) ORDER BY id`,
		budgetTS(now), budgetTS(now.Add(-lookback)))
	if err != nil {
		return nil, fmt.Errorf("state: budget spans: %w", err)
	}
	defer rows.Close()
	out := []budget.Span{}
	for rows.Next() {
		var sp budget.Span
		var started string
		var ended, usage sql.NullString
		if err := rows.Scan(&sp.AttemptID, &sp.Run, &sp.Task, &sp.Model, &started, &ended, &usage); err != nil {
			return nil, err
		}
		if sp.Start, err = budgetParseTime(started); err != nil {
			return nil, err
		}
		if sp.End, err = budgetOptTime(ended); err != nil {
			return nil, err
		}
		if sp.End != nil && usage.Valid && usage.String != "" {
			var u events.Usage
			if err := json.Unmarshal([]byte(usage.String), &u); err != nil {
				return nil, fmt.Errorf("state: budget span %d: usage_json: %w", sp.AttemptID, err)
			}
			sp.Usage = budgetUsageTokens(u)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// budgetLoadSamples: per model, the newest `window` finished attempts with
// known usage (all runs), newest first.
func budgetLoadSamples(q budgetQuerier, window int) (map[string][]budget.Sample, error) {
	rows, err := q.Query(`SELECT id, model, usage_json, COALESCE(num_turns, 0), COALESCE(duration_ms, 0), COALESCE(cost_usd_estimate, 0)
		FROM attempts WHERE ended_at IS NOT NULL AND usage_json IS NOT NULL ORDER BY julianday(ended_at) DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("state: budget samples: %w", err)
	}
	defer rows.Close()
	out := map[string][]budget.Sample{}
	for rows.Next() {
		var sm budget.Sample
		var usage string
		if err := rows.Scan(&sm.AttemptID, &sm.Model, &usage, &sm.NumTurns, &sm.DurationMS, &sm.CostUSD); err != nil {
			return nil, err
		}
		if window > 0 && len(out[sm.Model]) >= window {
			continue
		}
		var u events.Usage
		if err := json.Unmarshal([]byte(usage), &u); err != nil {
			return nil, fmt.Errorf("state: budget sample %d: usage_json: %w", sm.AttemptID, err)
		}
		t := budgetUsageTokens(u)
		if t == nil {
			continue
		}
		sm.Usage = *t
		out[sm.Model] = append(out[sm.Model], sm)
	}
	return out, rows.Err()
}

type budgetCaps struct {
	USDEstimate *float64           `json:"usd_estimate"`
	Window      map[string]float64 `json:"window"`
}

// budgetLoadRunsAndCandidates fills s.Runs (every live run: status running,
// heartbeat within run_ttl; the requesting run always) and s.Candidates
// (pending, deps passed, any not_before, in live unhalted runs with a free
// slot; the requested task always). resv is every non-orphan reservation
// row (for spend and consumption).
func budgetLoadRunsAndCandidates(q budgetQuerier, req SnapshotRequest, s *budget.Snapshot, resv []ReservationRow) error {
	c, now := req.Config, req.Now
	brs, err := budgetLoadRuns(q, "")
	if err != nil {
		return err
	}
	segStart := budgetSegmentStarts(s, c, now)
	k := budgetRatios(s, c, now)
	for _, br := range brs {
		var status, startedAt string
		if err := q.QueryRow(`SELECT status, started_at FROM runs WHERE id = ?`, br.Run).Scan(&status, &startedAt); err != nil {
			return fmt.Errorf("state: budget run %s: %w", br.Run, err)
		}
		if br.Run != req.Run && (status != "running" || now.Sub(br.HeartbeatAt) > c.RunTTL) {
			continue
		}
		started, err := budgetParseTime(startedAt)
		if err != nil {
			return err
		}
		rs := budget.RunState{Run: br.Run, Mode: br.Mode, Deadline: br.Deadline, Halt: br.Halt, HaltReason: br.HaltReason,
			Attributable: map[string]float64{}, SettledTokens: map[string]float64{}, Pending: []string{}}
		var caps budgetCaps
		if br.CapsJSON != "" {
			if err := json.Unmarshal([]byte(br.CapsJSON), &caps); err != nil {
				return fmt.Errorf("state: budget run %s: caps_json: %w", br.Run, err)
			}
		}
		rs.CapUSD, rs.CapWindow = caps.USDEstimate, caps.Window
		if err := budgetRunSpend(q, &rs, resv, s, segStart, k); err != nil {
			return err
		}
		tasks, err := tasksLoadQ(context.Background(), q, ` WHERE run_id = ?`, br.Run)
		if err != nil {
			return err
		}
		status2 := map[string]string{}
		for _, t := range tasks {
			status2[t.ID] = t.Status
			switch t.Status {
			case core.StatusRunning, core.StatusVerifying:
				rs.Running++
			case core.StatusPending:
				rs.Pending = append(rs.Pending, t.ID)
			}
		}
		sort.Strings(rs.Pending)
		s.Runs = append(s.Runs, rs)
		freeSlot := rs.Running < br.MaxParallel
		for _, t := range tasks {
			isFor := br.Run == req.Run && t.ID == req.Task
			if !isFor && (t.Status != core.StatusPending || rs.Halt != "" || !freeSlot) {
				continue
			}
			ok := true
			for _, d := range t.Deps {
				if status2[d] != core.StatusPassed {
					ok = false
				}
			}
			if !ok && !isFor {
				continue
			}
			cand, err := budgetCandidate(q, req, br.Run, t, started)
			if err != nil {
				return err
			}
			s.Candidates = append(s.Candidates, cand)
		}
	}
	// The requesting run without a budget_runs row (or with a missing task)
	// still contributes its own candidate.
	found := false
	for _, cd := range s.Candidates {
		if cd.Key == s.For {
			found = true
		}
	}
	if !found {
		cd := budget.Candidate{Key: s.For, Class: budget.ClassNormal, Models: []string{req.Model}}
		if tasks, err := tasksLoadQ(context.Background(), q, ` WHERE run_id = ? AND id = ?`, req.Run, req.Task); err == nil && len(tasks) == 1 {
			var startedAt string
			if err := q.QueryRow(`SELECT started_at FROM runs WHERE id = ?`, req.Run).Scan(&startedAt); err == nil {
				started, _ := budgetParseTime(startedAt)
				if c2, err := budgetCandidate(q, req, req.Run, tasks[0], started); err == nil {
					cd = c2
				}
			}
		}
		s.Candidates = append(s.Candidates, cd)
	}
	return nil
}

// budgetCandidate builds one candidate. Models: the requested model for
// the requested task; for others, the prefer list when routing is on, else
// the model of the run's newest attempt, else the requested model (an
// approximation: another run's model policy is not in the DB).
func budgetCandidate(q budgetQuerier, req SnapshotRequest, run string, t TaskRow, started time.Time) (budget.Candidate, error) {
	cd := budget.Candidate{Key: budget.Key{Run: run, Task: t.ID}, Class: budget.ClassNormal, RunStartedAt: started, Tier: t.Tier}
	var class, prefer string
	var maxTurns sql.NullInt64
	err := q.QueryRow(`SELECT class, prefer_json, max_turns FROM budget_tasks WHERE run_id = ? AND task_id = ?`, run, t.ID).Scan(&class, &prefer, &maxTurns)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return cd, fmt.Errorf("state: budget task %s/%s: %w", run, t.ID, err)
	default:
		cd.Class, cd.MaxTurns = class, int(maxTurns.Int64)
	}
	var preferList []string
	if prefer != "" && prefer != "null" {
		if err := json.Unmarshal([]byte(prefer), &preferList); err != nil {
			return cd, fmt.Errorf("state: budget task %s/%s: prefer_json: %w", run, t.ID, err)
		}
	}
	isFor := run == req.Run && t.ID == req.Task
	switch {
	case isFor && req.Config.RoutingEnabled && len(preferList) > 0:
		cd.Models = preferList
	case isFor:
		cd.Models = []string{req.Model}
	case req.Config.RoutingEnabled && len(preferList) > 0:
		cd.Models = preferList
	default:
		var m string
		if err := q.QueryRow(`SELECT model FROM attempts WHERE run_id = ? ORDER BY id DESC LIMIT 1`, run).Scan(&m); err != nil || m == "" {
			m = req.Model
		}
		cd.Models = []string{m}
	}
	if isFor {
		cd.Attempt = req.Attempt
	} else {
		var n int
		if err := q.QueryRow(`SELECT COALESCE(MAX(n), 0) FROM attempts WHERE run_id = ? AND task_id = ?`, run, t.ID).Scan(&n); err != nil {
			return cd, err
		}
		cd.Attempt = n + 1
	}
	return cd, nil
}

// budgetSegmentStarts is, per pool, the start of its current reset segment
// (budget.Segments; no segments known -> now - lookback).
func budgetSegmentStarts(s *budget.Snapshot, c budget.Config, now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for pool, rows := range s.Obs {
		start := now.Add(-c.Lookback)
		if segs := budget.Segments(rows, c.ResetEpsilon); len(segs) > 0 && len(segs[len(segs)-1]) > 0 {
			start = segs[len(segs)-1][0].ObservedAt
		}
		out[pool] = start
	}
	return out
}

// budgetRatios is, per pool, the fitted token->utilisation ratio when the
// fit has one (basis "fit", k > 0).
func budgetRatios(s *budget.Snapshot, c budget.Config, now time.Time) map[string]float64 {
	out := map[string]float64{}
	for pool, rows := range s.Obs {
		f := budget.FitRatio(pool, budget.Intervals(pool, rows, s.Spans, c.ModelFamilies, c.ResetEpsilon, now, c.FitLookback), c)
		if f.Basis == "fit" && f.KPerToken > 0 {
			out[pool] = f.KPerToken
		}
	}
	return out
}

// budgetRunSpend fills SpentUSD (settled cost + open belts),
// SettledTokens (settled tokens in each pool's current segment, unknown at
// the estimate) and Attributable (k × actual settled + reserve of
// open, or without a ratio the level's rise since the run's first
// reservation in the segment).
func budgetRunSpend(q budgetQuerier, rs *budget.RunState, resv []ReservationRow, s *budget.Snapshot, segStart map[string]time.Time, k map[string]float64) error {
	first := map[string]time.Time{}
	for _, r := range resv {
		if r.Run != rs.Run {
			continue
		}
		if r.State == "open" {
			if r.BeltUSD != nil {
				rs.SpentUSD += *r.BeltUSD
			}
		} else {
			var cost sql.NullFloat64
			if err := q.QueryRow(`SELECT cost_usd_estimate FROM attempts WHERE id = ?`, r.AttemptID).Scan(&cost); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("state: budget spend %d: %w", r.AttemptID, err)
			}
			rs.SpentUSD += cost.Float64
		}
		tokens := 0.0
		switch {
		case r.ActualTokens != nil:
			tokens = float64(*r.ActualTokens)
		case r.EstTokens != nil:
			tokens = *r.EstTokens
		}
		for pool, start := range segStart {
			if r.State == "settled" && r.SettledAt != nil && !r.SettledAt.Before(start) {
				rs.SettledTokens[pool] += tokens
				if kp, ok := k[pool]; ok {
					rs.Attributable[pool] += kp * tokens
				}
			}
			if !r.CreatedAt.Before(start) {
				if f, ok := first[pool]; !ok || r.CreatedAt.Before(f) {
					first[pool] = r.CreatedAt
				}
			}
			if r.State == "open" {
				if _, ok := k[pool]; ok {
					for _, pr := range r.Pools {
						if pr.Pool == pool {
							rs.Attributable[pool] += pr.Reserve
						}
					}
				}
			}
		}
	}
	for pool, f := range first {
		if _, ok := k[pool]; ok {
			continue
		}
		// at: the level when the run first reserved (newest row at or before
		// it, else the first row after); last: the newest level.
		var at, last *float64
		for i := range s.Obs[pool] {
			o := s.Obs[pool][i]
			if o.Utilization == nil {
				continue
			}
			if !o.ObservedAt.After(f) || at == nil {
				at = o.Utilization // rows are sorted: the last one at or before f, else the first after
			}
			last = o.Utilization
		}
		if at != nil && last != nil && *last > *at {
			rs.Attributable[pool] = *last - *at
		}
	}
	return nil
}
