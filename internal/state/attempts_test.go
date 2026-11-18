package state

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

func attemptsStart(t *testing.T, s *Store, run, task string, now time.Time) core.AttemptRef {
	t.Helper()
	var ref core.AttemptRef
	err := s.Immediate(context.Background(), func(tx *sql.Tx) error {
		var err error
		ref, err = StartAttempt(tx, now, AttemptStart{Run: run, Task: task, SessionID: "sess", Model: "m", Owner: "h:1:x", StreamPath: "/s"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func attemptsFinish(t *testing.T, s *Store, ref core.AttemptRef, now time.Time) {
	t.Helper()
	code := 0
	err := s.Immediate(context.Background(), func(tx *sql.Tx) error {
		return FinishAttempt(tx, now, ref, AttemptEnd{Outcome: "exited", ExitCode: &code, ResultSubtype: "success",
			NumTurns: 4, DurationMS: 99, CostUSDEstimate: 1.5, Usage: events.Usage{Input: 7}, HeadSHA: "abc"})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func attemptsCountEvents(t *testing.T, s *Store, typ string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events WHERE type = ?`, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAttemptsStartAndFinish(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "r", "t", core.StatusRunning)
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	r1 := attemptsStart(t, s, "r", "t", now)
	if r1.Attempt != 1 || r1.AttemptID == 0 {
		t.Fatalf("ref = %+v", r1)
	}
	ls, err := s.Leases(ctx, "r")
	if err != nil || len(ls) != 1 {
		t.Fatalf("leases = %v, %v", ls, err)
	}
	if ls[0].Attempt != 1 || ls[0].Owner != "h:1:x" || !ls[0].HeartbeatAt.Equal(now) || !ls[0].AcquiredAt.Equal(now) {
		t.Fatalf("lease = %+v", ls[0])
	}
	if err := s.SetLeaseProcess(ctx, r1, 42, 43, 0); err != nil {
		t.Fatal(err)
	}
	if ls, _ = s.Leases(ctx, "r"); ls[0].PID != 42 || ls[0].PGID != 43 {
		t.Fatalf("lease = %+v", ls[0])
	}

	attemptsFinish(t, s, r1, now.Add(time.Minute))
	if ls, _ = s.Leases(ctx, "r"); len(ls) != 0 {
		t.Fatalf("lease not deleted: %v", ls)
	}
	r2 := attemptsStart(t, s, "r", "t", now.Add(2*time.Minute))
	if r2.Attempt != 2 {
		t.Fatalf("second attempt n = %d", r2.Attempt)
	}
	attemptsFinish(t, s, r2, now.Add(3*time.Minute))

	var used int
	if err := s.DB().QueryRow(`SELECT attempts_used FROM tasks WHERE run_id='r' AND id='t'`).Scan(&used); err != nil || used != 2 {
		t.Fatalf("attempts_used = %d, %v", used, err)
	}
	if n := attemptsCountEvents(t, s, "launch"); n != 2 {
		t.Fatalf("launch events = %d", n)
	}
	if n := attemptsCountEvents(t, s, "attempt_result"); n != 2 {
		t.Fatalf("attempt_result events = %d", n)
	}
	var data string
	if err := s.DB().QueryRow(`SELECT data_json FROM events WHERE type='attempt_result' ORDER BY id LIMIT 1`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, `"cost_usd":1.5`) || !strings.Contains(data, `"attempt":1`) {
		t.Fatalf("attempt_result data = %s", data)
	}
	row, ok, err := s.LatestAttempt(ctx, "r", "t")
	if err != nil || !ok || row.N != 2 || row.Outcome != "exited" || row.ExitCode == nil || *row.ExitCode != 0 ||
		row.NumTurns != 4 || row.Usage.Input != 7 || row.HeadSHA != "abc" || row.EndedAt.IsZero() {
		t.Fatalf("latest = %+v ok=%v err=%v", row, ok, err)
	}
}

func TestAttemptsSecondLeaseRejected(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "r", "t", core.StatusRunning)
	now := time.Now()
	attemptsStart(t, s, "r", "t", now)
	err := s.Immediate(context.Background(), func(tx *sql.Tx) error {
		_, err := StartAttempt(tx, now, AttemptStart{Run: "r", Task: "t", SessionID: "s2", Model: "m", Owner: "o"})
		return err
	})
	if err == nil {
		t.Fatal("second StartAttempt without finish succeeded")
	}
	rows, _ := s.ListAttempts(context.Background(), "r", "t")
	if len(rows) != 1 {
		t.Fatalf("attempts = %d, want 1 (rolled back)", len(rows))
	}
}

func TestAttemptsHeartbeat(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "r", "a", core.StatusRunning)
	storeSeedTask(t, s, "r", "b", core.StatusRunning)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ra := attemptsStart(t, s, "r", "a", t0)
	attemptsStart(t, s, "r", "b", t0)

	t1 := t0.Add(10 * time.Second)
	if err := s.Heartbeat(ctx, ra, t1); err != nil {
		t.Fatal(err)
	}
	ls, _ := s.Leases(ctx, "r")
	if len(ls) != 2 || !ls[0].HeartbeatAt.Equal(t1) || !ls[1].HeartbeatAt.Equal(t0) {
		t.Fatalf("leases = %+v", ls)
	}
	attemptsFinish(t, s, ra, t1)
	if err := s.Heartbeat(ctx, ra, t1.Add(time.Second)); err != nil {
		t.Fatalf("heartbeat after lease gone: %v", err)
	}
	if ls, _ = s.Leases(ctx, "r"); len(ls) != 1 || ls[0].Task != "b" {
		t.Fatalf("leases = %+v", ls)
	}
}

func TestAttemptsVerdictAndExtras(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "r", "t", core.StatusRunning)
	ctx := context.Background()
	ref := attemptsStart(t, s, "r", "t", time.Now())
	attemptsFinish(t, s, ref, time.Now())

	err := s.Immediate(ctx, func(tx *sql.Tx) error {
		return SetVerdict(tx, ref, VerdictRecord{Result: "fail", VerdictJSON: []byte(`{"result":"fail"}`), FailedGates: []string{"tests", "lint"}, HeadSHA: "def"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttestation(ctx, ref, "/att/1.json"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMutation(ctx, ref, []byte(`{"score":0.5}`)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAttempts(ctx, "r", "t")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	r := rows[0]
	if r.VerdictResult != "fail" || string(r.VerdictJSON) != `{"result":"fail"}` || len(r.FailedGates) != 2 || r.FailedGates[1] != "lint" ||
		r.HeadSHA != "def" || r.AttestationPath != "/att/1.json" || string(r.MutationJSON) != `{"score":0.5}` {
		t.Fatalf("row = %+v", r)
	}
	if _, ok, _ := s.LatestAttempt(ctx, "r", "nope"); ok {
		t.Fatal("LatestAttempt found a row for an unknown task")
	}
}
