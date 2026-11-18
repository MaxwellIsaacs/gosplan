// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// run_crash_test.go: RED BY DESIGN. Crash recovery, leases, stall detection
// and retry backoff.
package conformance

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// bg is an `gosplan run` running in the background.
type bg struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (f *runFix) startBG(extra ...string) *bg {
	b := &bg{cmd: f.startNR(extra...), done: make(chan struct{})}
	go func() { _ = b.cmd.Wait(); close(b.done) }()
	return b
}

func (b *bg) exited() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// waitRec waits (up to 90s) for a ledger record, failing at once if gosplan exits first.
func (f *runFix) waitRec(b *bg, event, task, attempt string) ledgerRec {
	f.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if r, ok := f.find(event, task, attempt); ok {
			return r
		}
		if b.exited() {
			f.t.Fatalf("gosplan run exited (code %d) before worker %s attempt %s recorded %q", b.cmd.ProcessState.ExitCode(), task, attempt, event)
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("worker %s attempt %s never recorded %q within 90s", task, attempt, event)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// crash SIGKILLs the orchestrator process only (not the worker's group).
func (b *bg) crash(t *testing.T) {
	t.Helper()
	_ = b.cmd.Process.Signal(syscall.SIGKILL)
	select {
	case <-b.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("gosplan run did not die after SIGKILL")
	}
}

// TestRunLeaseReclaimAfterCrash: after the orchestrator dies mid-attempt, a
// restart kills the orphaned worker group, ends the attempt `lost` and retries.
// Clause: orchestrator#DY-LEASE-RECLAIM
func TestRunLeaseReclaimAfterCrash(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a.1": {{"op": "spawn_child"}, {"op": "hang"}}})
	b := f.startBG("--stall-timeout", "10m")
	child := f.waitRec(b, "child", "1.a", "1")
	b.crash(t)
	if !pidAlive(child.PID) || !pidAlive(child.ChildPID) {
		t.Fatalf("worker group died with the orchestrator; it must run in its own process group (pid %d child %d)", child.PID, child.ChildPID)
	}
	t.Cleanup(func() { _ = syscall.Kill(-child.PGID, syscall.SIGKILL) })
	run := f.onlyRun()
	mustExit(t, "gosplan run --run "+run+" (resume)", f.runNR("--run", run, "--stall-timeout", "10m"), 0)
	if !eventually(10*time.Second, func() bool { return !pidAlive(child.PID) && !pidAlive(child.ChildPID) }) {
		t.Fatalf("orphaned worker group survived the restart (pid %d child %d)", child.PID, child.ChildPID)
	}
	r, _ := f.status()
	x := r.task(t, "1.a")
	if x.Status != "passed" || len(x.Attempts) != 2 || x.Attempts[0].Outcome != "lost" {
		t.Fatalf("1.a: want passed with attempt 1 lost and a second attempt, got %s %+v", x.Status, x.Attempts)
	}
}

// TestRunStallKillsGroupAndRetries: a silent worker is killed with its whole
// group after the stall timeout, ends `stalled`, and is retried.
// Clause: orchestrator#DY-STALL
func TestRunStallKillsGroupAndRetries(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a.1": {{"op": "spawn_child"}, {"op": "hang"}}})
	res := f.runNR("--stall-timeout", "2s")
	child, ok := f.find("child", "1.a", "1")
	if ok {
		t.Cleanup(func() { _ = syscall.Kill(-child.PGID, syscall.SIGKILL) })
	}
	mustExit(t, "gosplan run --stall-timeout 2s", res, 0)
	if !ok {
		t.Fatalf("1.a attempt 1 never ran")
	}
	if !eventually(10*time.Second, func() bool { return !pidAlive(child.PID) && !pidAlive(child.ChildPID) }) {
		t.Fatalf("stalled worker group not killed (pid %d child %d)", child.PID, child.ChildPID)
	}
	r, _ := f.status()
	x := r.task(t, "1.a")
	if x.Status != "passed" || len(x.Attempts) != 2 || x.Attempts[0].Outcome != "stalled" || x.Attempts[1].Outcome != "exited" {
		t.Fatalf("1.a: want passed with outcomes [stalled exited], got %s %+v", x.Status, x.Attempts)
	}
}

// TestRunRetryBackoff: the delay before attempt n+1 is base·2^(n-1): with
// stall 2s and --retry-base 2s, attempt 2 starts ≥ 3s after attempt 1 and
// attempt 3 ≥ 5s after attempt 2 (1s slack each).
// Clause: orchestrator#DY-BACKOFF
func TestRunRetryBackoff(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a.1": {{"op": "hang"}}, "1.a.2": {{"op": "hang"}}})
	res := f.runNR("--stall-timeout", "2s", "--retry-base", "2s", "--max-attempts", "3")
	mustExit(t, "gosplan run (two stalls, then pass)", res, 0)
	st := f.starts("1.a")
	if len(st) != 3 {
		t.Fatalf("want 3 attempts, got %d", len(st))
	}
	if g := st[1].TMS - st[0].TMS; g < 3000 {
		t.Fatalf("attempt 2 started %dms after attempt 1; want ≥ 3000 (stall 2s + delay 2s - 1s)", g)
	}
	if g := st[2].TMS - st[1].TMS; g < 5000 {
		t.Fatalf("attempt 3 started %dms after attempt 2; want ≥ 5000 (stall 2s + delay 4s - 1s)", g)
	}
}

// TestRunMaxAttemptsThenFailed: after --max-attempts failures the task is
// failed and never launched again.
// Clause: orchestrator#DY-BACKOFF
func TestRunMaxAttemptsThenFailed(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a": {{"op": "exit", "code": 3}}})
	mustExit(t, "gosplan run (worker always exits 3)", f.runNR("--max-attempts", "2"), 1)
	if n := len(f.starts("1.a")); n != 2 {
		t.Fatalf("want exactly 2 launches with --max-attempts 2, got %d", n)
	}
	r, _ := f.status()
	if s := r.task(t, "1.a").Status; s != "failed" {
		t.Fatalf("1.a: want failed, got %s", s)
	}
}

// TestRunResumeSkipsPassed: a crash during 2.a, then `gosplan run --run ID`,
// finishes the run without re-running the already passed 1.a.
// Clause: orchestrator#DY-RESUME
func TestRunResumeSkipsPassed(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a", nil)
	f.scenario(map[string][]map[string]any{"2.a.1": {{"op": "spawn_child"}, {"op": "hang"}}})
	b := f.startBG("--stall-timeout", "10m")
	child := f.waitRec(b, "child", "2.a", "1")
	b.crash(t)
	t.Cleanup(func() { _ = syscall.Kill(-child.PGID, syscall.SIGKILL) })
	run := f.onlyRun()
	mustExit(t, "gosplan run --run "+run+" (resume)", f.runNR("--run", run, "--stall-timeout", "10m"), 0)
	if n := len(f.starts("1.a")); n != 1 {
		t.Fatalf("passed task 1.a re-ran on resume: %d launches", n)
	}
	r, _ := f.status()
	if r.Status != "complete" || r.task(t, "2.a").Status != "passed" || len(r.task(t, "1.a").Attempts) != 1 {
		t.Fatalf("after resume: want complete, 2.a passed, 1.a one attempt; got %s %+v", r.Status, r.Tasks)
	}
}
