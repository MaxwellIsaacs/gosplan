package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
)

var budgetT0 = time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)

// budgetEventsFixture seeds run r1 with task 1.a and appends n budget
// events (alternating types) in one tx. Returns the run and the eids.
func budgetEventsFixture(t *testing.T, s *Store, n int) (string, []int64) {
	t.Helper()
	run := "20261105-010203-bdgt"
	storeSeedTask(t, s, run, "1.a", core.StatusPending)
	types := []string{budgetlog.TypeInput, budgetlog.TypeDecision, budgetlog.TypeSettle, budgetlog.TypeBreaker}
	var ids []int64
	if err := s.Immediate(context.Background(), func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			id, err := AppendBudgetEvent(tx, budgetT0, run, "1.a", types[i%len(types)], map[string]any{"i": i})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return run, ids
}

func budgetCount(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBudgetEventsAppendFlush: append + flush writes a budget.jsonl that
// budgetlog.Validate accepts, in eid order with eid in every payload; a
// second flush writes nothing new; events and events.jsonl are untouched.
func TestBudgetEventsAppendFlush(t *testing.T) {
	s := newTestStore(t)
	run, ids := budgetEventsFixture(t, s, 6)
	const eventsBefore = 1 // storeSeedTask's task_state row
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("eids not increasing: %v", ids)
		}
	}
	if got := budgetCount(t, s, `SELECT count(*) FROM events`); got != eventsBefore {
		t.Fatalf("AppendBudgetEvent wrote to events: %d -> %d", eventsBefore, got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.jsonl")
	n, err := s.FlushBudgetEvents(context.Background(), run, path)
	if err != nil || n != 6 {
		t.Fatalf("flush: n=%d err=%v", n, err)
	}
	evs, err := budgetlog.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := budgetlog.Validate(evs); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 6 {
		t.Fatalf("want 6 lines, got %d", len(evs))
	}
	for i, ev := range evs {
		var d struct {
			EID int64 `json:"eid"`
			I   int   `json:"i"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.EID != ids[i] || d.I != i || ev.Run != run || ev.Task != "1.a" {
			t.Fatalf("line %d: %+v data %s", i, ev, ev.Data)
		}
	}
	if left := budgetCount(t, s, `SELECT count(*) FROM budget_events WHERE flushed = 0`); left != 0 {
		t.Fatalf("%d rows left unflushed", left)
	}
	n, err = s.FlushBudgetEvents(context.Background(), run, path)
	if err != nil || n != 0 {
		t.Fatalf("second flush: n=%d err=%v", n, err)
	}
	if evs, _ := budgetlog.ReadFile(path); len(evs) != 6 {
		t.Fatalf("second flush appended: %d lines", len(evs))
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("events.jsonl touched: %v", err)
	}
	if got := budgetCount(t, s, `SELECT count(*) FROM events WHERE flushed = 1`); got != 0 {
		t.Fatalf("budget flush marked %d events rows", got)
	}
}

// TestBudgetEventsNoDuplicateAfterCrash: rows that reached the file but
// were never marked flushed (a crash between append and mark) are not
// appended again.
func TestBudgetEventsNoDuplicateAfterCrash(t *testing.T) {
	s := newTestStore(t)
	run, ids := budgetEventsFixture(t, s, 7)
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	rows, err := s.DB().Query(`SELECT id, task_id, type, data_json FROM budget_events WHERE run_id = ? ORDER BY id`, run)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id        int64
		task, typ string
		data      string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.task, &r.typ, &r.data); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	const k = 3
	l, err := budgetlog.Open(path, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all[:k] {
		var m map[string]any
		if err := json.Unmarshal([]byte(r.data), &m); err != nil {
			t.Fatal(err)
		}
		m["eid"] = r.id
		if _, err := l.Append(r.task, r.typ, m); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	n, err := s.FlushBudgetEvents(context.Background(), run, path)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(all)-k {
		t.Fatalf("flushed %d, want %d (the rows not already in the file)", n, len(all)-k)
	}
	evs, err := budgetlog.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != len(all) {
		t.Fatalf("file has %d lines, want %d", len(evs), len(all))
	}
	seen := map[int64]bool{}
	for _, ev := range evs {
		var d struct {
			EID int64 `json:"eid"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if seen[d.EID] {
			t.Fatalf("eid %d appears twice", d.EID)
		}
		seen[d.EID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("eid %d missing", id)
		}
	}
	if left := budgetCount(t, s, `SELECT count(*) FROM budget_events WHERE flushed = 0`); left != 0 {
		t.Fatalf("%d rows left unflushed", left)
	}
}

// TestBudgetEventsRejects: unknown types (M1 types included) and non-object
// data are refused before any row is written.
func TestBudgetEventsRejects(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "r1", "1.a", core.StatusPending)
	for _, c := range []struct {
		typ  string
		data any
	}{
		{"task_state", map[string]any{}},
		{"budget_snapshot", map[string]any{}},
		{budgetlog.TypeDecision, []int{1}},
		{budgetlog.TypeDecision, nil},
	} {
		err := s.Immediate(context.Background(), func(tx *sql.Tx) error {
			_, err := AppendBudgetEvent(tx, budgetT0, "r1", "1.a", c.typ, c.data)
			return err
		})
		if err == nil {
			t.Errorf("AppendBudgetEvent(%q, %v) accepted", c.typ, c.data)
		}
	}
	if n := budgetCount(t, s, `SELECT count(*) FROM budget_events`); n != 0 {
		t.Fatalf("%d rows written", n)
	}
}

// TestBudgetEventsImportBudgetTasks: one budget_tasks row per task with the
// sidecar's class, prefer list (JSON, or null) and max_turns (or NULL).
func TestBudgetEventsImportBudgetTasks(t *testing.T) {
	s := newTestStore(t)
	run := "20261105-010203-tsks"
	storeSeedTask(t, s, run, "1.a", core.StatusPending)
	storeSeedTask(t, s, run, "2.a", core.StatusPending)
	sp := &spec.Spec{Tasks: []spec.Task{
		{ID: "1.a", Class: "critical", Prefer: []string{"opus", "sonnet"}, MaxTurns: 40},
		{ID: "2.a"},
	}}
	if err := s.ImportBudgetTasks(context.Background(), run, sp); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := s.DB().Query(`SELECT task_id, class || '|' || prefer_json || '|' || ifnull(max_turns, 'NULL') FROM budget_tasks WHERE run_id = ?`, run)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		got[id] = v
	}
	rows.Close()
	want := map[string]string{"1.a": `critical|["opus","sonnet"]|40`, "2.a": "normal|null|NULL"}
	if len(got) != 2 || got["1.a"] != want["1.a"] || got["2.a"] != want["2.a"] {
		t.Fatalf("budget_tasks: got %v, want %v", got, want)
	}
	// A task with no tasks row is a foreign-key error, and nothing is kept.
	bad := &spec.Spec{Tasks: []spec.Task{{ID: "9.z"}}}
	if err := s.ImportBudgetTasks(context.Background(), "20261105-010203-none", bad); err == nil {
		t.Fatal("ImportBudgetTasks accepted a task with no tasks row")
	}
}
