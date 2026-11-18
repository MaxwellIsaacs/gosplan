package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
)

// AppendBudgetEvent inserts one budget_events outbox row and returns its id, the eid FlushBudgetEvents adds to the
// payload. typ must be a budgetlog.Type* value and data must marshal to a
// JSON object. It never writes the events table (events.jsonl).
func AppendBudgetEvent(tx *sql.Tx, now time.Time, run, task, typ string, data any) (int64, error) {
	if !budgetlog.KnownType(typ) {
		return 0, fmt.Errorf("state: unknown budget event type %q", typ)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("state: budget event %s: %w", typ, err)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return 0, fmt.Errorf("state: budget event %s: data must be a JSON object, got %s", typ, raw)
	}
	res, err := tx.Exec(`INSERT INTO budget_events (run_id, task_id, type, data_json, ts) VALUES (?, ?, ?, ?, ?)`,
		run, task, typ, string(raw), timestamp(now))
	if err != nil {
		return 0, fmt.Errorf("state: budget event %s: %w", typ, err)
	}
	return res.LastInsertId()
}

// FlushBudgetEvents appends this run's unflushed budget_events rows, in id
// order, to the budget.jsonl at path (budgetlog.Open/Append) and marks them
// flushed. Each payload gets an extra "eid": <budget_events.id> key. As in
// FlushEvents, it first marks flushed every row whose eid is already in the
// file for this run (a crash between append and mark leaves no duplicate),
// and marks each row right after its append. Returns how many rows it
// appended.
func (s *Store) FlushBudgetEvents(ctx context.Context, run, path string) (int, error) {
	if s.readOnly {
		return 0, errReadOnly
	}
	s.budgetFlushMu.Lock()
	defer s.budgetFlushMu.Unlock()

	// Open first: it repairs a torn tail, which ReadFile would reject.
	log, err := budgetlog.Open(path, run)
	if err != nil {
		return 0, fmt.Errorf("state: budget flush: open log: %w", err)
	}
	defer log.Close()
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, fi.Mode().Perm()&0o700) // hygiene: owner only
	}
	evs, err := budgetlog.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("state: budget flush: read log: %w", err)
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
				if _, err := tx.ExecContext(ctx, `UPDATE budget_events SET flushed = 1 WHERE id = ? AND run_id = ? AND flushed = 0`, id, run); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("state: budget flush: mark present: %w", err)
		}
	}

	type row struct {
		id        int64
		task, typ string
		data      string
	}
	rows, err := s.writer.QueryContext(ctx, `SELECT id, task_id, type, data_json FROM budget_events WHERE run_id = ? AND flushed = 0 ORDER BY id`, run)
	if err != nil {
		return 0, fmt.Errorf("state: budget flush: %w", err)
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.task, &r.typ, &r.data); err != nil {
			rows.Close()
			return 0, fmt.Errorf("state: budget flush: %w", err)
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("state: budget flush: %w", err)
	}

	n := 0
	for _, r := range pending {
		var payload map[string]any
		if err := json.Unmarshal([]byte(r.data), &payload); err != nil {
			return n, fmt.Errorf("state: budget flush: event %d: %w", r.id, err)
		}
		if payload == nil {
			return n, fmt.Errorf("state: budget flush: event %d: data is not a JSON object", r.id)
		}
		payload["eid"] = r.id
		if _, err := log.Append(r.task, r.typ, payload); err != nil {
			return n, fmt.Errorf("state: budget flush: append event %d: %w", r.id, err)
		}
		n++
		if _, err := s.writer.ExecContext(ctx, `UPDATE budget_events SET flushed = 1 WHERE id = ?`, r.id); err != nil {
			return n, fmt.Errorf("state: budget flush: mark event %d: %w", r.id, err)
		}
	}
	return n, nil
}

// ImportBudgetTasks inserts one budget_tasks row per task of sp for run
// (class, prefer_json, max_turns from the sidecar), in one
// Immediate tx. The tasks rows must exist (ImportRun first). Class names are
// not checked here: the runner checks them against the budget config.
func (s *Store) ImportBudgetTasks(ctx context.Context, run string, sp *spec.Spec) error {
	return s.Immediate(ctx, func(tx *sql.Tx) error {
		for _, t := range sp.Tasks {
			class := t.Class
			if class == "" {
				class = spec.DefaultClass
			}
			prefer := "null"
			if len(t.Prefer) > 0 {
				b, err := json.Marshal(t.Prefer)
				if err != nil {
					return err
				}
				prefer = string(b)
			}
			var maxTurns any
			if t.MaxTurns > 0 {
				maxTurns = t.MaxTurns
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO budget_tasks (run_id, task_id, class, prefer_json, max_turns) VALUES (?, ?, ?, ?, ?)`,
				run, t.ID, class, prefer, maxTurns); err != nil {
				return fmt.Errorf("state: budget tasks %s/%s: %w", run, t.ID, err)
			}
		}
		return nil
	})
}
