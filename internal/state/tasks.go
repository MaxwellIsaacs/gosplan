package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
)

// TaskRow is one row of the tasks table, with its deps.
type TaskRow struct {
	Run, ID, Status, PromptPath, DepsSource string
	Tier                                    int
	Deps                                    []string
	Scope                                   []string // nil = unrestricted
	AllowNewProtected                       bool
	NotBefore                               *time.Time
	BaseSHA, Branch, Worktree, HeadSHA      string
	AttemptsUsed, VerifyErrors              int
	LastFailure, BlockedReason              string
}

// RunRow is one row of the runs table, with its spec dir.
type RunRow struct {
	ID, SpecDir, Project, BaseSHA, Status string
	StartedAt                             time.Time
	FinishedAt                            *time.Time
	IntegrationBranch, IntegrationSHA     string
}

// tasksGosplanVersion is the gosplan_version recorded in run_started events.
const tasksGosplanVersion = "dev"

// RunStartedRecord is the run_started payload gosplan run writes: the locked
// events.RunStartedData plus the run's base and the spec's content hash,
// so a resume can cross-check runs.base_sha and specs.content_sha256
// against the event log (two same-uid-writable places to forge
// consistently instead of one).
type RunStartedRecord struct {
	events.RunStartedData
	BaseSHA    string `json:"base_sha"`
	SpecSHA256 string `json:"spec_sha256"`
}

// SpecTasks returns the static part of the task rows ImportRun writes for
// sp (tier, prompt path, deps source, deps, scope, allow_new_protected,
// status pending), normalised the way ListTasks reads them back: deps in
// dep_id order, nil for none; scope nil when empty. A resume compares the
// DB's rows with these.
func SpecTasks(run string, sp *spec.Spec) []TaskRow {
	out := make([]TaskRow, 0, len(sp.Tasks))
	for _, t := range sp.Tasks {
		r := TaskRow{Run: run, ID: t.ID, Tier: t.Tier, PromptPath: t.PromptPath, Status: core.StatusPending,
			DepsSource: t.DepsSource, AllowNewProtected: t.AllowNewProtected}
		if len(t.Deps) > 0 {
			r.Deps = slices.Sorted(slices.Values(t.Deps))
		}
		if len(t.Scope) > 0 {
			r.Scope = slices.Clone(t.Scope)
		}
		out = append(out, r)
	}
	return out
}

// ImportRun writes specs (reusing a row with the same dir+sha), runs,
// tasks (status pending), task_deps, one run_started event and one
// task_state event per task (from "" to "pending"), in one Immediate tx.
// It fails if the run id exists.
func (s *Store) ImportRun(ctx context.Context, runID, project, baseSHA string, sp *spec.Spec) error {
	now := s.Now()
	rows := SpecTasks(runID, sp)
	return s.Immediate(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE id = ?`, runID).Scan(&exists); err != nil {
			return fmt.Errorf("state: import %s: %w", runID, err)
		}
		if exists > 0 {
			return fmt.Errorf("state: import: run %q already exists", runID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO specs (dir, content_sha256, imported_at) VALUES (?, ?, ?)`,
			sp.Dir, sp.ContentSHA256, timestamp(now)); err != nil {
			return fmt.Errorf("state: import %s: spec: %w", runID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs (id, spec_id, project, base_sha, status, started_at)
			VALUES (?, (SELECT id FROM specs WHERE dir = ? AND content_sha256 = ?), ?, ?, ?, ?)`,
			runID, sp.Dir, sp.ContentSHA256, project, baseSHA, core.RunRunning, timestamp(now)); err != nil {
			return fmt.Errorf("state: import %s: run: %w", runID, err)
		}
		if err := AppendEvent(tx, now, runID, "", events.RunStarted, RunStartedRecord{
			RunStartedData: events.RunStartedData{Spec: sp.Dir, Project: project, GosplanVersion: tasksGosplanVersion},
			BaseSHA:        baseSHA, SpecSHA256: sp.ContentSHA256,
		}); err != nil {
			return err
		}
		for _, t := range rows {
			scope, err := json.Marshal(t.Scope) // nil marshals to null
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source, scope_json, allow_new_protected)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				runID, t.ID, t.Tier, t.PromptPath, t.Status, t.DepsSource, string(scope), t.AllowNewProtected); err != nil {
				return fmt.Errorf("state: import %s: task %s: %w", runID, t.ID, err)
			}
		}
		for _, t := range rows {
			for _, d := range t.Deps {
				if _, err := tx.ExecContext(ctx, `INSERT INTO task_deps (run_id, task_id, dep_id) VALUES (?, ?, ?)`, runID, t.ID, d); err != nil {
					return fmt.Errorf("state: import %s: dep %s->%s: %w", runID, t.ID, d, err)
				}
			}
			if err := AppendEvent(tx, now, runID, t.ID, events.TaskState, events.TaskStateData{From: "", To: core.StatusPending}); err != nil {
				return err
			}
		}
		return nil
	})
}

// SpecSHA returns specs.content_sha256 of the spec run was imported from.
func (s *Store) SpecSHA(ctx context.Context, run string) (string, error) {
	var sha string
	err := s.writer.QueryRowContext(ctx,
		`SELECT s.content_sha256 FROM runs r JOIN specs s ON s.id = r.spec_id WHERE r.id = ?`, run).Scan(&sha)
	if err != nil {
		return "", fmt.Errorf("state: spec of run %s: %w", run, err)
	}
	return sha, nil
}

// tasksQuerier is satisfied by *sql.DB and *sql.Tx.
type tasksQuerier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

const tasksSelect = `SELECT run_id, id, tier, prompt_path, status, deps_source, scope_json, allow_new_protected,
	not_before, base_sha, branch, worktree, head_sha, attempts_used, verify_errors, last_failure, blocked_reason FROM tasks`

func tasksParseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

type tasksScanner interface{ Scan(dest ...any) error }

func tasksScan(sc tasksScanner) (TaskRow, error) {
	var r TaskRow
	var scope string
	var notBefore, base, branch, wt, head, lastFail, blocked sql.NullString
	if err := sc.Scan(&r.Run, &r.ID, &r.Tier, &r.PromptPath, &r.Status, &r.DepsSource, &scope, &r.AllowNewProtected,
		&notBefore, &base, &branch, &wt, &head, &r.AttemptsUsed, &r.VerifyErrors, &lastFail, &blocked); err != nil {
		return TaskRow{}, err
	}
	if err := json.Unmarshal([]byte(scope), &r.Scope); err != nil {
		return TaskRow{}, fmt.Errorf("state: task %s/%s: scope_json: %w", r.Run, r.ID, err)
	}
	if len(r.Scope) == 0 {
		r.Scope = nil
	}
	if notBefore.Valid {
		t, err := tasksParseTime(notBefore.String)
		if err != nil {
			return TaskRow{}, fmt.Errorf("state: task %s/%s: not_before: %w", r.Run, r.ID, err)
		}
		r.NotBefore = &t
	}
	r.BaseSHA, r.Branch, r.Worktree, r.HeadSHA = base.String, branch.String, wt.String, head.String
	r.LastFailure, r.BlockedReason = lastFail.String, blocked.String
	return r, nil
}

// tasksLoad reads task rows matching where (may be empty), ordered by (tier, id),
// and fills in Deps.
func (s *Store) tasksLoad(ctx context.Context, where string, args ...any) ([]TaskRow, error) {
	return tasksLoadQ(ctx, s.writer, where, args...)
}

func tasksLoadQ(ctx context.Context, q tasksQuerier, where string, args ...any) ([]TaskRow, error) {
	rows, err := q.QueryContext(ctx, tasksSelect+where+` ORDER BY tier, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: tasks: %w", err)
	}
	var out []TaskRow
	for rows.Next() {
		r, err := tasksScan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: tasks: %w", err)
	}
	for i := range out {
		deps, err := tasksDeps(ctx, q, out[i].Run, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Deps = deps
	}
	return out, nil
}

func tasksDeps(ctx context.Context, q tasksQuerier, run, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT dep_id FROM task_deps WHERE run_id = ? AND task_id = ? ORDER BY dep_id`, run, id)
	if err != nil {
		return nil, fmt.Errorf("state: deps %s/%s: %w", run, id, err)
	}
	defer rows.Close()
	var deps []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// GetTask returns one task, or an error wrapping sql.ErrNoRows.
func (s *Store) GetTask(ctx context.Context, run, id string) (TaskRow, error) {
	rows, err := s.tasksLoad(ctx, ` WHERE run_id = ? AND id = ?`, run, id)
	if err != nil {
		return TaskRow{}, err
	}
	if len(rows) == 0 {
		return TaskRow{}, fmt.Errorf("state: task %s/%s: %w", run, id, sql.ErrNoRows)
	}
	return rows[0], nil
}

// ListTasks returns every task of the run in (tier, id) order.
func (s *Store) ListTasks(ctx context.Context, run string) ([]TaskRow, error) {
	return s.tasksLoad(ctx, ` WHERE run_id = ?`, run)
}

// ListTasksTx is ListTasks inside a transaction.
func ListTasksTx(tx *sql.Tx, run string) ([]TaskRow, error) {
	return tasksLoadQ(context.Background(), tx, ` WHERE run_id = ?`, run)
}

// TaskUpdate sets only the non-nil fields.
type TaskUpdate struct {
	NotBefore                          **time.Time // pointer to nil pointer clears it
	BaseSHA, Branch, Worktree, HeadSHA *string
	AttemptsUsed, VerifyErrors         *int
	LastFailure, BlockedReason         *string
}

// UpdateTask applies u to run/id. It returns sql.ErrNoRows if there is no such task.
func UpdateTask(tx *sql.Tx, run, id string, u TaskUpdate) error {
	var sets []string
	var args []any
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if u.NotBefore != nil {
		if *u.NotBefore == nil {
			add("not_before", nil)
		} else {
			add("not_before", timestamp(**u.NotBefore))
		}
	}
	for _, f := range []struct {
		col string
		v   *string
	}{{"base_sha", u.BaseSHA}, {"branch", u.Branch}, {"worktree", u.Worktree}, {"head_sha", u.HeadSHA},
		{"last_failure", u.LastFailure}, {"blocked_reason", u.BlockedReason}} {
		if f.v != nil {
			add(f.col, *f.v)
		}
	}
	if u.AttemptsUsed != nil {
		add("attempts_used", *u.AttemptsUsed)
	}
	if u.VerifyErrors != nil {
		add("verify_errors", *u.VerifyErrors)
	}
	if len(sets) == 0 {
		return nil
	}
	q := "UPDATE tasks SET "
	for i, s := range sets {
		if i > 0 {
			q += ", "
		}
		q += s
	}
	res, err := tx.Exec(q+" WHERE run_id = ? AND id = ?", append(args, run, id)...)
	if err != nil {
		return fmt.Errorf("state: update task %s/%s: %w", run, id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update task %s/%s: %w", run, id, sql.ErrNoRows)
	}
	return nil
}

// LatestRun returns the newest run id (by started_at), or "" if none.
func (s *Store) LatestRun(ctx context.Context) (string, error) {
	var id string
	err := s.writer.QueryRowContext(ctx, `SELECT id FROM runs ORDER BY julianday(started_at) DESC, id DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("state: latest run: %w", err)
	}
	return id, nil
}

// GetRun returns one run, or an error wrapping sql.ErrNoRows.
func (s *Store) GetRun(ctx context.Context, id string) (RunRow, error) {
	return getRunQ(ctx, s.writer, id)
}

// GetRunTx is GetRun inside a transaction.
func GetRunTx(tx *sql.Tx, id string) (RunRow, error) {
	return getRunQ(context.Background(), tx, id)
}

func getRunQ(ctx context.Context, q tasksQuerier, id string) (RunRow, error) {
	var r RunRow
	var started string
	var finished, ibranch, isha sql.NullString
	err := q.QueryRowContext(ctx, `SELECT r.id, sp.dir, r.project, r.base_sha, r.status, r.started_at, r.finished_at,
		r.integration_branch, r.integration_sha FROM runs r JOIN specs sp ON sp.id = r.spec_id WHERE r.id = ?`, id).
		Scan(&r.ID, &r.SpecDir, &r.Project, &r.BaseSHA, &r.Status, &started, &finished, &ibranch, &isha)
	if err != nil {
		return RunRow{}, fmt.Errorf("state: run %s: %w", id, err)
	}
	if r.StartedAt, err = tasksParseTime(started); err != nil {
		return RunRow{}, fmt.Errorf("state: run %s: started_at: %w", id, err)
	}
	if finished.Valid {
		t, err := tasksParseTime(finished.String)
		if err != nil {
			return RunRow{}, fmt.Errorf("state: run %s: finished_at: %w", id, err)
		}
		r.FinishedAt = &t
	}
	r.IntegrationBranch, r.IntegrationSHA = ibranch.String, isha.String
	return r, nil
}

// FinishRun sets status, finished_at and writes run_finished, in tx.
func FinishRun(tx *sql.Tx, now time.Time, run, status, reason string) error {
	res, err := tx.Exec(`UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`, status, timestamp(now), run)
	if err != nil {
		return fmt.Errorf("state: finish run %s: %w", run, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: finish run %s: %w", run, sql.ErrNoRows)
	}
	return AppendEvent(tx, now, run, "", events.RunFinished, events.RunFinishedData{Status: status, Reason: reason})
}

// ReopenRun puts an aborted run back to running (resume after SIGINT).
func ReopenRun(tx *sql.Tx, run string) error {
	res, err := tx.Exec(`UPDATE runs SET status = 'running', finished_at = NULL WHERE id = ? AND status = 'aborted'`, run)
	if err != nil {
		return fmt.Errorf("state: reopen run %s: %w", run, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: reopen run %s: not aborted", run)
	}
	return nil
}

// SetIntegration records the integration branch and tip.
func (s *Store) SetIntegration(ctx context.Context, run, branch, sha string) error {
	return s.Immediate(ctx, func(tx *sql.Tx) error { return SetIntegrationTx(tx, run, branch, sha) })
}

// SetIntegrationTx is SetIntegration inside a transaction.
func SetIntegrationTx(tx *sql.Tx, run, branch, sha string) error {
	res, err := tx.Exec(`UPDATE runs SET integration_branch = ?, integration_sha = ? WHERE id = ?`, branch, sha, run)
	if err != nil {
		return fmt.Errorf("state: set integration %s: %w", run, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: set integration %s: %w", run, sql.ErrNoRows)
	}
	return nil
}
