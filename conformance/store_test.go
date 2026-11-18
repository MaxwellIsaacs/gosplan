// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// store_test.go: RED BY DESIGN. The SQLite state, read through the sqlite3 CLI (the DDL is the documented contract).
package conformance

import (
	"bufio"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestStoreWALAndSchema: gosplan.db is WAL, user_version 2, exactly the 8 v1
// tables with their documented columns unchanged plus the 6 budget tables,
// and every outbox row is flushed after the run.
// (Amended in a human-reviewed commit: user_version 1 -> 2
// and the budget tables; the v1 column check is new and stricter.)
// Clause: orchestrator#DY-SQLITE-WAL
// Clause: budget#BG-SCHEMA
func TestStoreWALAndSchema(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a", nil)
	mustExit(t, "gosplan run", f.runNR(), 0)
	db := f.db()
	if m := sql(t, db, "PRAGMA journal_mode;"); m != "wal" {
		t.Fatalf("journal_mode: want wal, got %q", m)
	}
	if v := sql(t, db, "PRAGMA user_version;"); v != "2" {
		t.Fatalf("user_version: want 2, got %q", v)
	}
	tables := sql(t, db, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name;")
	want := "attempts\nbudget_breakers\nbudget_events\nbudget_parks\nbudget_reservations\nbudget_runs\nbudget_tasks\nevents\nleases\nruns\nspecs\ntask_deps\ntasks\nwindow_observations"
	if tables != want {
		t.Fatalf("tables: want\n%s\ngot\n%s", want, tables)
	}
	for table, cols := range v1Columns {
		if got := sql(t, db, "SELECT group_concat(name, ',') FROM pragma_table_info('"+table+"');"); got != cols {
			t.Fatalf("table %s: v1 columns changed: want %s, got %s", table, cols, got)
		}
	}
	for table, cols := range budgetColumns {
		if got := sql(t, db, "SELECT group_concat(name, ',') FROM pragma_table_info('"+table+"');"); got != cols {
			t.Fatalf("table %s: columns are not the documented schema: want %s, got %s", table, cols, got)
		}
	}
	if n := sql(t, db, "SELECT count(*) FROM events WHERE flushed = 0;"); n != "0" {
		t.Fatalf("%s outbox events left unflushed after the run", n)
	}
	if s := sql(t, db, "SELECT group_concat(id||'='||status, ' ') FROM (SELECT id, status FROM tasks ORDER BY id);"); s != "1.a=passed 2.a=passed" {
		t.Fatalf("tasks table: got %q", s)
	}
	if n := sql(t, db, "SELECT count(*) FROM leases;"); n != "0" {
		t.Fatalf("%s leases left after a finished run", n)
	}
}

// TestStoreBeginImmediateBusy: while another process holds the write lock,
// `gosplan run` fails with SQLITE_BUSY and writes nothing; `gosplan status` still reads.
// Clause: orchestrator#DY-SQLITE-IMMEDIATE
func TestStoreBeginImmediateBusy(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	mustExit(t, "gosplan run (first run)", f.runNR(), 0)
	db := f.db()
	before := sql(t, db, "SELECT count(*) FROM runs;")

	lock := exec.Command(sqlite3Bin(t), db)
	stdin, err := lock.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, _ := lock.StdoutPipe()
	if err := lock.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_, _ = io.WriteString(stdin, "ROLLBACK;\n.quit\n")
			stdin.Close()
			_ = lock.Wait()
		}
	}
	t.Cleanup(release)
	_, _ = io.WriteString(stdin, "BEGIN IMMEDIATE;\nSELECT 'locked';\n")
	got := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(stdout).ReadString('\n')
		got <- strings.TrimSpace(l)
	}()
	select {
	case l := <-got:
		if l != "locked" {
			t.Fatalf("sqlite3 holder: want 'locked', got %q", l)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("sqlite3 holder never took the write lock")
	}

	if res := gosplan(t, f.project, f.env(), "status", "--state-dir", f.state, "--json"); res.code != 0 {
		t.Fatalf("gosplan status (read-only) must work while a writer holds the lock: %s", res)
	}
	res := run(t, 90*time.Second, f.project, f.env(), append([]string{nrBin}, f.args()...)...)
	if res.timedOut || res.code == 0 {
		t.Fatalf("second writer: want a non-zero exit within 90s, got %s", res)
	}
	if !regexp.MustCompile(`(?i)busy|locked`).MatchString(res.stderr) {
		t.Fatalf("second writer: stderr must report SQLITE_BUSY / database is locked, got %s", res)
	}
	release()
	if after := sql(t, db, "SELECT count(*) FROM runs;"); after != before {
		t.Fatalf("busy run left a partial write: runs %s -> %s", before, after)
	}
}

// v1Columns are the columns of the 8 v1 tables as of schema v1, in
// order. Schema v2
// adds tables only; these must never change.
var v1Columns = map[string]string{
	"specs":               "id,dir,content_sha256,imported_at",
	"runs":                "id,spec_id,project,base_sha,status,started_at,finished_at,integration_branch,integration_sha",
	"tasks":               "run_id,id,tier,prompt_path,status,deps_source,scope_json,allow_new_protected,not_before,base_sha,branch,worktree,head_sha,attempts_used,verify_errors,last_failure,blocked_reason",
	"task_deps":           "run_id,task_id,dep_id",
	"attempts":            "id,run_id,task_id,n,session_id,model,started_at,ended_at,outcome,exit_code,result_subtype,is_error,num_turns,duration_ms,cost_usd_estimate,usage_json,stream_path,blocked_request,head_sha,verdict_result,verdict_json,failed_gates_json,attestation_path,mutation_json",
	"leases":              "run_id,task_id,attempt_id,owner,pid,pgid,acquired_at,heartbeat_at,nonce,start_time",
	"events":              "id,run_id,task_id,type,data_json,ts,flushed",
	"window_observations": "id,window,utilization,resets_at,status,source,documented,observed_at,attempt_id,raw_json",
}

// budgetColumns are the documented columns of the 6 budget
// tables, in order.
var budgetColumns = map[string]string{
	"budget_tasks":        "run_id,task_id,class,prefer_json,max_turns",
	"budget_runs":         "run_id,owner,mode,max_parallel,deadline,caps_json,config_sha256,config_json,heartbeat_at,halt,halt_reason",
	"budget_reservations": "id,run_id,task_id,attempt_id,model,class,created_at,est_tokens,est_basis,pools_json,belt_usd,enforced,state,settled_at,actual_tokens,actual_json,actual_basis,decision_eid",
	"budget_parks":        "run_id,task_id,rule,reason,parked_at,admit_at,not_before,decision_eid",
	"budget_breakers":     "id,kind,scope,run_id,task_id,state,count,opened_at,closes_at,closed_at,reason,enforced,inputs_json",
	"budget_events":       "id,run_id,task_id,type,data_json,ts,flushed",
}
