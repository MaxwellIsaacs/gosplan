package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

// AttemptStart is the input of StartAttempt. BaseSHA and Branch go into the
// launch event, so the log records the base each attempt started from.
type AttemptStart struct {
	Run, Task, SessionID, Model, Owner, StreamPath string
	BaseSHA, Branch                                string
	Nonce                                          string // GOSPLAN_ATTEMPT_NONCE, recorded in the lease before launch
}

// NextAttempt is the number the task's next attempt gets: one more than
// its highest attempt row. It can exceed attempts_used + 1, because an
// attempt whose worker never started or was lost to an orchestrator fault
// gives its count back.
func NextAttempt(tx *sql.Tx, run, task string) (int, error) {
	var n int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(n), 0) FROM attempts WHERE run_id = ? AND task_id = ?`, run, task).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: next attempt %s/%s: %w", run, task, err)
	}
	return n + 1, nil
}

// CountInfraAttempts is how many of the task's attempts ended lost or
// start_failed: refunded attempts, which dispatch bounds separately.
func CountInfraAttempts(tx *sql.Tx, run, task string) (int, error) {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM attempts WHERE run_id = ? AND task_id = ? AND outcome IN ('lost', 'start_failed')`,
		run, task).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: count infra attempts %s/%s: %w", run, task, err)
	}
	return n, nil
}

// StartAttempt (inside the dispatch tx): n = NextAttempt, inserts the
// attempt row and the lease row (heartbeat_at = acquired_at = now),
// increments tasks.attempts_used and writes a `launch` event. It fails if
// the task has no row or a lease already exists for it.
func StartAttempt(tx *sql.Tx, now time.Time, a AttemptStart) (core.AttemptRef, error) {
	var used int
	if err := tx.QueryRow(`SELECT attempts_used FROM tasks WHERE run_id = ? AND id = ?`, a.Run, a.Task).Scan(&used); err != nil {
		return core.AttemptRef{}, fmt.Errorf("state: start attempt %s/%s: %w", a.Run, a.Task, err)
	}
	var leases int
	if err := tx.QueryRow(`SELECT count(*) FROM leases WHERE run_id = ? AND task_id = ?`, a.Run, a.Task).Scan(&leases); err != nil {
		return core.AttemptRef{}, fmt.Errorf("state: start attempt %s/%s: %w", a.Run, a.Task, err)
	}
	if leases > 0 {
		return core.AttemptRef{}, fmt.Errorf("state: start attempt %s/%s: lease already exists", a.Run, a.Task)
	}
	n, err := NextAttempt(tx, a.Run, a.Task)
	if err != nil {
		return core.AttemptRef{}, err
	}
	ts := timestamp(now)
	res, err := tx.Exec(`INSERT INTO attempts (run_id, task_id, n, session_id, model, started_at, stream_path)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, a.Run, a.Task, n, a.SessionID, a.Model, ts, a.StreamPath)
	if err != nil {
		return core.AttemptRef{}, fmt.Errorf("state: start attempt %s/%s: %w", a.Run, a.Task, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.AttemptRef{}, fmt.Errorf("state: start attempt %s/%s: %w", a.Run, a.Task, err)
	}
	if _, err := tx.Exec(`INSERT INTO leases (run_id, task_id, attempt_id, owner, nonce, acquired_at, heartbeat_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, a.Run, a.Task, id, a.Owner, a.Nonce, ts, ts); err != nil {
		return core.AttemptRef{}, fmt.Errorf("state: start attempt %s/%s: lease: %w", a.Run, a.Task, err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET attempts_used = ? WHERE run_id = ? AND id = ?`, used+1, a.Run, a.Task); err != nil {
		return core.AttemptRef{}, fmt.Errorf("state: start attempt %s/%s: %w", a.Run, a.Task, err)
	}
	if err := AppendEvent(tx, now, a.Run, a.Task, events.Launch, events.LaunchData{
		Attempt: n, Model: a.Model, SessionID: a.SessionID, BaseSHA: a.BaseSHA, Branch: a.Branch,
	}); err != nil {
		return core.AttemptRef{}, err
	}
	return core.AttemptRef{Run: a.Run, Task: a.Task, Attempt: n, AttemptID: id}, nil
}

// SetLeaseProcess records pid, pgid and the leader's start time once the
// process started.
func (s *Store) SetLeaseProcess(ctx context.Context, ref core.AttemptRef, pid, pgid int, startTime uint64) error {
	return s.Immediate(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE leases SET pid = ?, pgid = ?, start_time = ? WHERE attempt_id = ?`,
			pid, pgid, int64(startTime), ref.AttemptID)
		return err
	})
}

// Heartbeat sets leases.heartbeat_at for ref's attempt (no-op if the lease is gone).
func (s *Store) Heartbeat(ctx context.Context, ref core.AttemptRef, at time.Time) error {
	return s.Immediate(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE leases SET heartbeat_at = ? WHERE attempt_id = ?`, timestamp(at), ref.AttemptID)
		return err
	})
}

// AttemptEnd is the input of FinishAttempt.
type AttemptEnd struct {
	Outcome         string // exited | stalled | killed | start_failed | lost
	ExitCode        *int
	ResultSubtype   string
	IsError         bool
	NumTurns        int
	DurationMS      int64
	CostUSDEstimate float64
	Usage           events.Usage
	BlockedRequest  string
	HeadSHA         string
}

// FinishAttempt sets ended_at and the result fields, deletes the lease and
// writes an `attempt_result` event (CostUSD carries the estimate).
func FinishAttempt(tx *sql.Tx, now time.Time, ref core.AttemptRef, e AttemptEnd) error {
	usage, err := json.Marshal(e.Usage)
	if err != nil {
		return err
	}
	var code any
	exit := 0
	if e.ExitCode != nil {
		code, exit = *e.ExitCode, *e.ExitCode
	}
	isErr := 0
	if e.IsError {
		isErr = 1
	}
	res, err := tx.Exec(`UPDATE attempts SET ended_at = ?, outcome = ?, exit_code = ?, result_subtype = ?, is_error = ?,
		num_turns = ?, duration_ms = ?, cost_usd_estimate = ?, usage_json = ?, blocked_request = ?, head_sha = ?
		WHERE id = ? AND ended_at IS NULL`,
		timestamp(now), e.Outcome, code, e.ResultSubtype, isErr, e.NumTurns, e.DurationMS, e.CostUSDEstimate,
		string(usage), e.BlockedRequest, e.HeadSHA, ref.AttemptID)
	if err != nil {
		return fmt.Errorf("state: finish attempt %s/%s#%d: %w", ref.Run, ref.Task, ref.Attempt, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: finish attempt %s/%s#%d: no open attempt", ref.Run, ref.Task, ref.Attempt)
	}
	if _, err := tx.Exec(`DELETE FROM leases WHERE attempt_id = ?`, ref.AttemptID); err != nil {
		return fmt.Errorf("state: finish attempt %s/%s#%d: %w", ref.Run, ref.Task, ref.Attempt, err)
	}
	return AppendEvent(tx, now, ref.Run, ref.Task, events.AttemptResult, events.AttemptResultData{
		Attempt: ref.Attempt, ExitCode: exit, Subtype: e.ResultSubtype, IsError: e.IsError, NumTurns: e.NumTurns,
		DurationMS: e.DurationMS, CostUSD: e.CostUSDEstimate, Usage: e.Usage,
	})
}

// VerdictRecord is the verifier's verdict for one attempt.
type VerdictRecord struct {
	Result      string // pass | fail | error
	VerdictJSON []byte
	FailedGates []string
	HeadSHA     string
}

// SetVerdict stores the verdict on the attempt row.
func SetVerdict(tx *sql.Tx, ref core.AttemptRef, v VerdictRecord) error {
	gates := v.FailedGates
	if gates == nil {
		gates = []string{}
	}
	gj, err := json.Marshal(gates)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE attempts SET verdict_result = ?, verdict_json = ?, failed_gates_json = ?,
		head_sha = CASE WHEN ? != '' THEN ? ELSE head_sha END WHERE id = ?`,
		v.Result, string(v.VerdictJSON), string(gj), v.HeadSHA, v.HeadSHA, ref.AttemptID)
	if err != nil {
		return fmt.Errorf("state: set verdict %s/%s#%d: %w", ref.Run, ref.Task, ref.Attempt, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: set verdict %s/%s#%d: no such attempt", ref.Run, ref.Task, ref.Attempt)
	}
	return nil
}

// attemptsSetColumn updates one text column of the attempt row.
func (s *Store) attemptsSetColumn(ctx context.Context, ref core.AttemptRef, col, val string) error {
	return s.Immediate(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE attempts SET `+col+` = ? WHERE id = ?`, val, ref.AttemptID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("state: set %s %s/%s#%d: no such attempt", col, ref.Run, ref.Task, ref.Attempt)
		}
		return nil
	})
}

// SetAttestation records the attestation file path.
func (s *Store) SetAttestation(ctx context.Context, ref core.AttemptRef, path string) error {
	return s.attemptsSetColumn(ctx, ref, "attestation_path", path)
}

// SetMutation records the advisory mutation report.
func (s *Store) SetMutation(ctx context.Context, ref core.AttemptRef, reportJSON []byte) error {
	return s.attemptsSetColumn(ctx, ref, "mutation_json", string(reportJSON))
}

// LeaseRow is one row of the leases table.
type LeaseRow struct {
	Run, Task, Owner        string
	AttemptID               int64
	Attempt                 int
	PID, PGID               int
	Nonce                   string
	StartTime               uint64
	AcquiredAt, HeartbeatAt time.Time
}

// Leases returns the run's leases ordered by task id.
func (s *Store) Leases(ctx context.Context, run string) ([]LeaseRow, error) {
	rows, err := s.writer.QueryContext(ctx, `SELECT l.run_id, l.task_id, l.owner, l.attempt_id, a.n,
		COALESCE(l.pid, 0), COALESCE(l.pgid, 0), COALESCE(l.nonce, ''), COALESCE(l.start_time, 0), l.acquired_at, l.heartbeat_at
		FROM leases l JOIN attempts a ON a.id = l.attempt_id WHERE l.run_id = ? ORDER BY l.task_id`, run)
	if err != nil {
		return nil, fmt.Errorf("state: leases: %w", err)
	}
	defer rows.Close()
	var out []LeaseRow
	for rows.Next() {
		var r LeaseRow
		var acq, hb string
		var st int64
		if err := rows.Scan(&r.Run, &r.Task, &r.Owner, &r.AttemptID, &r.Attempt, &r.PID, &r.PGID, &r.Nonce, &st, &acq, &hb); err != nil {
			return nil, fmt.Errorf("state: leases: %w", err)
		}
		r.StartTime = uint64(st)
		if r.AcquiredAt, err = attemptsParseTime(acq); err != nil {
			return nil, err
		}
		if r.HeartbeatAt, err = attemptsParseTime(hb); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func attemptsParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("state: bad timestamp %q: %w", s, err)
	}
	return t, nil
}

// AttemptRow is one attempts row. Nullable columns are pointers or zero
// values (EndedAt is the zero time while the attempt runs).
type AttemptRow struct {
	ID              int64
	Run, Task       string
	N               int
	SessionID       string
	Model           string
	StartedAt       time.Time
	EndedAt         time.Time
	Outcome         string
	ExitCode        *int
	ResultSubtype   string
	IsError         bool
	NumTurns        int
	DurationMS      int64
	CostUSDEstimate float64
	Usage           events.Usage
	StreamPath      string
	BlockedRequest  string
	HeadSHA         string
	VerdictResult   string
	VerdictJSON     []byte
	FailedGates     []string
	AttestationPath string
	MutationJSON    []byte
}

const attemptsSelect = `SELECT id, run_id, task_id, n, session_id, model, started_at, ended_at, outcome, exit_code,
	result_subtype, is_error, num_turns, duration_ms, cost_usd_estimate, usage_json, stream_path, blocked_request,
	head_sha, verdict_result, verdict_json, failed_gates_json, attestation_path, mutation_json FROM attempts`

type attemptsScanner interface{ Scan(dest ...any) error }

func attemptsScan(sc attemptsScanner) (AttemptRow, error) {
	var r AttemptRow
	var started string
	var ended, outcome, subtype, usage, stream, blocked, head, vres, vjson, gates, att, mut sql.NullString
	var code, isErr, turns, dur sql.NullInt64
	var cost sql.NullFloat64
	if err := sc.Scan(&r.ID, &r.Run, &r.Task, &r.N, &r.SessionID, &r.Model, &started, &ended, &outcome, &code,
		&subtype, &isErr, &turns, &dur, &cost, &usage, &stream, &blocked, &head, &vres, &vjson, &gates, &att, &mut); err != nil {
		return AttemptRow{}, err
	}
	var err error
	if r.StartedAt, err = attemptsParseTime(started); err != nil {
		return AttemptRow{}, err
	}
	if ended.Valid {
		if r.EndedAt, err = attemptsParseTime(ended.String); err != nil {
			return AttemptRow{}, err
		}
	}
	if code.Valid {
		c := int(code.Int64)
		r.ExitCode = &c
	}
	r.Outcome, r.ResultSubtype = outcome.String, subtype.String
	r.IsError, r.NumTurns, r.DurationMS = isErr.Int64 != 0, int(turns.Int64), dur.Int64
	r.CostUSDEstimate = cost.Float64
	if usage.Valid && usage.String != "" {
		if err := json.Unmarshal([]byte(usage.String), &r.Usage); err != nil {
			return AttemptRow{}, fmt.Errorf("state: attempt usage_json: %w", err)
		}
	}
	r.StreamPath, r.BlockedRequest, r.HeadSHA = stream.String, blocked.String, head.String
	r.VerdictResult = vres.String
	if vjson.Valid {
		r.VerdictJSON = []byte(vjson.String)
	}
	if gates.Valid && gates.String != "" {
		if err := json.Unmarshal([]byte(gates.String), &r.FailedGates); err != nil {
			return AttemptRow{}, fmt.Errorf("state: attempt failed_gates_json: %w", err)
		}
	}
	r.AttestationPath = att.String
	if mut.Valid {
		r.MutationJSON = []byte(mut.String)
	}
	return r, nil
}

// ListAttempts returns the task's attempts ordered by n.
func (s *Store) ListAttempts(ctx context.Context, run, task string) ([]AttemptRow, error) {
	rows, err := s.writer.QueryContext(ctx, attemptsSelect+` WHERE run_id = ? AND task_id = ? ORDER BY n`, run, task)
	if err != nil {
		return nil, fmt.Errorf("state: list attempts: %w", err)
	}
	defer rows.Close()
	var out []AttemptRow
	for rows.Next() {
		r, err := attemptsScan(rows)
		if err != nil {
			return nil, fmt.Errorf("state: list attempts: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestAttempt returns the task's highest-n attempt; ok is false if none.
func (s *Store) LatestAttempt(ctx context.Context, run, task string) (AttemptRow, bool, error) {
	r, err := attemptsScan(s.writer.QueryRowContext(ctx, attemptsSelect+` WHERE run_id = ? AND task_id = ? ORDER BY n DESC LIMIT 1`, run, task))
	if errors.Is(err, sql.ErrNoRows) {
		return AttemptRow{}, false, nil
	}
	if err != nil {
		return AttemptRow{}, false, fmt.Errorf("state: latest attempt: %w", err)
	}
	return r, true, nil
}
