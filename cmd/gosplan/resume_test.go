package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// slowCheck makes the project's test gate take secs seconds, so a test can
// kill gosplan run while a task is verifying.
func (f *advFix) slowCheck(secs int) {
	f.t.Helper()
	write(f.t, f.project, "check.sh", "sleep "+strconv.Itoa(secs)+"; echo done >> "+f.logf+".checks; ls fake-*.txt >/dev/null 2>&1 && ! grep -q BAD fake-*.txt\n")
	git(f.t, f.project, "commit", "-q", "-am", "slow check")
}

// start launches gosplan run (extra flags appended) without waiting for it.
func (f *advFix) start(extra []string, env ...string) *exec.Cmd {
	f.t.Helper()
	args := append([]string{"run", "--spec", f.spec, "--project", f.project, "--state-dir", f.state,
		"--claude", f.fake, "--gosplan-bin", f.gosplan, "--max-parallel", "3", "--max-attempts", "2", "--stall-timeout", "30s",
		"--retry-base", "1s", "--mutation", "off", "--usage-probe=false"}, extra...)
	cmd := exec.Command(f.gosplan, args...)
	cmd.Dir = f.project
	cmd.Env = append(os.Environ(), append([]string{"ADV_LOG=" + f.logf, "ADV_DB=" + filepath.Join(f.state, "gosplan.db"),
		"SQLITE3=" + f.sqlite3}, env...)...)
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// sql runs one statement with the sqlite3 CLI (as a worker could) and
// returns its trimmed output.
func (f *advFix) sql(q string) string {
	f.t.Helper()
	out, err := exec.Command(f.sqlite3, "-cmd", ".timeout 5000", filepath.Join(f.state, "gosplan.db"), q).CombinedOutput()
	if err != nil {
		f.t.Fatalf("sqlite3 %q: %v\n%s", q, err, out)
	}
	return strings.TrimSpace(string(out))
}

// waitSQL polls until q returns want (2m cap).
func (f *advFix) waitSQL(q, want string) {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(f.state, "gosplan.db")); err == nil {
			out, err := exec.Command(f.sqlite3, "-cmd", ".timeout 2000", filepath.Join(f.state, "gosplan.db"), q).Output()
			if err == nil && strings.TrimSpace(string(out)) == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatalf("%q never returned %q\nadv.log:\n%s", q, want, f.log())
}

// crash SIGKILLs gosplan run (only the orchestrator: its workers and verifies
// run in their own process groups and survive, as after a real crash).
func (f *advFix) crash(cmd *exec.Cmd) {
	f.t.Helper()
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()
}

// runID is the only run in the state dir.
func (f *advFix) runID() string { return f.sql("SELECT id FROM runs") }

// killVerifies SIGKILLs every `gosplan verify` left running on this fixture's
// project (cleanup for crash tests).
func (f *advFix) killVerifies() {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		b, err := os.ReadFile(d + "/cmdline")
		if err != nil || !strings.Contains(string(b), "verify\x00--project\x00"+f.project+"\x00") {
			continue
		}
		if pid, err := strconv.Atoi(filepath.Base(d)); err == nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// scoperesume (re-review finding 1): the worker edits README outside its
// scope; gosplan run is killed during verify; scope_json is set to null while it
// is down. Before: the resume re-verified unrestricted and integrated the
// README edit, complete, exit 0. Now the resume halts state_tampered naming
// the scope and nothing is integrated.
func TestRunE2EResumeForgedScope(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	t.Cleanup(f.killVerifies)
	f.slowCheck(4)
	cmd := f.start(nil, "ADV_MODE=outscope")
	f.waitSQL("SELECT status FROM tasks WHERE id='1.a'", "verifying")
	f.crash(cmd)
	f.sql("UPDATE tasks SET scope_json='null' WHERE id='1.a'")
	res := f.runArgs([]string{"--run", f.runID()}, "ADV_MODE=happy")
	f.wantTampered(res, `tasks row 1.a: scope_json db="[]" spec="[fake-1.a.txt tests/**]"`)
	f.noIntegrated("README", "hacked")
	if n := f.sql("SELECT count(*) FROM runs WHERE integration_sha IS NOT NULL AND integration_sha != ''"); n != "0" {
		t.Errorf("integration_sha recorded on %s runs", n)
	}
}

// Crash loop (re-review finding 4): the worker's attempt is lost every time
// gosplan run dies (here: kill -9 while it runs; a worker could do it itself).
// Lost attempts are refunded, so before, every resume relaunched it at full
// quota, forever. Now, once the task has MaxAttempts (2) lost/start_failed
// attempts, the next dispatch blocks it `infrastructure` and the run halts.
func TestRunE2ECrashLoopIsBounded(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	launched := "SELECT count(*) FROM leases WHERE pid IS NOT NULL"
	cmd := f.start(nil, "ADV_MODE=hang")
	f.waitSQL(launched, "1")
	f.crash(cmd)
	run := f.runID()

	cmd = f.start([]string{"--run", run}, "ADV_MODE=hang")
	f.waitSQL("SELECT count(*) FROM attempts WHERE outcome = 'lost'", "1")
	f.waitSQL(launched, "1")
	f.crash(cmd)

	res := f.runArgs([]string{"--run", run}, "ADV_MODE=hang")
	if res.code != 1 || !strings.Contains(res.stderr, "1.a (blocked)") {
		t.Fatalf("want exit 1 with 1.a blocked, got %d\n%s", res.code, res.stderr)
	}
	if got := f.sql("SELECT blocked_reason FROM tasks WHERE id = '1.a'"); got != "infrastructure: 2 lost/start_failed attempts" {
		t.Errorf("blocked_reason %q", got)
	}
	if n := strings.Count(f.log(), "hang 1.a "); n != 2 {
		t.Errorf("worker launched %d times, want 2\n%s", n, f.log())
	}
	if got := f.sql("SELECT group_concat(outcome, ',') FROM (SELECT outcome FROM attempts ORDER BY n)"); got != "lost,lost" {
		t.Errorf("attempt outcomes %q", got)
	}
}

// logPID is the pid the fake logged as "<word> <pid>".
func (f *advFix) logPID(word string) int {
	for _, l := range strings.Split(f.log(), "\n") {
		if rest, ok := strings.CutPrefix(l, word+" "); ok {
			if pid, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				return pid
			}
		}
	}
	f.t.Fatalf("no %q pid in adv.log:\n%s", word, f.log())
	return 0
}

func procAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	return i < 0 || !strings.HasPrefix(strings.TrimSpace(string(b[i+1:])), "Z")
}

// crashleader (re-review finding 5a): gosplan run is killed while the worker
// runs; the worker's leader then exits, leaving a same-group child that
// dropped GOSPLAN_ATTEMPT_NONCE. Before: the resume skipped the group kill
// (the leader no longer passed Owns) and the child survived. Now the group
// is killed because a member still carries the run (GOSPLAN_RUN).
func TestRunE2EResumeKillsGroupOfDeadLeader(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	cmd := f.start(nil, "ADV_MODE=dropnonce")
	f.waitSQL("SELECT count(*) FROM leases WHERE pid IS NOT NULL", "1")
	leader, _ := strconv.Atoi(f.sql("SELECT pid FROM leases"))
	for !strings.Contains(f.log(), "child ") {
		time.Sleep(50 * time.Millisecond)
	}
	f.crash(cmd)
	child := f.logPID("child")
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	for deadline := time.Now().Add(20 * time.Second); procAlive(leader); {
		if time.Now().After(deadline) {
			t.Fatal("leader did not exit")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !procAlive(child) {
		t.Fatal("child died before the resume")
	}
	res := f.runArgs([]string{"--run", f.runID()}, "ADV_MODE=happy")
	if res.code != 0 {
		t.Fatalf("resume exit %d\n%s", res.code, res.stderr)
	}
	time.Sleep(200 * time.Millisecond)
	if procAlive(child) {
		t.Fatalf("same-group child %d of the dead leader survived the resume", child)
	}
}

// verifyPIDs are the live `gosplan verify` processes on this fixture's project.
func (f *advFix) verifyPIDs() []int {
	var out []int
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		b, err := os.ReadFile(d + "/cmdline")
		if err != nil || !strings.Contains(string(b), "verify\x00--project\x00"+f.project+"\x00") {
			continue
		}
		if pid, err := strconv.Atoi(filepath.Base(d)); err == nil && procAlive(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// crashverify (re-review finding 5b): gosplan run is killed during verify; its
// `gosplan verify` survives. Before: the straggler kept running through the
// resume and wrote the same verify/<task>.<n>.* files as the resumed
// verify (one could-not-verify, a 30s retry; two in a row block the
// task). Now the resume kills it by its verify nonce before verifying, and
// each invocation has its own verify/<task>.<n>.<seq>/ directory.
func TestRunE2EResumeKillsStragglerVerify(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	t.Cleanup(f.killVerifies)
	f.slowCheck(3)
	cmd := f.start(nil, "ADV_MODE=happy")
	f.waitSQL("SELECT status FROM tasks WHERE id='1.a'", "verifying")
	for deadline := time.Now().Add(10 * time.Second); len(f.verifyPIDs()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no gosplan verify running")
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.crash(cmd)
	stragglers := f.verifyPIDs()
	if len(stragglers) == 0 {
		t.Fatal("the crashed run's gosplan verify is already gone")
	}
	run := f.runID()
	res := f.runArgs([]string{"--run", run}, "ADV_MODE=happy")
	if res.code != 0 {
		t.Fatalf("resume exit %d\n%s", res.code, res.stderr)
	}
	for _, p := range stragglers {
		if procAlive(p) {
			t.Errorf("straggler gosplan verify %d survived the resume", p)
		}
	}
	// Only the resumed verify's gate finished: the straggler's was killed.
	if b, _ := os.ReadFile(f.logf + ".checks"); strings.Count(string(b), "done") != 1 {
		t.Errorf("test gate completed %d times, want 1 (the straggler's must be killed)", strings.Count(string(b), "done"))
	}
	if got := f.sql("SELECT verify_errors || ',' || attempts_used FROM tasks WHERE id='1.a'"); got != "0,1" {
		t.Errorf("verify_errors,attempts_used = %s, want 0,1", got)
	}
	if got := f.sql("SELECT group_concat(coalesce(verdict_result,'-'), ',') FROM attempts"); got != "pass" {
		t.Errorf("verdicts %q", got)
	}
	dirs, _ := filepath.Glob(filepath.Join(f.state, "runs", run, "verify", "1.a.1.*"))
	if len(dirs) != 2 {
		t.Errorf("verify dirs %v, want 1.a.1.1 (crashed) and 1.a.1.2 (resume)", dirs)
	}
}

// Old-run resume (re-review finding 6): `gosplan run --run ID` on a run that
// already completed, with a log in the old format (no integration_sha=).
// Before: the resume rewrote it halted state_tampered. Now it reports the
// recorded status (exit 0 for complete, 1 for halted) and writes nothing.
func TestRunE2EResumeEndedRunWritesNothing(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	if res := f.run("ADV_MODE=happy"); res.code != 0 {
		t.Fatalf("first run exit %d\n%s", res.code, res.stderr)
	}
	run := f.runID()
	dir := filepath.Join(f.state, "runs", run)
	logPath := filepath.Join(dir, "events.jsonl")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	old := regexp.MustCompile(`;? ?integration_sha=[0-9a-f]+`).ReplaceAllString(string(b), "")
	if old == string(b) {
		t.Fatal("log has no integration_sha= to strip")
	}
	if err := os.WriteFile(logPath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := func() string {
		var out []string
		for _, n := range []string{"events.jsonl", "state.json", "report.json"} {
			st, err := os.Stat(filepath.Join(dir, n))
			if err != nil {
				out = append(out, n+": "+err.Error())
				continue
			}
			c, _ := os.ReadFile(filepath.Join(dir, n))
			out = append(out, fmt.Sprintf("%s %d %s %x", n, st.Size(), st.ModTime(), sha256.Sum256(c)))
		}
		out = append(out, f.sql("SELECT status || ' ' || coalesce(finished_at,'') || ' ' || coalesce(integration_sha,'') FROM runs"),
			f.sql("SELECT count(*) FROM events"), f.sql("SELECT group_concat(id || '=' || status) FROM tasks"))
		return strings.Join(out, "\n")
	}
	for _, c := range []struct {
		status string
		code   int
	}{{"complete", 0}, {"halted", 1}} {
		f.sql("UPDATE runs SET status = '" + c.status + "'")
		before := snap()
		res := f.runArgs([]string{"--run", run}, "ADV_MODE=happy")
		if res.code != c.code || !strings.Contains(res.stderr, "already ended "+c.status) {
			t.Fatalf("%s: resume exit %d, want %d\n%s", c.status, res.code, c.code, res.stderr)
		}
		if after := snap(); after != before {
			t.Errorf("%s: resume wrote something:\nbefore:\n%s\nafter:\n%s", c.status, before, after)
		}
	}
	if n := strings.Count(f.log(), "happy 1.a"); n != 1 {
		t.Errorf("worker launched %d times", n)
	}
}
