package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
)

// budgetV1Columns are the schema v1 columns of the 8 v1 tables,
// in order. Schema v2 adds tables only, so these never change.
var budgetV1Columns = map[string]string{
	"specs":               "id,dir,content_sha256,imported_at",
	"runs":                "id,spec_id,project,base_sha,status,started_at,finished_at,integration_branch,integration_sha",
	"tasks":               "run_id,id,tier,prompt_path,status,deps_source,scope_json,allow_new_protected,not_before,base_sha,branch,worktree,head_sha,attempts_used,verify_errors,last_failure,blocked_reason",
	"task_deps":           "run_id,task_id,dep_id",
	"attempts":            "id,run_id,task_id,n,session_id,model,started_at,ended_at,outcome,exit_code,result_subtype,is_error,num_turns,duration_ms,cost_usd_estimate,usage_json,stream_path,blocked_request,head_sha,verdict_result,verdict_json,failed_gates_json,attestation_path,mutation_json",
	"leases":              "run_id,task_id,attempt_id,owner,pid,pgid,acquired_at,heartbeat_at,nonce,start_time",
	"events":              "id,run_id,task_id,type,data_json,ts,flushed",
	"window_observations": "id,window,utilization,resets_at,status,source,documented,observed_at,attempt_id,raw_json",
}

// budgetV2Columns are the 6 budget tables of schema v2.
var budgetV2Columns = map[string]string{
	"budget_tasks":        "run_id,task_id,class,prefer_json,max_turns",
	"budget_runs":         "run_id,owner,mode,max_parallel,deadline,caps_json,config_sha256,config_json,heartbeat_at,halt,halt_reason",
	"budget_reservations": "id,run_id,task_id,attempt_id,model,class,created_at,est_tokens,est_basis,pools_json,belt_usd,enforced,state,settled_at,actual_tokens,actual_json,actual_basis,decision_eid",
	"budget_parks":        "run_id,task_id,rule,reason,parked_at,admit_at,not_before,decision_eid",
	"budget_breakers":     "id,kind,scope,run_id,task_id,state,count,opened_at,closes_at,closed_at,reason,enforced,inputs_json",
	"budget_events":       "id,run_id,task_id,type,data_json,ts,flushed",
}

// budgetV1DDL is a v1 file from before the lease identity columns, as
// conformance/budget_surface_test.go's v1DDL builds it, with rows.
const budgetV1DDL = `PRAGMA user_version = 1;
CREATE TABLE specs (id INTEGER PRIMARY KEY, dir TEXT NOT NULL, content_sha256 TEXT NOT NULL, imported_at TEXT NOT NULL, UNIQUE (dir, content_sha256));
CREATE TABLE runs (id TEXT PRIMARY KEY, spec_id INTEGER NOT NULL REFERENCES specs(id), project TEXT NOT NULL, base_sha TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('running','complete','halted','aborted')), started_at TEXT NOT NULL, finished_at TEXT,
  integration_branch TEXT, integration_sha TEXT);
CREATE TABLE tasks (run_id TEXT NOT NULL REFERENCES runs(id), id TEXT NOT NULL, tier INTEGER NOT NULL, prompt_path TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','running','verifying','passed','failed','blocked','abandoned')),
  deps_source TEXT NOT NULL CHECK (deps_source IN ('tiers','sidecar')), scope_json TEXT NOT NULL DEFAULT 'null',
  allow_new_protected INTEGER NOT NULL DEFAULT 0, not_before TEXT, base_sha TEXT, branch TEXT, worktree TEXT, head_sha TEXT,
  attempts_used INTEGER NOT NULL DEFAULT 0, verify_errors INTEGER NOT NULL DEFAULT 0, last_failure TEXT, blocked_reason TEXT,
  PRIMARY KEY (run_id, id));
CREATE TABLE task_deps (run_id TEXT NOT NULL, task_id TEXT NOT NULL, dep_id TEXT NOT NULL, PRIMARY KEY (run_id, task_id, dep_id),
  FOREIGN KEY (run_id, task_id) REFERENCES tasks(run_id, id), FOREIGN KEY (run_id, dep_id) REFERENCES tasks(run_id, id));
CREATE TABLE attempts (id INTEGER PRIMARY KEY, run_id TEXT NOT NULL, task_id TEXT NOT NULL, n INTEGER NOT NULL, session_id TEXT NOT NULL,
  model TEXT NOT NULL, started_at TEXT NOT NULL, ended_at TEXT,
  outcome TEXT CHECK (outcome IN ('exited','stalled','killed','start_failed','lost')), exit_code INTEGER, result_subtype TEXT,
  is_error INTEGER, num_turns INTEGER, duration_ms INTEGER, cost_usd_estimate REAL, usage_json TEXT, stream_path TEXT,
  blocked_request TEXT, head_sha TEXT, verdict_result TEXT CHECK (verdict_result IN ('pass','fail','error')), verdict_json TEXT,
  failed_gates_json TEXT, attestation_path TEXT, mutation_json TEXT, UNIQUE (run_id, task_id, n),
  FOREIGN KEY (run_id, task_id) REFERENCES tasks(run_id, id));
CREATE TABLE leases (run_id TEXT NOT NULL, task_id TEXT NOT NULL, attempt_id INTEGER NOT NULL UNIQUE REFERENCES attempts(id),
  owner TEXT NOT NULL, pid INTEGER, pgid INTEGER, acquired_at TEXT NOT NULL, heartbeat_at TEXT NOT NULL, PRIMARY KEY (run_id, task_id));
CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, task_id TEXT NOT NULL DEFAULT '', type TEXT NOT NULL,
  data_json TEXT NOT NULL, ts TEXT NOT NULL, flushed INTEGER NOT NULL DEFAULT 0);
CREATE INDEX events_unflushed ON events(run_id, flushed, id);
CREATE TABLE window_observations (id INTEGER PRIMARY KEY, window TEXT NOT NULL, utilization REAL, resets_at INTEGER, status TEXT,
  source TEXT NOT NULL, documented INTEGER NOT NULL, observed_at TEXT NOT NULL, attempt_id INTEGER, raw_json TEXT NOT NULL);
CREATE INDEX window_latest ON window_observations(window, observed_at);
INSERT INTO specs VALUES (1, '/old/spec', 'aaaa', '2026-10-30T00:00:00Z');
INSERT INTO runs (id, spec_id, project, base_sha, status, started_at) VALUES ('20261030-000000-old1', 1, '/old/project', '0000000000000000000000000000000000000000', 'complete', '2026-10-30T00:00:00Z');
INSERT INTO window_observations (window, utilization, status, source, documented, observed_at, raw_json) VALUES ('seven_day', 0.2, 'allowed', 'rate_limit_event.unifiedWindows', 0, '2026-10-30T00:00:00Z', '{}');
`

// budgetRawDB builds a file at a new path with ddl through a plain
// connection (no Open), and returns the path.
func budgetRawDB(t *testing.T, ddl string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gosplan.db")
	raw, err := sql.Open("sqlite", writerDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(ddl); err != nil {
		t.Fatalf("build raw db: %v", err)
	}
	return path
}

func budgetQ(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

func budgetCheckColumns(t *testing.T, db *sql.DB, want map[string]string) {
	t.Helper()
	for table, cols := range want {
		if got := budgetQ(t, db, `SELECT group_concat(name, ',') FROM pragma_table_info(?)`, table); got != cols {
			t.Errorf("table %s columns:\n got %s\nwant %s", table, got, cols)
		}
	}
}

// TestBudgetMigrateFreshDB: a new file is user_version 2 with the 8 v1
// tables (schema v1 columns) plus the 6 budget tables, 14 in all.
func TestBudgetMigrateFreshDB(t *testing.T) {
	s := newTestStore(t)
	db := s.DB()
	if v := budgetQ(t, db, "PRAGMA user_version"); v != "2" {
		t.Fatalf("user_version: got %s, want 2", v)
	}
	if s.SchemaVersion() != 2 {
		t.Fatalf("SchemaVersion: got %d", s.SchemaVersion())
	}
	tables := budgetQ(t, db, `SELECT group_concat(name, ',') FROM (SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name)`)
	want := "attempts,budget_breakers,budget_events,budget_parks,budget_reservations,budget_runs,budget_tasks,events,leases,runs,specs,task_deps,tasks,window_observations"
	if tables != want {
		t.Fatalf("tables:\n got %s\nwant %s", tables, want)
	}
	budgetCheckColumns(t, db, budgetV1Columns)
	budgetCheckColumns(t, db, budgetV2Columns)
	for _, idx := range []string{"budget_reservations_open", "budget_breakers_open", "budget_events_unflushed"} {
		if n := budgetQ(t, db, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx); n != "1" {
			t.Errorf("index %s missing", idx)
		}
	}
}

// TestBudgetMigrateV1WithRows: a v1 file built from master's schema.sql
// with rows is migrated to 2 on Open; the rows survive and the v1
// columns are exactly schema v1's.
func TestBudgetMigrateV1WithRows(t *testing.T) {
	path := budgetRawDB(t, schemaSQL)
	raw, err := sql.Open("sqlite", writerDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO specs (id, dir, content_sha256, imported_at) VALUES (1, '/s', 'x', '2026-10-30T00:00:00Z')`,
		`INSERT INTO runs (id, spec_id, project, base_sha, status, started_at) VALUES ('r1', 1, '/p', '0000000000000000000000000000000000000000', 'running', '2026-10-30T00:00:00Z')`,
		`INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source) VALUES ('r1', '1.a', 1, '/s/prompts/1.a.md', 'running', 'tiers')`,
		`INSERT INTO attempts (id, run_id, task_id, n, session_id, model, started_at) VALUES (7, 'r1', '1.a', 1, 'sess', 'sonnet', '2026-10-30T00:00:00Z')`,
		`INSERT INTO leases (run_id, task_id, attempt_id, owner, acquired_at, heartbeat_at, nonce, start_time) VALUES ('r1', '1.a', 7, 'h:1:n', '2026-10-30T00:00:00Z', '2026-10-30T00:00:00Z', 'nonce1', 42)`,
		`INSERT INTO events (run_id, task_id, type, data_json, ts) VALUES ('r1', '1.a', 'task_state', '{"from":"","to":"pending"}', '2026-10-30T00:00:00Z')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	defer s.Close()
	db := s.DB()
	if v := budgetQ(t, db, "PRAGMA user_version"); v != "2" {
		t.Fatalf("user_version after migration: %s", v)
	}
	budgetCheckColumns(t, db, budgetV1Columns)
	budgetCheckColumns(t, db, budgetV2Columns)
	if got := budgetQ(t, db, `SELECT nonce || ':' || start_time FROM leases WHERE attempt_id = 7`); got != "nonce1:42" {
		t.Fatalf("lease row after migration: %q", got)
	}
	if got := budgetQ(t, db, `SELECT status FROM tasks WHERE run_id = 'r1' AND id = '1.a'`); got != core.StatusRunning {
		t.Fatalf("task row after migration: %q", got)
	}
	if got := budgetQ(t, db, `SELECT count(*) FROM events`); got != "1" {
		t.Fatalf("events after migration: %s", got)
	}
	// The new tables work against the old rows (foreign keys on).
	if _, err := db.Exec(`INSERT INTO budget_tasks (run_id, task_id) VALUES ('r1', '1.a')`); err != nil {
		t.Fatalf("budget_tasks insert after migration: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO budget_tasks (run_id, task_id) VALUES ('r1', 'nope')`); err == nil {
		t.Fatal("budget_tasks accepted a task that does not exist")
	}
}

// TestBudgetMigrateV1BeforeLeaseColumns: a v1 file from before the lease
// identity columns (conformance's v1DDL) gets migrateLeases then v2 in
// one Open: same columns as a fresh file, rows intact.
func TestBudgetMigrateV1BeforeLeaseColumns(t *testing.T) {
	path := budgetRawDB(t, budgetV1DDL)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open pre-lease v1: %v", err)
	}
	defer s.Close()
	db := s.DB()
	if v := budgetQ(t, db, "PRAGMA user_version"); v != "2" {
		t.Fatalf("user_version: %s", v)
	}
	budgetCheckColumns(t, db, budgetV1Columns)
	budgetCheckColumns(t, db, budgetV2Columns)
	if got := budgetQ(t, db, `SELECT count(*) FROM runs WHERE id = '20261030-000000-old1'`); got != "1" {
		t.Fatal("migration lost the old run")
	}
	if got := budgetQ(t, db, `SELECT count(*) FROM window_observations WHERE observed_at = '2026-10-30T00:00:00Z'`); got != "1" {
		t.Fatal("migration lost the old observation")
	}
	if n := budgetQ(t, db, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`); n != "14" {
		t.Fatalf("tables after migration: %s, want 14", n)
	}
}

// TestBudgetMigrateRefusesNewer: user_version 3 is refused by Open and
// OpenReadOnly, and Open leaves the file as it was.
func TestBudgetMigrateRefusesNewer(t *testing.T) {
	path := budgetRawDB(t, "PRAGMA user_version = 3; CREATE TABLE future (x INTEGER);")
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open accepted user_version 3")
	} else if !strings.Contains(err.Error(), "schema version 3, this gosplan supports up to 2") {
		t.Fatalf("Open error: %v", err)
	}
	if s, err := OpenReadOnly(path); err == nil {
		s.Close()
		t.Fatal("OpenReadOnly accepted user_version 3")
	}
	raw, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if n := budgetQ(t, raw, `SELECT count(*) FROM sqlite_master WHERE name LIKE 'budget_%'`); n != "0" {
		t.Fatalf("refused Open still created %s budget objects", n)
	}
}

// TestBudgetMigrateOpenReadOnly: OpenReadOnly reads a v1 file without
// migrating it (SchemaVersion 1) and a v2 file (SchemaVersion 2).
func TestBudgetMigrateOpenReadOnly(t *testing.T) {
	v1 := budgetRawDB(t, budgetV1DDL)
	ro, err := OpenReadOnly(v1)
	if err != nil {
		t.Fatalf("OpenReadOnly v1: %v", err)
	}
	if ro.SchemaVersion() != 1 {
		t.Fatalf("v1 SchemaVersion: %d", ro.SchemaVersion())
	}
	if run, err := ro.LatestRun(context.Background()); err != nil || run != "20261030-000000-old1" {
		t.Fatalf("read v1 through OpenReadOnly: %q %v", run, err)
	}
	if v := budgetQ(t, ro.DB(), "PRAGMA user_version"); v != "1" {
		t.Fatalf("OpenReadOnly migrated the file: user_version %s", v)
	}
	ro.Close()

	s := newTestStore(t)
	ro2, err := OpenReadOnly(s.path)
	if err != nil {
		t.Fatalf("OpenReadOnly v2: %v", err)
	}
	defer ro2.Close()
	if ro2.SchemaVersion() != 2 {
		t.Fatalf("v2 SchemaVersion: %d", ro2.SchemaVersion())
	}
}
