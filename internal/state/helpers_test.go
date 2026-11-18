// Contract file: changes only by human-reviewed commits.

package state

import (
	"path/filepath"
	"testing"
)

// newTestStore opens a fresh store in t.TempDir() and closes it at cleanup.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// storeSeedTask inserts, with plain SQL, a spec and run row (once per run)
// and a task row with the given status, plus the task's first outbox
// task_state event (from "" to status), as spec import does. The task has
// tier 1, deps_source 'tiers' and no deps.
func storeSeedTask(t *testing.T, s *Store, run, task, status string) {
	t.Helper()
	db := s.DB()
	if _, err := db.Exec(`INSERT OR IGNORE INTO specs (dir, content_sha256, imported_at) VALUES ('/seed/spec', 'seed', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO runs (id, spec_id, project, base_sha, status, started_at)
		VALUES (?, (SELECT id FROM specs WHERE dir = '/seed/spec' AND content_sha256 = 'seed'), '/seed/project',
		        '0000000000000000000000000000000000000000', 'running', '2026-01-01T00:00:00Z')`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source)
		VALUES (?, ?, 1, ?, ?, 'tiers')`, run, task, "/seed/spec/prompts/"+task+".md", status); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events (run_id, task_id, type, data_json, ts)
		VALUES (?, ?, 'task_state', json_object('from', '', 'to', ?), '2026-01-01T00:00:00Z')`, run, task, status); err != nil {
		t.Fatal(err)
	}
}
