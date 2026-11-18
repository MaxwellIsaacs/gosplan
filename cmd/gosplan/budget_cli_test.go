package main

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// budgetCLICapture runs f with os.Stdout and os.Stderr redirected and
// returns the exit code and both outputs.
func budgetCLICapture(t *testing.T, f func() int) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wo, we
	outc, errc := make(chan string), make(chan string)
	go func() { var b bytes.Buffer; io.Copy(&b, ro); outc <- b.String() }()
	go func() { var b bytes.Buffer; io.Copy(&b, re); errc <- b.String() }()
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	code = f()
	wo.Close()
	we.Close()
	return code, <-outc, <-errc
}

// budgetV1StateDir makes a state dir whose gosplan.db is a schema v1 file
// (user_version 1, no budget tables) and returns the dir.
func budgetV1StateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "gosplan.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA user_version = 1;
CREATE TABLE specs (id INTEGER PRIMARY KEY, dir TEXT NOT NULL, content_sha256 TEXT NOT NULL, imported_at TEXT NOT NULL);
CREATE TABLE runs (id TEXT PRIMARY KEY, spec_id INTEGER NOT NULL, project TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT, integration_branch TEXT, integration_sha TEXT);`); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestBudgetCLIExplainNoStateDir: `gosplan budget explain` with no state
// database, or with no TASK, exits 2.
func TestBudgetCLIExplainNoStateDir(t *testing.T) {
	empty := t.TempDir()
	code, _, stderr := budgetCLICapture(t, func() int {
		return dispatch([]string{"budget", "explain", "2.a", "--state-dir", empty})
	})
	if code != exitError || !strings.Contains(stderr, "no state database") {
		t.Fatalf("explain without a state dir: code %d stderr %q", code, stderr)
	}
	code, _, stderr = budgetCLICapture(t, func() int {
		return dispatch([]string{"budget", "explain", "--state-dir", empty})
	})
	if code != exitError || !strings.Contains(stderr, "TASK") {
		t.Fatalf("explain without TASK: code %d stderr %q", code, stderr)
	}
	for _, args := range [][]string{{"budget"}, {"budget", "calibrate"}} {
		code, _, _ := budgetCLICapture(t, func() int { return dispatch(append(args, "--state-dir", empty)) })
		if code != exitError {
			t.Errorf("%v without a state dir: code %d, want 2", args, code)
		}
	}
	code, _, _ = budgetCLICapture(t, func() int { return dispatch([]string{"budget", "bogus"}) })
	if code != exitError {
		t.Errorf("gosplan budget bogus: code %d, want 2", code)
	}
}

// TestBudgetCLIV1StateDir: every budget surface on a schema-1 state dir
// prints "no budget tables (schema version 1)" and exits 0, and leaves the
// file at version 1 (read-only, no migration).
func TestBudgetCLIV1StateDir(t *testing.T) {
	dir := budgetV1StateDir(t)
	for _, args := range [][]string{
		{"budget", "--state-dir", dir},
		{"budget", "--state-dir", dir, "--json"},
		{"budget", "explain", "2.a", "--state-dir", dir},
		{"budget", "calibrate", "--state-dir", dir},
	} {
		code, stdout, stderr := budgetCLICapture(t, func() int { return dispatch(args) })
		if code != exitPass || !strings.Contains(stdout, "no budget tables (schema version 1)") {
			t.Errorf("%v: code %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
	s, err := state.OpenReadOnly(filepath.Join(dir, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := s.SchemaVersion(); v != 1 {
		t.Fatalf("gosplan budget migrated the file: schema version %d", v)
	}
}

// TestBudgetCLIReplayHelp: `gosplan replay --help` lists --budget-log, and the
// top-level usage names the gosplan budget commands.
func TestBudgetCLIReplayHelp(t *testing.T) {
	_, _, stderr := budgetCLICapture(t, func() int { return dispatch([]string{"replay", "--help"}) })
	if !strings.Contains(stderr, "-budget-log") {
		t.Fatalf("gosplan replay --help does not list --budget-log:\n%s", stderr)
	}
	_, _, stderr = budgetCLICapture(t, func() int { return dispatch(nil) })
	for _, want := range []string{"gosplan budget [", "gosplan budget explain TASK", "gosplan budget calibrate", "--budget-log FILE"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("usage lacks %q:\n%s", want, stderr)
		}
	}
}
