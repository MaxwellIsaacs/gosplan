package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
)

// tasksTestSpec writes a spec 1.a -> (2.a || 2.b) -> 3.a with 2.b a root
// (deps = []) and 1.a scoped, and loads it.
func tasksTestSpec(t *testing.T) *spec.Spec {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"sequence.txt":  "1.a -> (2.a || 2.b) -> 3.a\n",
		"1.a.md":        "one",
		"2.a.md":        "two a",
		"2.b.md":        "two b",
		"3.a.md":        "three",
		"1.a.task.toml": "scope = [\"x/**\"]\nallow_new_protected = true\n",
		"2.b.task.toml": "deps = []\n",
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(p, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sp, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func tasksSetStatus(t *testing.T, s *Store, run, id, status string) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE tasks SET status = ? WHERE run_id = ? AND id = ?`, status, run, id); err != nil {
		t.Fatal(err)
	}
}

func TestTasksImportRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sp := tasksTestSpec(t)
	if err := s.ImportRun(ctx, "r1", "/proj", "abc123", sp); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListTasks(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
		if r.Status != "pending" {
			t.Errorf("%s status %q", r.ID, r.Status)
		}
	}
	if want := []string{"1.a", "2.a", "2.b", "3.a"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	a1, _ := s.GetTask(ctx, "r1", "1.a")
	if !reflect.DeepEqual(a1.Scope, []string{"x/**"}) || !a1.AllowNewProtected || a1.Tier != 1 || a1.PromptPath != sp.Tasks[0].PromptPath {
		t.Errorf("1.a = %+v", a1)
	}
	b2, _ := s.GetTask(ctx, "r1", "2.b")
	if len(b2.Deps) != 0 || b2.DepsSource != "sidecar" || b2.Scope != nil {
		t.Errorf("2.b = %+v", b2)
	}
	a3, _ := s.GetTask(ctx, "r1", "3.a")
	if !reflect.DeepEqual(a3.Deps, []string{"2.a", "2.b"}) || a3.DepsSource != "tiers" {
		t.Errorf("3.a = %+v", a3)
	}
	run, err := s.GetRun(ctx, "r1")
	if err != nil || run.Project != "/proj" || run.BaseSHA != "abc123" || run.Status != "running" || run.SpecDir != sp.Dir || run.FinishedAt != nil {
		t.Errorf("run = %+v err=%v", run, err)
	}
	if id, _ := s.LatestRun(ctx); id != "r1" {
		t.Errorf("LatestRun = %q", id)
	}

	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	if n, err := s.FlushEvents(ctx, "r1", logPath); err != nil || n != 5 {
		t.Fatalf("flush n=%d err=%v", n, err)
	}
	evs, err := events.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Validate(evs); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if evs[0].Type != events.RunStarted {
		t.Errorf("first event %s, want run_started", evs[0].Type)
	}
	for i, id := range ids {
		ev := evs[i+1]
		if ev.Type != events.TaskState || ev.Task != id {
			t.Errorf("event %d = %s/%s", i+1, ev.Type, ev.Task)
		}
	}
}

func TestTasksImportAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sp := tasksTestSpec(t)
	if err := s.ImportRun(ctx, "r1", "/proj", "abc", sp); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		var n int
		if err := s.DB().QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := [4]int{count(`SELECT count(*) FROM tasks`), count(`SELECT count(*) FROM task_deps`),
		count(`SELECT count(*) FROM events`), count(`SELECT count(*) FROM runs`)}
	if err := s.ImportRun(ctx, "r1", "/proj", "abc", sp); err == nil {
		t.Fatal("duplicate run id imported")
	}
	// A second spec revision under the same run id must not leave a spec row behind.
	sp2 := *sp
	sp2.ContentSHA256 = "different"
	specs := count(`SELECT count(*) FROM specs`)
	if err := s.ImportRun(ctx, "r1", "/proj", "abc", &sp2); err == nil {
		t.Fatal("duplicate run id imported")
	}
	after := [4]int{count(`SELECT count(*) FROM tasks`), count(`SELECT count(*) FROM task_deps`),
		count(`SELECT count(*) FROM events`), count(`SELECT count(*) FROM runs`)}
	if before != after {
		t.Errorf("rows changed after failed import: %v -> %v", before, after)
	}
	if got := count(`SELECT count(*) FROM specs`); got != specs {
		t.Errorf("specs %d -> %d after failed import", specs, got)
	}
	// Same spec content reuses the spec row for a new run.
	if err := s.ImportRun(ctx, "r2", "/proj", "abc", sp); err != nil {
		t.Fatal(err)
	}
	if got := count(`SELECT count(*) FROM specs`); got != specs {
		t.Errorf("specs = %d, want reuse (%d)", got, specs)
	}
}

func TestTasksUpdateTask(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ImportRun(ctx, "r1", "/p", "abc", tasksTestSpec(t)); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	nb := &when
	branch, wt, fail := "task/1.a", "/wt/1.a", "boom"
	attempts := 2
	if err := s.Immediate(ctx, func(tx *sql.Tx) error {
		return UpdateTask(tx, "r1", "1.a", TaskUpdate{NotBefore: &nb, Branch: &branch, Worktree: &wt, LastFailure: &fail, AttemptsUsed: &attempts})
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(ctx, "r1", "1.a")
	if got.NotBefore == nil || !got.NotBefore.Equal(when) || got.Branch != branch || got.Worktree != wt || got.LastFailure != fail || got.AttemptsUsed != 2 {
		t.Fatalf("after set: %+v", got)
	}
	if got.BaseSHA != "" || got.HeadSHA != "" || got.VerifyErrors != 0 || got.BlockedReason != "" || got.Status != "pending" {
		t.Errorf("untouched fields changed: %+v", got)
	}
	var none *time.Time
	reason, head := "why", "deadbeef"
	if err := s.Immediate(ctx, func(tx *sql.Tx) error {
		return UpdateTask(tx, "r1", "1.a", TaskUpdate{NotBefore: &none, BlockedReason: &reason, HeadSHA: &head})
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, "r1", "1.a")
	if got.NotBefore != nil || got.BlockedReason != reason || got.HeadSHA != head {
		t.Errorf("after clear: %+v", got)
	}
	if got.Branch != branch || got.AttemptsUsed != 2 {
		t.Errorf("fields not in the update were reset: %+v", got)
	}
	err := s.Immediate(ctx, func(tx *sql.Tx) error {
		return UpdateTask(tx, "r1", "9.z", TaskUpdate{Branch: &branch})
	})
	if err == nil {
		t.Error("update of unknown task succeeded")
	}
}

func TestTasksFinishRunAndIntegration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ImportRun(ctx, "r1", "/p", "abc", tasksTestSpec(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntegration(ctx, "r1", "gosplan/r1", "feed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Immediate(ctx, func(tx *sql.Tx) error {
		return FinishRun(tx, time.Now(), "r1", "halted", "stop")
	}); err != nil {
		t.Fatal(err)
	}
	run, err := s.GetRun(ctx, "r1")
	if err != nil || run.Status != "halted" || run.FinishedAt == nil || run.IntegrationBranch != "gosplan/r1" || run.IntegrationSHA != "feed" {
		t.Errorf("run = %+v err=%v", run, err)
	}
}
