package state

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

// knownStatuses are the task statuses Transition accepts.
var knownStatuses = map[string]bool{
	core.StatusPending: true, core.StatusRunning: true, core.StatusVerifying: true,
	core.StatusPassed: true, core.StatusFailed: true, core.StatusBlocked: true,
	core.StatusAbandoned: true,
}

// knownTypes mirrors the events package's (unexported) set, so a bad type
// fails at AppendEvent rather than at flush time.
var knownTypes = map[events.Type]bool{
	events.RunStarted: true, events.TaskState: true, events.Launch: true,
	events.AttemptResult: true, events.Gate: true, events.Verdict: true,
	events.Blocker: true, events.RunFinished: true,
}

// timestamp formats t as the store's RFC 3339 UTC text.
func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Transition moves run/task from `from` to `to` with a compare-and-set
// (UPDATE tasks SET status=? WHERE run_id=? AND id=? AND status=?). If no row
// matched it returns core.ErrConflict. In the same tx it inserts a task_state
// event row with data events.TaskStateData{From: from, To: to, Reason: reason}.
// Both statuses must be core.Status* values, else an error.
func Transition(tx *sql.Tx, now time.Time, run, task, from, to, reason string) error {
	if !knownStatuses[from] {
		return fmt.Errorf("state: transition %s/%s: unknown from status %q", run, task, from)
	}
	if !knownStatuses[to] {
		return fmt.Errorf("state: transition %s/%s: unknown to status %q", run, task, to)
	}
	res, err := tx.Exec(`UPDATE tasks SET status = ? WHERE run_id = ? AND id = ? AND status = ?`, to, run, task, from)
	if err != nil {
		return fmt.Errorf("state: transition %s/%s: %w", run, task, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: transition %s/%s: %w", run, task, err)
	}
	if n == 0 {
		return core.ErrConflict
	}
	return AppendEvent(tx, now, run, task, events.TaskState, events.TaskStateData{From: from, To: to, Reason: reason})
}

// AppendEvent inserts one outbox row. t must be a known events.Type.
// data must marshal to a JSON object (FlushEvents adds an "eid" key to it).
func AppendEvent(tx *sql.Tx, now time.Time, run, task string, t events.Type, data any) error {
	if !knownTypes[t] {
		return fmt.Errorf("state: unknown event type %q", t)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("state: event %s: %w", t, err)
	}
	if !bytes.HasPrefix(raw, []byte("{")) {
		return fmt.Errorf("state: event %s: data must be a JSON object, got %s", t, raw)
	}
	if _, err := tx.Exec(`INSERT INTO events (run_id, task_id, type, data_json, ts) VALUES (?, ?, ?, ?, ?)`,
		run, task, string(t), string(raw), timestamp(now)); err != nil {
		return fmt.Errorf("state: event %s: %w", t, err)
	}
	return nil
}

// FlushEvents appends this run's unflushed outbox rows, in id order, to the
// events.jsonl at logPath using events.Open/Append, then marks them flushed.
// Each appended payload carries an extra "eid": <events.id> key. Before
// appending it reads the file and marks as flushed every row whose eid is
// already there for this run (crash between append and mark: no duplicate).
// Returns how many rows it appended.
//
// Rows are marked one at a time right after their append, so a crash leaves
// at most one appended-but-unmarked row, which the next call skips.
func (s *Store) FlushEvents(ctx context.Context, run, logPath string) (int, error) {
	if s.readOnly {
		return 0, errReadOnly
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	// Open first: it repairs a torn tail, which ReadFile would reject.
	log, err := events.Open(logPath, run)
	if err != nil {
		return 0, fmt.Errorf("state: flush: open log: %w", err)
	}
	defer log.Close()
	if fi, err := os.Stat(logPath); err == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(logPath, fi.Mode().Perm()&0o700) // hygiene: owner only
	}
	evs, err := events.ReadFile(logPath)
	if err != nil {
		return 0, fmt.Errorf("state: flush: read log: %w", err)
	}
	var present []int64
	for _, ev := range evs {
		if ev.Run != run {
			continue
		}
		var d struct {
			EID *int64 `json:"eid"`
		}
		if err := json.Unmarshal(ev.Data, &d); err == nil && d.EID != nil {
			present = append(present, *d.EID)
		}
	}
	if len(present) > 0 {
		err := s.Immediate(ctx, func(tx *sql.Tx) error {
			for _, id := range present {
				if _, err := tx.ExecContext(ctx, `UPDATE events SET flushed = 1 WHERE id = ? AND run_id = ? AND flushed = 0`, id, run); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("state: flush: mark present: %w", err)
		}
	}

	type row struct {
		id        int64
		task, typ string
		data      string
	}
	rows, err := s.writer.QueryContext(ctx, `SELECT id, task_id, type, data_json FROM events WHERE run_id = ? AND flushed = 0 ORDER BY id`, run)
	if err != nil {
		return 0, fmt.Errorf("state: flush: %w", err)
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.task, &r.typ, &r.data); err != nil {
			rows.Close()
			return 0, fmt.Errorf("state: flush: %w", err)
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("state: flush: %w", err)
	}

	n := 0
	for _, r := range pending {
		var payload map[string]any
		if err := json.Unmarshal([]byte(r.data), &payload); err != nil {
			return n, fmt.Errorf("state: flush: event %d: %w", r.id, err)
		}
		if payload == nil {
			return n, fmt.Errorf("state: flush: event %d: data is not a JSON object", r.id)
		}
		payload["eid"] = r.id
		if _, err := log.Append(r.task, events.Type(r.typ), payload); err != nil {
			return n, fmt.Errorf("state: flush: append event %d: %w", r.id, err)
		}
		n++
		if _, err := s.writer.ExecContext(ctx, `UPDATE events SET flushed = 1 WHERE id = ?`, r.id); err != nil {
			return n, fmt.Errorf("state: flush: mark event %d: %w", r.id, err)
		}
	}
	return n, nil
}

// WriteStateJSON writes {task: status} for the run to path (via temp file +
// rename), in the flat format `gosplan replay --check` reads.
func (s *Store) WriteStateJSON(ctx context.Context, run, path string) error {
	var exists int
	if err := s.writer.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE id = ?`, run).Scan(&exists); err != nil {
		return fmt.Errorf("state: state.json: %w", err)
	}
	if exists == 0 {
		return fmt.Errorf("state: state.json: unknown run %q", run)
	}
	rows, err := s.writer.QueryContext(ctx, `SELECT id, status FROM tasks WHERE run_id = ?`, run)
	if err != nil {
		return fmt.Errorf("state: state.json: %w", err)
	}
	statuses := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return fmt.Errorf("state: state.json: %w", err)
		}
		statuses[id] = status
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("state: state.json: %w", err)
	}
	b, err := json.MarshalIndent(statuses, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

// writeFileAtomic writes b to a temp file next to path, fsyncs it and
// renames it over path, so readers see the old or the new file, never part.
func writeFileAtomic(path string, b []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return fmt.Errorf("state: write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	return nil
}
