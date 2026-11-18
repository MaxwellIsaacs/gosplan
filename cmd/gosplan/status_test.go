package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// statusSeed creates a state dir holding two runs (old, then new) and returns it.
func statusSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := state.Open(filepath.Join(dir, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO specs (dir, content_sha256, imported_at) VALUES ('/spec', 'x', '2026-10-27T00:00:00Z')`)
	for _, r := range []struct{ id, at string }{{"20261027-000000-aaaa", "2026-10-27T00:00:00Z"}, {"20261028-031500-bbbb", "2026-10-28T03:15:00Z"}} {
		exec(`INSERT INTO runs (id, spec_id, project, base_sha, status, started_at) VALUES (?, 1, '/p', 'deadbeef', 'running', ?)`, r.id, r.at)
		exec(`INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source) VALUES (?, '1.a', 1, '/p', 'pending', 'tiers')`, r.id)
	}
	return dir
}

// statusRun runs `gosplan status` with args and captures stdout.
func statusRun(t *testing.T, args ...string) (int, string) {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, rd)
		done <- buf.String()
	}()
	code := cmdStatus(args)
	wr.Close()
	os.Stdout = old
	return code, <-done
}

// Without --run the newest run is shown; --run picks another.
func TestStatusJSON(t *testing.T) {
	d := statusSeed(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--state-dir", d, "--json"}, "20261028-031500-bbbb"},
		{[]string{"--state-dir", d, "--json", "--run", "20261027-000000-aaaa"}, "20261027-000000-aaaa"},
	} {
		code, out := statusRun(t, c.args...)
		var r struct {
			Run string `json:"run"`
		}
		if code != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Run != c.want {
			t.Errorf("%v: exit %d, run %q, want %s\n%s", c.args, code, r.Run, c.want, out)
		}
	}
}

// No database: exit 2, say so on stderr, and do not create one.
func TestStatusMissingDB(t *testing.T) {
	d := t.TempDir()
	old := os.Stderr
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = wr
	code, out := statusRun(t, "--state-dir", d)
	wr.Close()
	os.Stderr = old
	msg, _ := io.ReadAll(rd)
	if code != 2 || out != "" || !strings.Contains(string(msg), "no state database") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 2, nothing, 'no state database'", code, out, msg)
	}
	if _, err := os.Stat(filepath.Join(d, "gosplan.db")); !os.IsNotExist(err) {
		t.Errorf("gosplan status created gosplan.db: %v", err)
	}
}

func TestStatusDoesNotWrite(t *testing.T) {
	d := statusSeed(t)
	db := filepath.Join(d, "gosplan.db")
	// Backdate so an mtime update would be visible.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(db, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(db)
	if code, _ := statusRun(t, "--state-dir", d); code != 0 {
		t.Fatalf("exit %d", code)
	}
	after, _ := os.Stat(db)
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("db changed: %v/%d -> %v/%d", before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
	if _, err := os.Stat(filepath.Join(d, "runs")); !os.IsNotExist(err) {
		t.Errorf("runs dir created without --write-report: %v", err)
	}
}

func TestStatusWriteReport(t *testing.T) {
	d := statusSeed(t)
	if code, _ := statusRun(t, "--state-dir", d, "--write-report"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(filepath.Join(d, "runs", "20261028-031500-bbbb", "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct{ Schema string }
	if err := json.Unmarshal(b, &r); err != nil || r.Schema != "gosplan.run-report/v1" {
		t.Errorf("report.json: %v %s", err, b)
	}
}
