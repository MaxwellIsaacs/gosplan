package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// WindowObservation is one observed rate-limit window value, stored with its
// source and age. Rows whose Documented is false come from undocumented
// fields or text layouts (rateLimitType, unifiedWindows, the /usage text).
type WindowObservation struct {
	ID          int64
	Window      string // five_hour | seven_day | session | week_all | week_<model> | unknown | _probe
	Utilization *float64
	ResetsAt    *int64
	Status      string
	Source      string // rate_limit_event | rate_limit_event.unifiedWindows | api_retry | usage_text
	Documented  bool
	ObservedAt  time.Time
	AttemptID   *int64
	Raw         string
}

// RecordWindows inserts obs in one transaction. A zero ObservedAt is stamped
// with s.Now().
func (s *Store) RecordWindows(ctx context.Context, obs []WindowObservation) error {
	if len(obs) == 0 {
		return nil
	}
	now := s.Now()
	return s.Immediate(ctx, func(tx *sql.Tx) error {
		for _, o := range obs {
			at := o.ObservedAt
			if at.IsZero() {
				at = now
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO window_observations
				(window, utilization, resets_at, status, source, documented, observed_at, attempt_id, raw_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				o.Window, o.Utilization, o.ResetsAt, o.Status, o.Source, o.Documented,
				timestamp(at), o.AttemptID, o.Raw); err != nil {
				return fmt.Errorf("state: record window %q: %w", o.Window, err)
			}
		}
		return nil
	})
}

// LatestWindows returns the newest row per window, excluding "_probe", ordered
// by window name.
func (s *Store) LatestWindows(ctx context.Context) ([]WindowObservation, error) {
	rows, err := s.DB().QueryContext(ctx, `SELECT id, window, utilization, resets_at, status, source,
			documented, observed_at, attempt_id, raw_json
		FROM window_observations w
		WHERE window != '_probe' AND id = (
			SELECT id FROM window_observations
			WHERE window = w.window
			ORDER BY julianday(observed_at) DESC, id DESC LIMIT 1)
		ORDER BY window`)
	if err != nil {
		return nil, fmt.Errorf("state: latest windows: %w", err)
	}
	defer rows.Close()
	var out []WindowObservation
	for rows.Next() {
		var (
			o      WindowObservation
			status sql.NullString
			at     string
		)
		if err := rows.Scan(&o.ID, &o.Window, &o.Utilization, &o.ResetsAt, &status, &o.Source,
			&o.Documented, &at, &o.AttemptID, &o.Raw); err != nil {
			return nil, fmt.Errorf("state: scan window: %w", err)
		}
		o.Status = status.String
		if o.ObservedAt, err = tasksParseTime(at); err != nil {
			return nil, fmt.Errorf("state: window observed_at %q: %w", at, err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// LastObservedAt returns the newest observed_at over all non-probe rows, or
// the zero time if there are none.
func (s *Store) LastObservedAt(ctx context.Context) (time.Time, error) {
	return s.windowsNewest(ctx, `window != '_probe'`)
}

// LastProbeAt returns the newest observed_at among usage_text and _probe rows,
// or the zero time if there are none.
func (s *Store) LastProbeAt(ctx context.Context) (time.Time, error) {
	return s.windowsNewest(ctx, `(source = 'usage_text' OR window = '_probe')`)
}

func (s *Store) windowsNewest(ctx context.Context, where string) (time.Time, error) {
	var at sql.NullString
	err := s.DB().QueryRowContext(ctx, `SELECT observed_at FROM window_observations WHERE `+where+
		` ORDER BY julianday(observed_at) DESC, id DESC LIMIT 1`).Scan(&at)
	if err == sql.ErrNoRows || (err == nil && !at.Valid) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("state: newest window observation: %w", err)
	}
	t, err := tasksParseTime(at.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("state: window observed_at %q: %w", at.String, err)
	}
	return t, nil
}
