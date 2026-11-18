package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

var storeT0 = time.Date(2026, 10, 28, 1, 2, 3, 456789, time.UTC)

// storeTransition runs one Transition in its own Immediate tx.
func storeTransition(t *testing.T, s *Store, run, task, from, to string) error {
	t.Helper()
	return s.Immediate(context.Background(), func(tx *sql.Tx) error {
		return Transition(tx, storeT0, run, task, from, to, "test")
	})
}

func storeStatus(t *testing.T, s *Store, run, task string) string {
	t.Helper()
	var st string
	if err := s.DB().QueryRow(`SELECT status FROM tasks WHERE run_id = ? AND id = ?`, run, task).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// storeStateEvents returns the data of every task_state outbox row of task.
func storeStateEvents(t *testing.T, s *Store, run, task string) []events.TaskStateData {
	t.Helper()
	rows, err := s.DB().Query(`SELECT data_json FROM events WHERE run_id = ? AND task_id = ? AND type = 'task_state' ORDER BY id`, run, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []events.TaskStateData
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var d events.TaskStateData
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func storeTaskTable(t *testing.T, s *Store, run string) map[string]string {
	t.Helper()
	rows, err := s.DB().Query(`SELECT id, status FROM tasks WHERE run_id = ?`, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			t.Fatal(err)
		}
		out[id] = st
	}
	return out
}

// storeEIDs returns the eid of every event in the file, in file order.
func storeEIDs(t *testing.T, evs []events.Event) []int64 {
	t.Helper()
	var out []int64
	for _, ev := range evs {
		var d struct {
			EID *int64 `json:"eid"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || d.EID == nil {
			t.Fatalf("event seq %d has no eid: %s", ev.Seq, ev.Data)
		}
		out = append(out, *d.EID)
	}
	return out
}

// foreign_keys is enforced, not just reported.
func TestStoreForeignKeys(t *testing.T) {
	s := newTestStore(t)
	_, err := s.DB().Exec(`INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source) VALUES ('no-such-run', '1.a', 1, '/p', 'pending', 'tiers')`)
	if err == nil {
		t.Fatal("insert of a task with no run row succeeded; foreign keys not enforced")
	}
}

// Reopening does not re-apply the schema: data survives. A database from
// before the lease identity columns gets them on open.
func TestStoreReopenAndMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gosplan.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	storeSeedTask(t, s, "r1", "1.a", core.StatusPending)
	for _, c := range []string{"nonce", "start_time"} {
		if _, err := s.DB().Exec(`ALTER TABLE leases DROP COLUMN ` + c); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if got := storeStatus(t, s, "r1", "1.a"); got != core.StatusPending {
		t.Fatalf("after reopen status = %q", got)
	}
	if _, err := s.Leases(context.Background(), "r1"); err != nil {
		t.Fatalf("leases after migration: %v", err)
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gosplan.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open accepted user_version 99")
	}
	if s, err := OpenReadOnly(path); err == nil {
		s.Close()
		t.Fatal("OpenReadOnly accepted user_version 99")
	}
}

func TestStoreImmediateIsExclusive(t *testing.T) {
	a := newTestStore(t)
	b, err := Open(a.path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	storeSeedTask(t, a, "r1", "1.a", core.StatusPending)

	aIn := make(chan struct{})
	aRelease := make(chan struct{})
	var released atomic.Bool
	aDone := make(chan error, 1)
	go func() {
		aDone <- a.Immediate(context.Background(), func(tx *sql.Tx) error {
			// A has read (and so would "plan" from) the status before B runs.
			var st string
			if err := tx.QueryRow(`SELECT status FROM tasks WHERE run_id = 'r1' AND id = '1.a'`).Scan(&st); err != nil {
				return err
			}
			close(aIn)
			<-aRelease
			return Transition(tx, storeT0, "r1", "1.a", st, core.StatusRunning, "a")
		})
	}()
	<-aIn

	var bEnteredEarly atomic.Bool
	bDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		bDone <- b.Immediate(ctx, func(tx *sql.Tx) error {
			if !released.Load() {
				bEnteredEarly.Store(true)
			}
			var st string
			if err := tx.QueryRow(`SELECT status FROM tasks WHERE run_id = 'r1' AND id = '1.a'`).Scan(&st); err != nil {
				return err
			}
			if st != core.StatusRunning {
				return Transition(tx, storeT0, "r1", "1.a", st, core.StatusBlocked, "b")
			}
			return nil
		})
	}()

	// Give B well past its 200ms deadline to (wrongly) get in while A holds the lock.
	time.Sleep(400 * time.Millisecond)
	released.Store(true)
	close(aRelease)
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
	bErr := <-bDone
	if bEnteredEarly.Load() {
		t.Fatalf("B's transaction began while A's IMMEDIATE transaction was open (B err: %v)", bErr)
	}
	// B either failed (busy / deadline) or ran after A committed and saw
	// running; either way the status is A's, and the event chain has one entry.
	if got := storeStatus(t, a, "r1", "1.a"); got != core.StatusRunning {
		t.Fatalf("status = %q, want running (B err: %v)", got, bErr)
	}
	if evs := storeStateEvents(t, a, "r1", "1.a"); len(evs) != 2 {
		t.Fatalf("task_state rows = %+v, want seed + A's", evs)
	}
}

func TestStoreTransitionCAS(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "r1", "1.a", core.StatusPending)
	seed := len(storeStateEvents(t, s, "r1", "1.a"))

	if err := storeTransition(t, s, "r1", "1.a", core.StatusPending, core.StatusRunning); err != nil {
		t.Fatalf("pending->running: %v", err)
	}
	evs := storeStateEvents(t, s, "r1", "1.a")
	if len(evs) != seed+1 {
		t.Fatalf("task_state rows after one transition: %d, want %d", len(evs), seed+1)
	}
	if last := evs[len(evs)-1]; last.From != core.StatusPending || last.To != core.StatusRunning || last.Reason != "test" {
		t.Fatalf("event = %+v", last)
	}
	if got := storeStatus(t, s, "r1", "1.a"); got != core.StatusRunning {
		t.Fatalf("status = %q", got)
	}

	// Stale plan: the task is no longer pending.
	err := storeTransition(t, s, "r1", "1.a", core.StatusPending, core.StatusRunning)
	if !errors.Is(err, core.ErrConflict) {
		t.Fatalf("second pending->running: err = %v, want core.ErrConflict", err)
	}
	if n := len(storeStateEvents(t, s, "r1", "1.a")); n != seed+1 {
		t.Fatalf("conflicting transition wrote an event: %d rows", n)
	}
	// Missing task is a conflict too.
	if err := storeTransition(t, s, "r1", "9.z", core.StatusPending, core.StatusRunning); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("missing task: err = %v, want core.ErrConflict", err)
	}

	// A failing Immediate rolls back both the status and the event.
	boom := errors.New("boom")
	err = s.Immediate(context.Background(), func(tx *sql.Tx) error {
		if err := Transition(tx, storeT0, "r1", "1.a", core.StatusRunning, core.StatusVerifying, "x"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Immediate err = %v, want fn's error", err)
	}
	if got := storeStatus(t, s, "r1", "1.a"); got != core.StatusRunning {
		t.Fatalf("rolled-back status = %q, want running", got)
	}
	if n := len(storeStateEvents(t, s, "r1", "1.a")); n != seed+1 {
		t.Fatalf("rolled-back transition left an event: %d rows", n)
	}
}

func TestStoreTransitionRejectsUnknownStatus(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "r1", "1.a", core.StatusPending)
	cases := []struct{ from, to string }{
		{core.StatusPending, "done"},
		{core.StatusPending, ""},
		{"", core.StatusPending},
		{"Pending", core.StatusRunning},
	}
	for _, c := range cases {
		err := storeTransition(t, s, "r1", "1.a", c.from, c.to)
		if err == nil || errors.Is(err, core.ErrConflict) {
			t.Errorf("Transition(%q -> %q): err = %v, want a non-conflict error", c.from, c.to, err)
		}
	}
	if got := storeStatus(t, s, "r1", "1.a"); got != core.StatusPending {
		t.Fatalf("status = %q, want pending", got)
	}
	if n := len(storeStateEvents(t, s, "r1", "1.a")); n != 1 {
		t.Fatalf("rejected transitions wrote events: %d rows", n)
	}
	// AppendEvent refuses unknown types and non-object payloads.
	err := s.Immediate(context.Background(), func(tx *sql.Tx) error {
		return AppendEvent(tx, storeT0, "r1", "1.a", events.Type("nope"), map[string]any{})
	})
	if err == nil {
		t.Fatal("AppendEvent accepted an unknown type")
	}
	err = s.Immediate(context.Background(), func(tx *sql.Tx) error {
		return AppendEvent(tx, storeT0, "r1", "1.a", events.Blocker, []string{"x"})
	})
	if err == nil {
		t.Fatal("AppendEvent accepted a non-object payload")
	}
}

// storeFlushFixture seeds three tasks and moves them along; returns the run.
func storeFlushFixture(t *testing.T, s *Store) string {
	t.Helper()
	run := "20261028-010203-abcd"
	for _, id := range []string{"1.a", "2.a", "2.b"} {
		storeSeedTask(t, s, run, id, core.StatusPending)
	}
	if err := s.Immediate(context.Background(), func(tx *sql.Tx) error {
		return AppendEvent(tx, storeT0, run, "", events.RunStarted, events.RunStartedData{Spec: "/s", Project: "/p"})
	}); err != nil {
		t.Fatal(err)
	}
	steps := []struct{ task, from, to string }{
		{"1.a", core.StatusPending, core.StatusRunning},
		{"2.a", core.StatusPending, core.StatusAbandoned},
		{"1.a", core.StatusRunning, core.StatusVerifying},
		{"1.a", core.StatusVerifying, core.StatusPassed},
		{"2.b", core.StatusPending, core.StatusRunning},
	}
	for _, st := range steps {
		if err := storeTransition(t, s, run, st.task, st.from, st.to); err != nil {
			t.Fatalf("%s %s->%s: %v", st.task, st.from, st.to, err)
		}
	}
	return run
}

func TestStoreFlushEventsValidates(t *testing.T) {
	s := newTestStore(t)
	run := storeFlushFixture(t, s)
	var total int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events WHERE run_id = ?`, run).Scan(&total); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	n, err := s.FlushEvents(context.Background(), run, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Fatalf("flushed %d, want %d", n, total)
	}
	evs, err := events.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := events.Validate(evs); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	replayed, err := events.TaskStatuses(evs)
	if err != nil {
		t.Fatal(err)
	}
	if want := storeTaskTable(t, s, run); !reflect.DeepEqual(replayed, want) {
		t.Fatalf("replayed %v, tasks table %v", replayed, want)
	}
	// eids follow outbox id order, and the envelope carries run and task.
	eids := storeEIDs(t, evs)
	for i := 1; i < len(eids); i++ {
		if eids[i] <= eids[i-1] {
			t.Fatalf("eids not in id order: %v", eids)
		}
	}
	for _, ev := range evs {
		if ev.Run != run {
			t.Fatalf("event run = %q", ev.Run)
		}
	}
	var unflushed int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events WHERE flushed = 0`).Scan(&unflushed); err != nil {
		t.Fatal(err)
	}
	if unflushed != 0 {
		t.Fatalf("%d rows left unflushed", unflushed)
	}

	// A second flush appends nothing; a later transition appends just itself.
	if n, err := s.FlushEvents(context.Background(), run, logPath); err != nil || n != 0 {
		t.Fatalf("second flush: n=%d err=%v", n, err)
	}
	if err := storeTransition(t, s, run, "2.b", core.StatusRunning, core.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if n, err := s.FlushEvents(context.Background(), run, logPath); err != nil || n != 1 {
		t.Fatalf("third flush: n=%d err=%v", n, err)
	}
	evs, err = events.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	replayed, _ = events.TaskStatuses(evs)
	if want := storeTaskTable(t, s, run); !reflect.DeepEqual(replayed, want) {
		t.Fatalf("after third flush: replayed %v, tasks table %v", replayed, want)
	}
}

func TestStoreFlushEventsNoDuplicateAfterCrash(t *testing.T) {
	s := newTestStore(t)
	run := storeFlushFixture(t, s)
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	// Simulate a crash: the first k rows reach the file exactly as
	// FlushEvents writes them, but flushed stays 0.
	rows, err := s.DB().Query(`SELECT id, task_id, type, data_json FROM events WHERE run_id = ? ORDER BY id`, run)
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
	const k = 4
	l, err := events.Open(logPath, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all[:k] {
		var m map[string]any
		if err := json.Unmarshal([]byte(r.data), &m); err != nil {
			t.Fatal(err)
		}
		m["eid"] = r.id
		if _, err := l.Append(r.task, events.Type(r.typ), m); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	n, err := s.FlushEvents(context.Background(), run, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(all)-k {
		t.Fatalf("flushed %d, want %d (the rows not already in the file)", n, len(all)-k)
	}
	evs, err := events.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := events.Validate(evs); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	eids := storeEIDs(t, evs)
	if len(eids) != len(all) {
		t.Fatalf("file has %d events, want %d", len(eids), len(all))
	}
	for i, r := range all {
		if eids[i] != r.id {
			t.Fatalf("file eids %v, want outbox ids in order", eids)
		}
	}
	replayed, _ := events.TaskStatuses(evs)
	if want := storeTaskTable(t, s, run); !reflect.DeepEqual(replayed, want) {
		t.Fatalf("replayed %v, tasks table %v", replayed, want)
	}
}

func TestStoreWriteStateJSON(t *testing.T) {
	s := newTestStore(t)
	run := storeFlushFixture(t, s)
	storeSeedTask(t, s, "other-run", "1.a", core.StatusFailed)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteStateJSON(context.Background(), run, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{} // the exact type cmdReplay decodes into
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("state.json is not a flat task->status object: %v\n%s", err, b)
	}
	want := map[string]string{"1.a": "passed", "2.a": "abandoned", "2.b": "running"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state.json = %v, want %v", got, want)
	}
	// The flushed log replays to the same map (the `gosplan replay --check` contract).
	logPath := filepath.Join(dir, "events.jsonl")
	if _, err := s.FlushEvents(context.Background(), run, logPath); err != nil {
		t.Fatal(err)
	}
	evs, err := events.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	replayed, _ := events.TaskStatuses(evs)
	if d := events.Diff(replayed, got); d != "" {
		t.Fatalf("replay mismatch: %s", d)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if err := s.WriteStateJSON(context.Background(), "no-such-run", filepath.Join(dir, "x.json")); err == nil {
		t.Fatal("WriteStateJSON accepted an unknown run")
	}
}

func TestStoreDefaultDir(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	project := filepath.Join(t.TempDir(), "myproj")

	got, err := DefaultDir(project)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(project))
	want := filepath.Join(xdg, "gosplan", "myproj-"+hex.EncodeToString(sum[:])[:12])
	if got != want {
		t.Fatalf("DefaultDir = %s, want %s", got, want)
	}
	if again, _ := DefaultDir(project); again != got {
		t.Fatalf("not deterministic: %s vs %s", again, got)
	}
	// A relative path names the same project as its absolute form.
	t.Chdir(filepath.Dir(project))
	if rel, _ := DefaultDir("myproj"); rel != got {
		t.Fatalf("relative path: %s, want %s", rel, got)
	}
	// Same basename, different path: different dir.
	other, _ := DefaultDir(filepath.Join(t.TempDir(), "myproj"))
	if other == got {
		t.Fatal("two projects with the same basename share a state dir")
	}
	if rel, err := filepath.Rel(project, got); err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("state dir %s is inside project %s", got, project)
	}

	// Unset XDG_STATE_HOME: $HOME/.local/state.
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	got, err = DefaultDir(project)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "gosplan", filepath.Base(want)); got != want {
		t.Fatalf("fallback DefaultDir = %s, want %s", got, want)
	}

	// A state home inside the project is refused, never used.
	t.Setenv("XDG_STATE_HOME", filepath.Join(project, ".state"))
	if dir, err := DefaultDir(project); err == nil {
		t.Fatalf("DefaultDir returned %s inside the project", dir)
	}
}

func TestStoreReadOnlyRejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gosplan.db")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	storeSeedTask(t, w, "r1", "1.a", core.StatusPending)
	defer w.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got := storeStatus(t, ro, "r1", "1.a"); got != core.StatusPending {
		t.Fatalf("read-only read: %q", got)
	}
	if err := ro.Immediate(context.Background(), func(tx *sql.Tx) error {
		return Transition(tx, storeT0, "r1", "1.a", core.StatusPending, core.StatusRunning, "")
	}); err == nil {
		t.Fatal("Immediate on a read-only store succeeded")
	}
	if _, err := ro.DB().Exec(`UPDATE tasks SET status = 'running'`); err == nil {
		t.Fatal("raw UPDATE through the read-only pool succeeded")
	}
	if _, err := ro.FlushEvents(context.Background(), "r1", filepath.Join(t.TempDir(), "e.jsonl")); err == nil {
		t.Fatal("FlushEvents on a read-only store succeeded")
	}
	if got := storeStatus(t, w, "r1", "1.a"); got != core.StatusPending {
		t.Fatalf("status changed through the read-only store: %q", got)
	}
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("OpenReadOnly created a missing database")
	}
}

// tableInfo is every table's PRAGMA table_info rows (cid, name, type,
// notnull, default, pk), in column order.
func tableInfo(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	names, err := s.DB().Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var n string
		if err := names.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	names.Close()
	out := map[string][]string{}
	for _, tb := range tables {
		rows, err := s.DB().Query(`SELECT cid, name, type, "notnull", coalesce(dflt_value, 'NULL'), pk FROM pragma_table_info(?)`, tb)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ, dflt string
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			out[tb] = append(out[tb], fmt.Sprintf("%d %s %s notnull=%d default=%s pk=%d", cid, name, typ, notnull, dflt, pk))
		}
		rows.Close()
	}
	return out
}

// A fresh DB and a v1 DB from before the lease identity columns, once
// migrated, have the same tables with the same columns in the same order
// (code and later schema work that reads columns by position, or compares
// schemas, must not see two layouts).
func TestStoreFreshAndMigratedSchemasMatch(t *testing.T) {
	fresh, err := Open(filepath.Join(t.TempDir(), "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	// The v1 schema as shipped: schema.sql without the two lease columns.
	var v1 []string
	for _, l := range strings.Split(schemaSQL, "\n") {
		f := strings.Fields(l)
		if len(f) > 0 && (f[0] == "nonce" || f[0] == "start_time") {
			continue
		}
		v1 = append(v1, l)
	}
	if len(v1) != len(strings.Split(schemaSQL, "\n"))-2 {
		t.Fatalf("schema.sql no longer has exactly the two lease identity columns to strip")
	}
	oldPath := filepath.Join(t.TempDir(), "gosplan.db")
	raw, err := sql.Open("sqlite", writerDSN(oldPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(strings.Join(v1, "\n")); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	raw.Close()
	migrated, err := Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()

	a, b := tableInfo(t, fresh), tableInfo(t, migrated)
	if len(a) < 8 { // v1 has 8; later schema versions add tables
		t.Fatalf("fresh DB has only %d tables", len(a))
	}
	for tb, cols := range a {
		if !reflect.DeepEqual(cols, b[tb]) {
			t.Errorf("table %s differs:\nfresh:    %s\nmigrated: %s", tb, strings.Join(cols, " | "), strings.Join(b[tb], " | "))
		}
	}
	if len(b) != len(a) {
		t.Errorf("migrated DB has %d tables, fresh %d", len(b), len(a))
	}
}
