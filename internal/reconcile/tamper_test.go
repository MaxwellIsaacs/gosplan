package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/streamjson"
)

// wantHalt checks the run halted with a reason containing every want.
func wantHalt(t *testing.T, o Outcome, want ...string) {
	t.Helper()
	if o.Status != core.RunHalted {
		t.Fatalf("status = %s (%q), want halted", o.Status, o.Reason)
	}
	for _, w := range want {
		if !strings.Contains(o.Reason, w) {
			t.Fatalf("halt reason %q lacks %q", o.Reason, w)
		}
	}
}

func (h *harness) launched(task string) int {
	ls, _ := h.l.snapshot()
	n := 0
	for _, l := range ls {
		if l.task == task {
			n++
		}
	}
	return n
}

// A worker marks another task passed: the next pre-check halts the run and
// the forged task is never launched, verified or integrated.
func TestReconcileTamperStatusHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("2.a", 2, "1.a"))
	h.l.onLaunch = func(req core.LaunchRequest) {
		if req.Ref.Task == "1.a" {
			h.exec(`UPDATE tasks SET status = 'passed', head_sha = 'evil' WHERE id = '2.a'`)
		}
	}
	o := h.runOutcome()
	wantHalt(t, o, ReasonTampered+": ", "tasks row 2.a: status db=\"passed\" mem=\"pending\"", "head_sha")
	if h.launched("2.a") != 0 || len(h.v.callsFor("2.a")) != 0 {
		t.Errorf("2.a launched %d / verified %d times", h.launched("2.a"), len(h.v.callsFor("2.a")))
	}
	if len(h.w.refs) != 0 {
		t.Errorf("integration ran: %v", h.w.refs)
	}
}

// A worker moves its own task's base_sha (to hide a protected edit below
// it): the run halts before any verify sees the forged base.
func TestReconcileTamperBaseSHAHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.script = func(string, int) step {
		s := okStep
		s.lineGap = 100 * time.Millisecond
		return s
	}
	h.l.onLaunch = func(req core.LaunchRequest) {
		h.exec(`UPDATE tasks SET base_sha = 'forged' WHERE id = '1.a'`)
	}
	o := h.runOutcome()
	wantHalt(t, o, "tasks row 1.a: base_sha db=\"forged\" mem=\"runbase\"")
	if n := len(h.v.callsFor("1.a")); n != 0 {
		t.Errorf("verifier called %d times after the base was forged", n)
	}
}

// head_sha forged while the verify runs: the verdict is not recorded as a
// pass; the run halts naming head_sha.
func TestReconcileTamperHeadSHAHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.v.outcomeReq = func(req core.VerifyRequest) core.VerifyOutcome {
		h.exec(`UPDATE tasks SET head_sha = 'evil' WHERE id = '1.a'`)
		return passOutcome
	}
	o := h.runOutcome()
	wantHalt(t, o, "tasks row 1.a: head_sha db=\"evil\" mem=\"head-1.a\"")
	if st := h.task("1.a").Status; st == core.StatusPassed {
		t.Errorf("1.a reached passed")
	}
	if vc := h.v.callsFor("1.a"); len(vc) != 1 || vc[0].Head != "head-1.a" {
		t.Errorf("verify requests %+v, want one for the resolved head-1.a", vc)
	}
}

// The integration tip in the runs row is forged mid-run: halt.
func TestReconcileTamperIntegrationTipHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.onLaunch = func(core.LaunchRequest) {
		h.exec(`UPDATE runs SET integration_sha = 'forged'`)
	}
	wantHalt(t, h.runOutcome(), "integration_sha db=\"forged\" mem=\"\"")
}

// The integration tip forged in an interrupted run is caught by the resume
// cross-check against events.jsonl. (The tip is only in the log once a
// run_finished names it, so the run is aborted after integrating.)
func TestReconcileResumeChecksIntegrationTip(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	if o := h.runOutcome(); o.Status != core.RunComplete || o.IntegrationSHA != "merge(runbase+head-1.a)" {
		t.Fatalf("first run = %+v", o)
	}
	h.exec(`UPDATE runs SET status = 'aborted', integration_sha = 'forged'`)
	h.cfg.Resume = true
	wantHalt(t, h.runOutcome(), ReasonTampered, "integration_sha db=\"forged\", events.jsonl=\"merge(runbase+head-1.a)\"")
}

// Re-review 6: resuming a run that already ended (here complete, with a
// log in the old format: no integration_sha= in run_finished) reports its
// recorded status and writes nothing. Before, the cross-check failed on
// the old log and the run was rewritten halted.
func TestReconcileResumeEndedRunWritesNothing(t *testing.T) {
	for _, status := range []string{core.RunComplete, core.RunHalted} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t, task("1.a", 1))
			if o := h.runOutcome(); o.Status != core.RunComplete {
				t.Fatalf("first run = %+v", o)
			}
			h.exec(`UPDATE runs SET status = ?`, status)
			log, err := os.ReadFile(h.cfg.EventLog)
			if err != nil {
				t.Fatal(err)
			}
			old := []byte(strings.ReplaceAll(string(log), "integration_sha=merge(runbase+head-1.a)", ""))
			if err := os.WriteFile(h.cfg.EventLog, old, 0o600); err != nil {
				t.Fatal(err)
			}
			var events0 int
			_ = h.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events0)
			h.cfg.Resume = true
			o := h.runOutcome()
			if o.Status != status {
				t.Fatalf("resume = %+v, want %s", o, status)
			}
			if got, _ := os.ReadFile(h.cfg.EventLog); string(got) != string(old) {
				t.Error("events.jsonl changed")
			}
			var events1 int
			_ = h.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events1)
			if events1 != events0 {
				t.Errorf("%d events written", events1-events0)
			}
			if rr, _ := h.store.GetRun(context.Background(), testRun); rr.Status != status {
				t.Errorf("runs.status = %s", rr.Status)
			}
		})
	}
}

// A task that is ready but can never be dispatched halts the run
// no_progress, naming it, instead of idling forever.
func TestReconcileLivenessHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("1.b", 1))
	h.cfg.ProgressTimeout = 300 * time.Millisecond
	h.w.computeErr = func(base string, heads []string) error {
		return errors.New("git exploded")
	}
	o := h.runOutcome()
	wantHalt(t, o, ReasonNoProgress+": ", "1.a (ready but not dispatched", "1.b (ready but not dispatched")
}

// Resume does not trust a recorded pass: 1.a's head is forged to an
// unverified commit in tasks and attempts (consistent with the event log,
// which carries no heads). The re-verify of the forged head fails and the
// run halts state_tampered; 2.a is not relaunched.
func TestReconcileResumeReverifiesForgedPass(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("2.a", 2, "1.a"))
	h.l.script = func(id string, n int) step {
		if id == "2.a" {
			return step{hang: true, heartbeat: 50 * time.Millisecond}
		}
		return okStep
	}
	h.v.outcomeReq = func(req core.VerifyRequest) core.VerifyOutcome {
		if req.Head == "bad" {
			return failOutcome("cmd:test")
		}
		return passOutcome
	}
	r, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for h.launched("2.a") == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	if st, err := r.Run(ctx); err != nil || st != core.RunAborted {
		t.Fatalf("first run = %s, %v", st, err)
	}
	h.exec(`UPDATE tasks SET head_sha = 'bad' WHERE id = '1.a'`)
	h.exec(`UPDATE attempts SET head_sha = 'bad' WHERE task_id = '1.a'`)
	launchesBefore := h.launched("2.a")

	h.cfg.Resume = true
	o := h.runOutcome()
	wantHalt(t, o, ReasonTampered, "task 1.a is recorded passed, but re-verifying runbase..bad fails")
	vc := h.v.callsFor("1.a")
	if last := vc[len(vc)-1]; last.Head != "bad" || last.Base != "runbase" {
		t.Errorf("re-verify request %+v", last)
	}
	if h.launched("2.a") != launchesBefore {
		t.Errorf("2.a relaunched after a failed re-verify")
	}
}

// A clean resume of an aborted run (SIGINT) returns it to running,
// re-verifies the passed task (once), retries the killed one and finishes.
func TestReconcileResumeReverifiesPass(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("2.a", 2, "1.a"))
	r, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.l.script = func(id string, n int) step {
		if id == "2.a" && n == 1 {
			return step{hang: true, heartbeat: 50 * time.Millisecond}
		}
		return okStep
	}
	go func() {
		for h.launched("2.a") == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	if st, _ := r.Run(ctx); st != core.RunAborted {
		t.Fatalf("first run = %s", st)
	}
	h.cfg.Resume = true
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("resume = %+v", o)
	}
	if got := outcomes(h.attempts("2.a")); !reflect.DeepEqual(got, []string{"killed", "exited"}) {
		t.Errorf("2.a outcomes = %v, want [killed exited]", got)
	}
	evs, err := events.ReadFile(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	var finished []string
	for _, ev := range evs {
		var d events.RunFinishedData
		if ev.Type == events.RunFinished && json.Unmarshal(ev.Data, &d) == nil {
			finished = append(finished, d.Status)
		}
	}
	if !reflect.DeepEqual(finished, []string{"aborted", "complete"}) {
		t.Errorf("run_finished events = %v, want [aborted complete]", finished)
	}
	if n := len(h.v.callsFor("1.a")); n != 2 {
		t.Errorf("1.a verified %d times, want 2 (run, then resume)", n)
	}
	if h.launched("1.a") != 1 {
		t.Errorf("1.a relaunched on resume")
	}
}

// Another process holding the SQLite write lock past LockTimeout halts the
// run state_locked instead of retrying forever.
func TestReconcileStateLockedHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.LockTimeout = 200 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	locked := make(chan struct{})
	h.l.onLaunch = func(core.LaunchRequest) {
		go func() {
			s, err := openSecond(h)
			if err != nil {
				t.Error(err)
				close(locked)
				return
			}
			defer s.Close()
			tx, err := s.DB().Begin() // BEGIN IMMEDIATE: holds the write lock
			if err != nil {
				t.Error(err)
				close(locked)
				return
			}
			close(locked)
			<-release
			_ = tx.Rollback()
		}()
		<-locked
	}
	start := time.Now()
	o := h.runOutcome()
	wantHalt(t, o, ReasonLocked+": gosplan.db stayed locked")
	if d := time.Since(start); d > 25*time.Second {
		t.Errorf("halt took %v", d)
	}
}

// Two passed tasks that conflict at integration: the later one is blocked
// with integration_conflict and its paths, a blocker event is written, the
// branch keeps the last good merge and the run halts.
func TestReconcileIntegrationConflictBlocks(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("1.b", 1), task("2.a", 2, "1.a"))
	h.w.conflicts = map[string][]string{"head-1.b": {"shared.txt"}}
	o := h.runOutcome()
	wantHalt(t, o, ReasonIntegrationConflict+": task 1.b", "on: shared.txt", "not integrated: 1.b, 2.a")
	tr := h.task("1.b")
	if tr.Status != core.StatusBlocked || !strings.Contains(tr.BlockedReason, "integration_conflict") {
		t.Fatalf("1.b = %s (%q), want blocked integration_conflict", tr.Status, tr.BlockedReason)
	}
	if want := "merge(runbase+head-1.a)"; o.IntegrationSHA != want || h.w.refs[o.IntegrationBranch] != want {
		t.Errorf("integration tip %q (branch %q), want the last good merge %s", o.IntegrationSHA, h.w.refs[o.IntegrationBranch], want)
	}
	evs, err := events.ReadFile(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		found = found || (ev.Type == events.Blocker && ev.Task == "1.b" && strings.Contains(string(ev.Data), "shared.txt"))
	}
	if !found {
		t.Error("no blocker event for 1.b naming shared.txt")
	}
}

// events.jsonl becomes unwritable before the first launch (the reviewer's
// case). Before: every attempt ended lost and the task was failed with no
// worker ever started. Now no attempt is consumed: the task stays pending
// with attempts_used 0, and the run halts `infrastructure`.
func TestReconcileUnwritableLogIsInfrastructure(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MaxInfraFailures = 3
	var once sync.Once
	h.cfg.Now = time.Now
	r, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.afterPlan = func(string) {
		once.Do(func() {
			if err := os.Chmod(h.cfg.EventLog, 0o400); err != nil {
				t.Error(err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := r.Run(ctx)
	if err != nil || ctx.Err() != nil {
		t.Fatalf("Run = %s, %v (ctx %v)", st, err, ctx.Err())
	}
	wantHalt(t, r.Outcome(), ReasonInfrastructure+": 3 infrastructure failures in a row")
	if n := h.launched("1.a"); n != 0 {
		t.Errorf("launched %d workers", n)
	}
	tr := h.task("1.a")
	if tr.Status == core.StatusFailed || tr.AttemptsUsed != 0 {
		t.Errorf("1.a = %s with attempts_used %d; a worker that never started cannot fail", tr.Status, tr.AttemptsUsed)
	}
	for _, a := range h.attempts("1.a") {
		if a.Outcome != "start_failed" {
			t.Errorf("attempt %d outcome %s", a.N, a.Outcome)
		}
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("é", 600) // 1200 bytes, 2 per rune
	for _, got := range []string{headBytes(s, 999), tailBytes(s, 999), firstLine(strings.Repeat("€", 100)), truncate(strings.Repeat("ü", 3000))} {
		if !utf8.ValidString(got) {
			t.Errorf("cut produced invalid UTF-8: %q", got[len(got)-3:])
		}
	}
	if got := describeExit(1, "exited", 3, streamjson.Result{Result: s}, true, nil, 0); !utf8.ValidString(got) {
		t.Error("describeExit cut a rune")
	}
}

// The worker closes its own attempt row (the reviewer's endedat case), so
// FinishAttempt fails on every retry. Before: the run stayed running with
// no halt. Now it halts state_write_failed naming the step and the error.
func TestReconcileWriteFailureHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.WriteFailTimeout = 2 * time.Second
	h.cfg.MaxWriteFailures = 1000 // the timeout decides here
	h.l.onLaunch = func(core.LaunchRequest) {
		h.exec(`UPDATE attempts SET ended_at = '2026-01-01T00:00:00Z' WHERE task_id = '1.a' AND ended_at IS NULL`)
	}
	start := time.Now()
	o := h.runOutcome()
	wantHalt(t, o, ReasonWriteFailed+": task/1.a: ", "no open attempt")
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("halt took %v", d)
	}
	if n := len(h.v.callsFor("1.a")); n != 0 {
		t.Errorf("verified %d times", n)
	}
}

// The consecutive-failure bound halts without waiting for the timeout.
func TestReconcileWriteFailureCountHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.WriteFailTimeout = time.Hour
	h.cfg.MaxWriteFailures = 3
	h.l.onLaunch = func(core.LaunchRequest) {
		h.exec(`UPDATE attempts SET ended_at = '2026-01-01T00:00:00Z' WHERE task_id = '1.a' AND ended_at IS NULL`)
	}
	wantHalt(t, h.runOutcome(), ReasonWriteFailed+": task/1.a: ", "(3 failures in")
}

// The run dir turns read-only mid-run (a disk-level failure): state.json
// can no longer be written, and the run halts state_write_failed on the
// journal instead of carrying on without one.
func TestReconcileReadOnlyStateDirHalts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t, task("1.a", 1), task("2.a", 2, "1.a"))
	h.cfg.WriteFailTimeout = 2 * time.Second
	// Low, so the journal bound trips before 2.a's start_failed launches
	// use up its attempts (that would end the run another fail-closed way).
	h.cfg.MaxWriteFailures = 2
	t.Cleanup(func() { _ = os.Chmod(h.cfg.RunDir, 0o700) })
	h.l.onLaunch = func(req core.LaunchRequest) {
		if req.Ref.Task == "1.a" {
			if err := os.Chmod(h.cfg.RunDir, 0o500); err != nil {
				t.Error(err)
			}
		}
	}
	wantHalt(t, h.runOutcome(), ReasonWriteFailed+": journal: ", "state.json")
	if h.launched("2.a") != 0 {
		t.Errorf("2.a launched with no journal")
	}
}

// The liveness belt: a worker exit or a verdict that could not be recorded
// is named after ProgressTimeout even if noteWrite never fires.
func TestReconcileLivenessNamesUnrecordedExit(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.ProgressTimeout = 300 * time.Millisecond
	h.cfg.WriteFailTimeout = time.Hour
	h.cfg.MaxWriteFailures = 1 << 30
	h.l.onLaunch = func(core.LaunchRequest) {
		h.exec(`UPDATE attempts SET ended_at = '2026-01-01T00:00:00Z' WHERE task_id = '1.a' AND ended_at IS NULL`)
	}
	wantHalt(t, h.runOutcome(), ReasonNoProgress+": ", "1.a (exit could not be recorded")

	h2 := newHarness(t, task("1.a", 1))
	h2.cfg.ProgressTimeout = 300 * time.Millisecond
	h2.cfg.WriteFailTimeout = time.Hour
	h2.cfg.MaxWriteFailures = 1 << 30
	h2.v.outcomeReq = func(req core.VerifyRequest) core.VerifyOutcome {
		h2.exec(`DELETE FROM attempts WHERE task_id = '1.a'`)
		return passOutcome
	}
	wantHalt(t, h2.runOutcome(), ReasonNoProgress+": ", "1.a (verdict could not be recorded")
}

// Refunded attempts are bounded across restarts: a task whose attempts
// already ended lost or start_failed MaxAttempts times (earlier
// orchestrators, say a worker that kept killing gosplan run) is blocked
// `infrastructure` at its next dispatch, not launched at full quota again.
func TestReconcileInfraAttemptsBlockTask(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("2.a", 2, "1.a"), task("1.b", 1))
	h.cfg.MaxAttempts = 2
	for n, outcome := range []string{"lost", "start_failed"} {
		h.exec(`INSERT INTO attempts (run_id, task_id, n, session_id, model, started_at, ended_at, outcome)
			VALUES (?, '1.a', ?, 's', 'sonnet', '2026-11-01T00:00:00Z', '2026-11-01T00:00:01Z', ?)`, testRun, n+1, outcome)
	}
	o := h.runOutcome()
	if o.Status != core.RunHalted || !strings.Contains(o.Reason, "1.a (blocked)") || !strings.Contains(o.Reason, "2.a (abandoned)") {
		t.Fatalf("outcome %+v", o)
	}
	if got := h.task("1.a").BlockedReason; got != ReasonInfrastructure+": 2 lost/start_failed attempts" {
		t.Errorf("1.a blocked_reason = %q", got)
	}
	if h.launched("1.a") != 0 || h.launched("1.b") != 1 {
		t.Errorf("launches: 1.a %d, 1.b %d", h.launched("1.a"), h.launched("1.b"))
	}
}

// Re-review 5c: a handle whose pgid is not its pid is not a group this
// orchestrator made. Its lease records no group (so no restart kills it by
// group) and the worker is stopped; the task retries normally.
func TestReconcileRecordsOnlyLeaderGroup(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	pid := os.Getpid() // a live process; pgid deliberately not its pid
	h.l.ids = func(task string, n int) (int, int) {
		if n == 1 {
			return pid, pid + 1
		}
		return 0, 0
	}
	h.l.script = func(string, int) step { return step{hang: true, heartbeat: 20 * time.Millisecond} }
	// Record what SetLeaseProcess writes (the lease row is gone by the end).
	h.exec(`CREATE TABLE lease_log (pid INTEGER, pgid INTEGER)`)
	h.exec(`CREATE TRIGGER lease_log_t AFTER UPDATE OF pid ON leases BEGIN INSERT INTO lease_log VALUES (NEW.pid, NEW.pgid); END`)
	h.cfg.MaxAttempts = 1
	o := h.runOutcome()
	var l state.LeaseRow
	if err := h.store.DB().QueryRow(`SELECT pid, pgid FROM lease_log`).Scan(&l.PID, &l.PGID); err != nil || l.PID != pid {
		t.Fatalf("lease_log: pid %d, %v", l.PID, err)
	}
	if l.PGID != 0 {
		t.Errorf("lease pgid = %d, want 0 (not a group this process made)", l.PGID)
	}
	if h.l.killCount("1.a") == 0 {
		t.Error("worker with a foreign pgid was not stopped")
	}
	if o.Status != core.RunHalted {
		t.Errorf("outcome %+v", o)
	}
}

// Re-review 7: one busy result (say a read) starts the lock clock; if the
// next poll's probe finds the write lock free, the clock clears and a
// quiet spell longer than LockTimeout does not halt. A lock that is still
// held when the probe runs halts state_locked as before.
func TestReconcileBusyClockProbes(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.LockTimeout = 200 * time.Millisecond
	r, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := r.load(ctx); err != nil {
		t.Fatal(err)
	}
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	r.stateMu.Lock()
	r.noteDB(ctx, time.Now(), busy, false)
	r.stateMu.Unlock()
	time.Sleep(300 * time.Millisecond) // past LockTimeout with no write
	r.busyExpired(ctx)
	if r.isHalted() {
		t.Fatalf("halted on a stale clock: %s", r.Outcome().Reason)
	}
	if !r.busySince.IsZero() {
		t.Error("probe succeeded but the clock still runs")
	}

	// Now really hold the write lock: the probe is busy and the run halts.
	s, err := openSecond(h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	r.stateMu.Lock()
	r.noteDB(ctx, time.Now(), busy, false)
	r.stateMu.Unlock()
	time.Sleep(300 * time.Millisecond)
	r.busyExpired(ctx)
	if o := r.Outcome(); !strings.HasPrefix(o.Reason, ReasonLocked+": ") {
		t.Fatalf("lock held: reason %q, want state_locked", o.Reason)
	}
}
