package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/driver"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

func diamond() []spec.Task {
	return []spec.Task{task("1.a", 1), task("2.a", 2, "1.a"), task("2.b", 2, "1.a"), task("3.a", 3, "2.a", "2.b")}
}

func failOutcome(gate string) core.VerifyOutcome {
	return core.VerifyOutcome{Exit: 1, Result: "fail", FailedGates: []string{gate}, Detail: gate + ": 3 tests failed",
		VerdictJSON: []byte(`{"result":"fail"}`)}
}

func TestReconcileDiamondCompletes(t *testing.T) {
	h := newHarness(t, diamond()...)
	if got := h.run(); got != core.RunComplete {
		t.Fatalf("run status = %s, want complete", got)
	}
	ctx := context.Background()
	rows, err := h.store.ListTasks(ctx, testRun)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, r := range rows {
		if r.Status != core.StatusPassed {
			t.Errorf("task %s = %s, want passed", r.ID, r.Status)
		}
		want[r.ID] = r.Status
	}
	p, ok := h.w.prepareFor("3.a")
	if !ok {
		t.Fatal("3.a was never prepared")
	}
	if !reflect.DeepEqual(p.DepHeads, []string{"head-2.a", "head-2.b"}) {
		t.Errorf("3.a DepHeads = %v, want [head-2.a head-2.b]", p.DepHeads)
	}
	if p.BaseSHA != "runbase" {
		t.Errorf("3.a Prepare BaseSHA = %q, want the run base", p.BaseSHA)
	}
	vc := h.v.callsFor("3.a")
	if want := "merge(runbase+head-2.a+head-2.b)"; len(vc) != 1 || vc[0].Base != want || vc[0].Head != "head-3.a" || vc[0].EventLog != h.cfg.EventLog {
		t.Errorf("3.a verify requests = %+v, want one with %s..head-3.a", vc, want)
	}
	evs, err := events.ReadFile(h.cfg.EventLog) // validates
	if err != nil {
		t.Fatalf("events.ReadFile: %v", err)
	}
	if err := events.Validate(evs); err != nil {
		t.Fatalf("events.Validate: %v", err)
	}
	got, err := events.TaskStatuses(evs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replayed statuses = %v, rows = %v", got, want)
	}
	b, err := os.ReadFile(filepath.Join(h.cfg.RunDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sj map[string]string
	if err := json.Unmarshal(b, &sj); err != nil {
		t.Fatal(err)
	}
	if d := events.Diff(got, sj); d != "" {
		t.Errorf("state.json disagrees with the log: %s", d)
	}
}

func TestReconcileNoTierBarrier(t *testing.T) {
	c := task("2.c", 2)
	c.DepsSource = "sidecar"
	h := newHarness(t, append(diamond(), c)...)
	h.cfg.MaxAttempts = 2
	slow := func(n int, gap time.Duration) step {
		s := step{lineGap: gap}
		for i := 0; i < n; i++ {
			s.lines = append(s.lines, `{"type":"assistant"}`)
		}
		s.lines = append(s.lines, resultLine("done"))
		return s
	}
	h.l.script = func(id string, n int) step {
		switch id {
		case "1.a":
			return slow(3, 100*time.Millisecond)
		case "2.a":
			return slow(6, 100*time.Millisecond)
		}
		return okStep
	}
	h.v.outcome = func(id string, n int) core.VerifyOutcome {
		if id == "2.b" {
			return failOutcome("gate_b")
		}
		return passOutcome
	}
	if got := h.run(); got != core.RunHalted {
		t.Fatalf("run status = %s, want halted", got)
	}
	launches, _ := h.l.snapshot()
	launchAt := map[string]time.Time{}
	for _, l := range launches {
		if _, ok := launchAt[l.task]; !ok {
			launchAt[l.task] = l.at
		}
	}
	a1 := h.attempts("1.a")
	a2 := h.attempts("2.a")
	if len(a1) == 0 || len(a2) == 0 {
		t.Fatalf("1.a/2.a attempts: %d/%d", len(a1), len(a2))
	}
	if !launchAt["2.c"].Before(a1[0].EndedAt) {
		t.Errorf("2.c launched at %v, after 1.a (an earlier tier) ended at %v: tier barrier", launchAt["2.c"], a1[0].EndedAt)
	}
	if !launchAt["2.c"].Before(a2[len(a2)-1].EndedAt) {
		t.Errorf("2.c launched at %v, after 2.a ended at %v", launchAt["2.c"], a2[len(a2)-1].EndedAt)
	}
	for id, want := range map[string]string{"1.a": "passed", "2.a": "passed", "2.b": "failed", "2.c": "passed", "3.a": "abandoned"} {
		if got := h.task(id).Status; got != want {
			t.Errorf("task %s = %s, want %s", id, got, want)
		}
	}
	if r := h.task("3.a").BlockedReason; !strings.Contains(r, "2.b") {
		t.Errorf("3.a reason = %q, want it to name 2.b", r)
	}
	if _, ok := launchAt["3.a"]; ok {
		t.Error("3.a was launched although 2.b failed")
	}
	evs, err := events.ReadFile(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		var d events.TaskStateData
		if ev.Type == events.TaskState && ev.Task == "3.a" && json.Unmarshal(ev.Data, &d) == nil && d.To == "abandoned" {
			found = strings.Contains(d.Reason, "2.b")
		}
	}
	if !found {
		t.Error("no task_state 3.a -> abandoned event naming 2.b")
	}
}

func TestReconcileRespectsDeps(t *testing.T) {
	h := newHarness(t, diamond()...)
	var mu sync.Mutex
	var violations []string
	h.l.onLaunch = func(req core.LaunchRequest) {
		tr, err := h.store.GetTask(context.Background(), testRun, req.Ref.Task)
		if err != nil {
			t.Error(err)
			return
		}
		for _, d := range tr.Deps {
			if st := h.task(d).Status; st != core.StatusPassed {
				mu.Lock()
				violations = append(violations, fmt.Sprintf("%s launched while dep %s is %s", tr.ID, d, st))
				mu.Unlock()
			}
		}
	}
	h.l.script = func(id string, n int) step {
		if id == "2.b" {
			s := okStep
			s.lineGap = 150 * time.Millisecond
			return s
		}
		return okStep
	}
	if got := h.run(); got != core.RunComplete {
		t.Fatalf("run status = %s", got)
	}
	launches, _ := h.l.snapshot()
	if len(launches) != 4 {
		t.Errorf("launches = %d, want 4", len(launches))
	}
	if last := launches[len(launches)-1]; last.task != "3.a" {
		t.Errorf("last launch = %s, want 3.a", last.task)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, v := range violations {
		t.Error(v)
	}
}

func TestReconcileMaxParallel(t *testing.T) {
	var tasks []spec.Task
	for _, id := range []string{"1.a", "1.b", "1.c", "1.d", "1.e"} {
		tasks = append(tasks, task(id, 1))
	}
	h := newHarness(t, tasks...)
	h.cfg.MaxParallel = 2
	h.cfg.Workers = 3
	h.l.script = func(string, int) step {
		s := okStep
		s.lineGap = 75 * time.Millisecond
		return s
	}
	if got := h.run(); got != core.RunComplete {
		t.Fatalf("run status = %s", got)
	}
	launches, peak := h.l.snapshot()
	if len(launches) != 5 {
		t.Errorf("launches = %d, want 5", len(launches))
	}
	if peak != 2 {
		t.Errorf("peak live handles = %d, want exactly 2", peak)
	}
}

// Two reconcilers on one run (the run lock normally prevents this): both
// plan a dispatch before either applies. The winner's write is foreign to
// the loser, whose pre-check halts the run state_tampered without starting
// a second attempt.
func TestReconcileDispatchRecheck(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("1.b", 1))
	h.cfg.MaxParallel = 1
	store2, err := state.Open(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store2.Close() })
	cfg2 := h.cfg
	cfg2.Store = store2
	cfg2.EventLog = filepath.Join(h.dir, "events2.jsonl")
	r1, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := New(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, r := range []*Reconciler{r1, r2} {
		if err := r.load(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Both reconcilers finish planning (both see 0 active) before either applies.
	var planned sync.WaitGroup
	planned.Add(2)
	allPlanned := make(chan struct{})
	go func() { planned.Wait(); close(allPlanned) }()
	barrier := func(string) {
		planned.Done()
		select {
		case <-allPlanned:
		case <-time.After(5 * time.Second):
			t.Error("barrier: the other reconciler never planned")
		}
	}
	r1.afterPlan, r2.afterPlan = barrier, barrier
	var mu sync.Mutex
	errs := map[string]error{}
	record := func(key string, _ Result, err error) {
		mu.Lock()
		errs[key] = err
		mu.Unlock()
	}
	r1.afterReconcile, r2.afterReconcile = record, record
	r1.queue.Add("task/1.a")
	r2.queue.Add("task/1.b")
	var wg sync.WaitGroup
	for _, r := range []*Reconciler{r1, r2} {
		wg.Add(1)
		go func(r *Reconciler) { defer wg.Done(); r.processNextWorkItem(ctx) }(r)
	}
	wg.Wait()
	r1.cancelChildren()
	r2.cancelChildren()
	r1.bg.Wait()
	r2.bg.Wait()
	r1.queue.ShutDown()
	r2.queue.ShutDown()

	n1, n2 := len(h.attempts("1.a")), len(h.attempts("1.b"))
	if n1+n2 != 1 {
		t.Fatalf("attempt rows: 1.a=%d 1.b=%d, want exactly one in total (MaxParallel 1)", n1, n2)
	}
	// The winner's next write may then see the loser's halt as foreign too.
	loser, lr := "task/1.b", r2
	if n2 == 1 {
		loser, lr = "task/1.a", r1
	}
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(errs[loser], errHalted) || !strings.HasPrefix(lr.haltReason, ReasonTampered+": tasks row") {
		t.Errorf("%s: %v, halt %q; want a state_tampered halt naming the tasks row", loser, errs[loser], lr.haltReason)
	}
}

func TestReconcileStallRetriesWithBackoff(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.script = func(id string, n int) step {
		if n == 1 {
			return step{hang: true} // silent until killed
		}
		return okStep
	}
	if got := h.run(); got != core.RunComplete {
		t.Fatalf("run status = %s", got)
	}
	as := h.attempts("1.a")
	if got := outcomes(as); !reflect.DeepEqual(got, []string{"stalled", "exited"}) {
		t.Fatalf("outcomes = %v, want [stalled exited]", got)
	}
	launches, _ := h.l.snapshot()
	if len(launches) != 2 {
		t.Fatalf("launches = %d", len(launches))
	}
	if gap := launches[1].at.Sub(as[0].EndedAt); gap < h.cfg.RetryDelay(1) {
		t.Errorf("attempt 2 launched %v after attempt 1 ended, want >= %v", gap, h.cfg.RetryDelay(1))
	}
	if h.l.killCount("1.a") != 1 {
		t.Errorf("attempt 1 kills = %d, want 1", h.l.killCount("1.a"))
	}
	if h.task("1.a").Status != core.StatusPassed {
		t.Errorf("status = %s", h.task("1.a").Status)
	}
}

func TestReconcileVerifyFailRetriesThenFails(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MaxAttempts = 2
	h.v.outcome = func(string, int) core.VerifyOutcome { return failOutcome("gate_unit_tests") }
	if got := h.run(); got != core.RunHalted {
		t.Fatalf("run status = %s", got)
	}
	tr := h.task("1.a")
	if tr.Status != core.StatusFailed || tr.AttemptsUsed != 2 {
		t.Fatalf("task = %s after %d attempts, want failed after 2", tr.Status, tr.AttemptsUsed)
	}
	as := h.attempts("1.a")
	if len(as) != 2 || as[0].VerdictResult != "fail" || as[1].VerdictResult != "fail" {
		t.Fatalf("attempts = %+v", as)
	}
	launches, _ := h.l.snapshot()
	if len(launches) != 2 {
		t.Fatalf("launches = %d", len(launches))
	}
	p1, p2 := launches[0].req.Argv[2], launches[1].req.Argv[2]
	if strings.Contains(p1, "gate_unit_tests") {
		t.Error("attempt 1 prompt already mentions the failed gate")
	}
	if !strings.Contains(p2, "Previous attempt did not pass:\nattempt 1 failed verification.") || !strings.Contains(p2, "gate_unit_tests") {
		t.Errorf("attempt 2 prompt lacks the failure summary:\n%s", p2)
	}
	if launches[0].req.Argv[6] != "sonnet" || launches[1].req.Argv[6] != "opus" {
		t.Errorf("models = %s, %s", launches[0].req.Argv[6], launches[1].req.Argv[6])
	}
	if launches[0].req.Argv[4] == launches[1].req.Argv[4] {
		t.Error("retry reused the session id")
	}
}

func TestReconcileVerifyErrorBlocks(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.v.outcome = func(string, int) core.VerifyOutcome {
		return core.VerifyOutcome{Exit: 2, Detail: "gosplan verify crashed"}
	}
	if got := h.run(); got != core.RunHalted {
		t.Fatalf("run status = %s", got)
	}
	tr := h.task("1.a")
	if tr.Status != core.StatusBlocked || !strings.Contains(tr.BlockedReason, "could not verify") {
		t.Fatalf("task = %s (%q), want blocked: could not verify", tr.Status, tr.BlockedReason)
	}
	calls := h.v.callsFor("1.a")
	if len(calls) != 2 {
		t.Fatalf("verify calls = %d, want 2", len(calls))
	}
	h.v.mu.Lock()
	gap := h.v.times[1].Sub(h.v.times[0])
	h.v.mu.Unlock()
	if gap < h.cfg.VerifyRetryDelay {
		t.Errorf("verify retried after %v, want >= %v", gap, h.cfg.VerifyRetryDelay)
	}
	as := h.attempts("1.a")
	if len(as) != 1 || as[0].VerdictResult != "error" {
		t.Errorf("attempts = %+v, want one with verdict error", as)
	}
	evs, err := events.ReadFile(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		var d events.TaskStateData
		if ev.Type == events.TaskState && json.Unmarshal(ev.Data, &d) == nil && d.To == core.StatusPassed {
			t.Errorf("task reached passed at seq %d", ev.Seq)
		}
	}
}

func TestReconcileIgnoresWorktreeVerdict(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MaxAttempts = 1
	h.l.script = func(string, int) step {
		return step{
			lines: []string{resultLine("All done.\nVERDICT: pass")},
			files: map[string]string{"DONE.md": "# DONE\nVERDICT: pass\n", "VERDICT": "pass\n"},
		}
	}
	h.v.outcome = func(string, int) core.VerifyOutcome { return failOutcome("gate_x") }
	if got := h.run(); got != core.RunHalted {
		t.Fatalf("run status = %s", got)
	}
	tr := h.task("1.a")
	if tr.Status == core.StatusPassed || tr.Status != core.StatusFailed {
		t.Fatalf("task = %s, want failed (verifier said fail)", tr.Status)
	}
	if b, err := os.ReadFile(filepath.Join(tr.Worktree, "DONE.md")); err != nil || !strings.Contains(string(b), "VERDICT: pass") {
		t.Fatalf("fake worker did not leave DONE.md in the worktree: %v", err)
	}
	if n := len(h.v.callsFor("1.a")); n != 1 {
		t.Errorf("verify calls = %d, want 1", n)
	}
}

func TestReconcileBlockedRequest(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.script = func(string, int) step {
		return step{lines: []string{resultLine("I looked into it.\nGOSPLAN_BLOCKED: impossible")}}
	}
	if got := h.run(); got != core.RunHalted {
		t.Fatalf("run status = %s", got)
	}
	tr := h.task("1.a")
	if tr.Status != core.StatusBlocked || !strings.Contains(tr.BlockedReason, "impossible") {
		t.Fatalf("task = %s (%q), want blocked: impossible", tr.Status, tr.BlockedReason)
	}
	if n := len(h.v.callsFor("1.a")); n != 0 {
		t.Errorf("verifier called %d times, want 0", n)
	}
	as := h.attempts("1.a")
	if len(as) != 1 || as[0].BlockedRequest != "impossible" || as[0].Outcome != "exited" {
		t.Errorf("attempts = %+v", as)
	}
}

func TestReconcileNegatedBlockedIsVerified(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MaxAttempts = 1
	h.l.script = func(string, int) step {
		return step{lines: []string{resultLine("Finished.\nGOSPLAN_BLOCKED: none \u2014 task complete.")}}
	}
	h.v.outcome = func(string, int) core.VerifyOutcome { return failOutcome("gate_x") }
	h.run()
	tr := h.task("1.a")
	if tr.Status == core.StatusBlocked {
		t.Fatalf("task = %s (%q), a negated GOSPLAN_BLOCKED must not block", tr.Status, tr.BlockedReason)
	}
	if n := len(h.v.callsFor("1.a")); n != 1 {
		t.Errorf("verifier called %d times, want 1", n)
	}
}

func TestReconcileMergeConflictBlocks(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.w.prepareErr = map[string]error{"1.a": fmt.Errorf("merge head-x: %w", core.ErrMergeConflict)}
	if got := h.run(); got != core.RunHalted {
		t.Fatalf("run status = %s", got)
	}
	tr := h.task("1.a")
	if tr.Status != core.StatusBlocked || !strings.Contains(tr.BlockedReason, "merge conflict") {
		t.Fatalf("task = %s (%q), want blocked on merge conflict", tr.Status, tr.BlockedReason)
	}
	if got := outcomes(h.attempts("1.a")); !reflect.DeepEqual(got, []string{"start_failed"}) {
		t.Errorf("outcomes = %v", got)
	}
	if launches, _ := h.l.snapshot(); len(launches) != 0 {
		t.Errorf("launched %d workers after a merge conflict", len(launches))
	}
}

// seedLost leaves 1.a running with an open attempt whose lease belongs to
// another (dead) orchestrator and names pid/pgid, start time and nonce.
func seedLost(t *testing.T, h *harness, pid int, start uint64, nonce string) {
	t.Helper()
	ctx := context.Background()
	var ref core.AttemptRef
	err := h.store.Immediate(ctx, func(tx *sql.Tx) error {
		if err := state.Transition(tx, time.Now(), testRun, "1.a", core.StatusPending, core.StatusRunning, "seed"); err != nil {
			return err
		}
		var err error
		ref, err = state.StartAttempt(tx, time.Now(), state.AttemptStart{
			Run: testRun, Task: "1.a", SessionID: "s", Model: "sonnet", Owner: "other:1:x",
			StreamPath: filepath.Join(h.dir, "lost.jsonl"), Nonce: nonce,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if pid != 0 {
		if err := h.store.SetLeaseProcess(ctx, ref, pid, pid, start); err != nil {
			t.Fatal(err)
		}
	}
	h.cfg.Resume = true
}

// sleeper starts `sleep 60` in its own group with env, and reports how it ended.
func sleeper(t *testing.T, env ...string) (*exec.Cmd, chan error) {
	t.Helper()
	c := exec.Command("sleep", "60")
	c.Env = append(os.Environ(), env...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- c.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL) })
	return c, waited
}

func wantKilled(t *testing.T, waited chan error, want bool) {
	t.Helper()
	select {
	case err := <-waited:
		var ee *exec.ExitError
		killed := errors.As(err, &ee) && ee.Sys().(syscall.WaitStatus).Signal() == syscall.SIGKILL
		if !want || !killed {
			t.Errorf("process ended with %v; want killed=%v", err, want)
		}
	case <-time.After(3 * time.Second):
		if want {
			t.Error("the orphaned worker was not killed")
		}
	}
}

func wantLostThenPassed(t *testing.T, h *harness) {
	t.Helper()
	if got := h.run(); got != core.RunComplete {
		t.Fatalf("run status = %s", got)
	}
	if got := outcomes(h.attempts("1.a")); !reflect.DeepEqual(got, []string{"lost", "exited"}) {
		t.Errorf("outcomes = %v, want [lost exited]", got)
	}
}

// The orphan's leader still has the start time the lease recorded: it is
// ours, its group is killed.
func TestReconcileRecoversLostLease(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	c, waited := sleeper(t)
	st, err := driver.ProcStartTime(c.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	seedLost(t, h, c.Process.Pid, st, "")
	wantLostThenPassed(t, h)
	wantKilled(t, waited, true)
}

// No start time, but the orphan carries the lease's nonce: ours, killed.
func TestReconcileLostLeaseNonceMatch(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	nonce := testRun + ".feedface"
	c, waited := sleeper(t, driver.EnvNonce+"="+nonce)
	seedLost(t, h, c.Process.Pid, 0, nonce)
	wantLostThenPassed(t, h)
	wantKilled(t, waited, true)
}

// The lease's pid/pgid now belong to an unrelated process (pid reuse): the
// start time differs and it has no nonce, so it is not killed; the attempt
// still ends lost and the task is retried.
func TestReconcileLostLeaseReusedPidNotKilled(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	c, waited := sleeper(t)
	st, _ := driver.ProcStartTime(c.Process.Pid)
	seedLost(t, h, c.Process.Pid, st+1, testRun+".other")
	wantLostThenPassed(t, h)
	wantKilled(t, waited, false)
}

// The lease names another user's process (kill gives EPERM). Before, the
// group looked alive, the kill failed and the task wedged in running.
func TestReconcileLostLeaseEPERMNoWedge(t *testing.T) {
	pid := foreignPID(t)
	h := newHarness(t, task("1.a", 1))
	seedLost(t, h, pid, 1, testRun+".x")
	wantLostThenPassed(t, h)
}

// A worker started but its pid never reached the lease (crash between
// Launch and SetLeaseProcess): the resume sweep finds it by the run's nonce
// prefix and kills it.
func TestReconcileSweepKillsUnrecordedWorker(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	nonce := testRun + ".0badc0de"
	_, waited := sleeper(t, driver.EnvNonce+"="+nonce)
	seedLost(t, h, 0, 0, nonce)
	wantLostThenPassed(t, h)
	wantKilled(t, waited, true)
}

// A crash before the pid reached the lease, with the worker leader still
// alive and a same-group child that dropped the nonce (the reviewer's
// prelaunch case): the sweep kills the leader's whole group.
func TestReconcileSweepKillsUnrecordedWorkerGroup(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	nonce := testRun + ".0badc0de"
	c := exec.Command("sh", "-c", `env -u `+driver.EnvNonce+` sleep 60 </dev/null >/dev/null 2>&1 & echo $!; wait`)
	c.Env = append(os.Environ(), driver.EnvNonce+"="+nonce)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL); _ = c.Wait() })
	var b [32]byte
	n, _ := out.Read(b[:])
	child, err := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	if err != nil {
		t.Fatal(err)
	}
	seedLost(t, h, 0, 0, nonce)
	wantLostThenPassed(t, h)
	for i := 0; i < 50 && driver.Alive(child); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if driver.Alive(child) {
		t.Error("nonce-less child of an unrecorded worker survived the resume")
	}
}

// A resume that halts on its checks still kills the crashed run's
// processes first (before: the halt returned before the sweep, and the
// straggler kept running).
func TestReconcileResumeHaltStillSweeps(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	_, waited := sleeper(t, driver.EnvNonce+"="+testRun+".deadbeef")
	h.exec(`UPDATE tasks SET scope_json = '["**"]'`)
	h.cfg.Resume = true
	wantHalt(t, h.runOutcome(), ReasonTampered+": ", "scope_json")
	wantKilled(t, waited, true)
}

// Re-review 5b: the crashed run's `gosplan verify` (verifyexec gives it the
// nonce "<run>.verify.<task>.<n>") is stopped by the resume sweep (SIGTERM,
// so it can cancel its gates) before the resumed run starts a verify of its
// own; one that ignores SIGTERM is SIGKILLed after the grace.
func TestReconcileSweepKillsStragglerVerify(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	c, waited := sleeper(t, driver.EnvNonce+"="+testRun+".verify.1.a.1")
	stubborn := exec.Command("sh", "-c", "trap '' TERM; sleep 60 & wait")
	stubborn.Env = append(os.Environ(), driver.EnvNonce+"="+testRun+".verify.1.a.2")
	if err := stubborn.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stubborn.Process.Kill() })
	stubbornDone := make(chan error, 1)
	go func() { stubbornDone <- stubborn.Wait() }()
	pid := c.Process.Pid
	var aliveAtVerify []bool
	var mu sync.Mutex
	h.v.outcomeReq = func(req core.VerifyRequest) core.VerifyOutcome {
		mu.Lock()
		defer mu.Unlock()
		aliveAtVerify = append(aliveAtVerify, driver.Alive(pid) || driver.Alive(stubborn.Process.Pid))
		return passOutcome
	}
	seedLost(t, h, 0, 0, testRun+".gone")
	wantLostThenPassed(t, h)
	select {
	case err := <-waited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Errorf("straggler ended with %v, want SIGTERM", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("straggler verify not stopped")
	}
	select {
	case err := <-stubbornDone:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Errorf("stubborn straggler ended with %v, want SIGKILL", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("stubborn straggler not killed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(aliveAtVerify) == 0 || aliveAtVerify[0] {
		t.Errorf("straggler verify alive when the first verify started: %v", aliveAtVerify)
	}
}

// foreignPID is a live process of another user, or skips.
func foreignPID(t *testing.T) int {
	t.Helper()
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		var st syscall.Stat_t
		pid, err := strconv.Atoi(filepath.Base(d))
		if err != nil || pid <= 1 || syscall.Stat(d, &st) != nil || int(st.Uid) == os.Getuid() {
			continue
		}
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.EPERM) {
			return pid
		}
	}
	t.Skip("no process of another user visible")
	return 0
}

func TestReconcileCancelAborts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.script = func(string, int) step { return step{hang: true, heartbeat: 50 * time.Millisecond} }
	r, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if l, _ := h.l.snapshot(); len(l) > 0 {
				time.Sleep(100 * time.Millisecond)
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	done := make(chan struct{})
	var status string
	go func() { defer close(done); status, err = r.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if err != nil || status != core.RunAborted {
		t.Fatalf("Run = %q, %v; want aborted", status, err)
	}
	if h.l.killCount("1.a") != 1 {
		t.Errorf("kills = %d, want 1", h.l.killCount("1.a"))
	}
	if got := outcomes(h.attempts("1.a")); !reflect.DeepEqual(got, []string{"killed"}) {
		t.Errorf("outcomes = %v, want [killed]", got)
	}
	run, err := h.store.GetRun(context.Background(), testRun)
	if err != nil || run.Status != core.RunAborted {
		t.Errorf("run row = %+v, %v", run, err)
	}
	if ls, _ := h.store.Leases(context.Background(), testRun); len(ls) != 0 {
		t.Errorf("leases left: %+v", ls)
	}
}

func TestReconcileUsesWorkqueue(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	r, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.queue.ShutDown()
	var mu sync.Mutex
	calls := map[string]int{}
	started, release := make(chan struct{}), make(chan struct{})
	r.reconcileFn = func(ctx context.Context, key string) (Result, error) {
		mu.Lock()
		calls[key]++
		n := calls[key]
		mu.Unlock()
		if n == 1 {
			close(started)
			<-release
		}
		return Result{}, nil
	}
	const key = "task/1.a"
	r.queue.Add(key)
	first := make(chan struct{})
	go func() { defer close(first); r.processNextWorkItem(context.Background()) }()
	<-started
	r.queue.Add(key)
	r.queue.Add(key)
	if n := r.queue.Len(); n != 0 {
		t.Errorf("queue length while processing = %d, want 0 (key held in the dirty set)", n)
	}
	close(release)
	<-first
	if n := r.queue.Len(); n != 1 {
		t.Fatalf("queue length after Done = %d, want 1 (two Adds collapse to one)", n)
	}
	r.processNextWorkItem(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if calls[key] != 2 {
		t.Errorf("Reconcile calls for %s = %d, want 2", key, calls[key])
	}
	if n := r.queue.Len(); n != 0 {
		t.Errorf("queue length at end = %d, want 0", n)
	}
}

func TestDefaultPrompt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "1.a.md")
	if err := os.WriteFile(p, []byte("Build the thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := state.TaskRow{ID: "1.a", PromptPath: p}
	first, err := DefaultPrompt(tr, 1, "stale failure")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, Preamble) || !strings.Contains(first, "GOSPLAN_BLOCKED: <reason>") ||
		!strings.HasSuffix(first, "Build the thing.\n") || strings.Contains(first, "stale failure") {
		t.Errorf("attempt 1 prompt:\n%s", first)
	}
	second, err := DefaultPrompt(tr, 2, "failed gates: gate_q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "Previous attempt did not pass:\nfailed gates: gate_q") {
		t.Errorf("attempt 2 prompt:\n%s", second)
	}
	if _, err := DefaultPrompt(state.TaskRow{ID: "x", PromptPath: filepath.Join(t.TempDir(), "missing.md")}, 1, ""); err == nil {
		t.Error("missing prompt file: no error")
	}
}
