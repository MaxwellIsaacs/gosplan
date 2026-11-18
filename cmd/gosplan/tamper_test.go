package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// advFake is the reviewer's adversarial worker (fake.sh), cut down to
// the DB-forging modes. It runs as the same uid as gosplan and writes gosplan.db with
// the sqlite3 CLI, as a real worker could.
const advFake = `#!/usr/bin/env bash
set -u
if [ "${2:-}" = "/usage" ]; then echo '{"type":"result","subtype":"success","is_error":false,"result":""}'; exit 0; fi
t=${GOSPLAN_TASK//./_}; mv="ADV_MODE_$t"; mode=${!mv:-${ADV_MODE:-happy}}
res(){ printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","num_turns":1}\n' "$1"; }
G="git -c user.name=t -c user.email=t@x -c commit.gpgsign=false"
echo '{"type":"system","subtype":"init","session_id":"s","model":"m"}'
echo "$mode $GOSPLAN_TASK $GOSPLAN_ATTEMPT" >> "$ADV_LOG"
commit(){ echo "${1:-ok}" > "fake-$GOSPLAN_TASK.txt"; $G add "fake-$GOSPLAN_TASK.txt" >/dev/null; $G commit -qm "task $GOSPLAN_TASK" >/dev/null; }
DB=$ADV_DB
q(){ "$SQLITE3" "$DB" "PRAGMA busy_timeout=5000; $1" 2>>"$ADV_LOG"; }
case $mode in
 happy) commit; res done;;
 setsidhold) commit; res done; setsid sleep 600 & echo "setsid $!" >> "$ADV_LOG"; sleep 0.2;;
 setsidhang) commit; setsid sleep 600 & echo "setsid $!" >> "$ADV_LOG"; sleep 600;;
 outscope) commit; echo hacked >> README; $G add README; $G commit -qm readme; res done;;
 endedat) commit; q "UPDATE attempts SET ended_at='2026-01-01T00:00:00Z' WHERE run_id='$GOSPLAN_RUN' AND task_id='$GOSPLAN_TASK' AND ended_at IS NULL;"; res done;;
 hang) commit; sleep 3600;;
 dropnonce) commit; env -u GOSPLAN_ATTEMPT_NONCE sleep 902 </dev/null >/dev/null 2>&1 & echo "child $!" >> "$ADV_LOG"; sleep 3; exit 0;;
 conflict) echo "$GOSPLAN_TASK" > tests/shared.txt; $G add tests/shared.txt; commit; res done;;
 dbpass) commit BAD; q "UPDATE tasks SET status='passed' WHERE run_id='$GOSPLAN_RUN' AND id='$GOSPLAN_TASK';"; res done;;
 dbother) commit; $G checkout -q -b evil; echo BAD > fake-2.a.txt; $G add fake-2.a.txt; $G commit -qm evil; E=$($G rev-parse HEAD); $G checkout -q -
   q "UPDATE tasks SET status='passed', head_sha='$E' WHERE run_id='$GOSPLAN_RUN' AND id='2.a';"; res done;;
 dbbase2) echo tampered >> tests/guard.txt; $G add tests/guard.txt; $G commit -qm tamper; B=$($G rev-parse HEAD); commit
   q "UPDATE tasks SET base_sha='$B' WHERE run_id='$GOSPLAN_RUN' AND id='$GOSPLAN_TASK';"; res done;;
esac
exit 0
`

type advFix struct {
	t                          *testing.T
	gosplan, fake, sqlite3     string
	spec, project, state, logf string
}

func sqlite3CLI(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("GOSPLAN_CONF_SQLITE3"); p != "" {
		return p
	}
	p, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("needs the sqlite3 CLI")
	}
	return p
}

// newAdvFix is the reviewer's mk.sh: tasks of seq with scope fake-<id>.txt
// and tests/**, a project whose test gate fails on BAD, and a protected
// tests/guard.txt.
func newAdvFix(t *testing.T, seq string, ids ...string) *advFix {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash")
	}
	f := &advFix{t: t, gosplan: runBinaries(t).gosplan, sqlite3: sqlite3CLI(t), spec: t.TempDir(), state: t.TempDir()}
	f.logf = filepath.Join(t.TempDir(), "adv.log")
	f.fake = filepath.Join(t.TempDir(), "fake.sh")
	if err := os.WriteFile(f.fake, []byte(advFake), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, f.spec, "spec.md", "# spec\n")
	write(t, f.spec, "prompts/sequence.txt", seq+"\n")
	for _, id := range ids {
		write(t, f.spec, "prompts/"+id+".md", "# task "+id+"\n")
		write(t, f.spec, "prompts/"+id+".task.toml", fmt.Sprintf("scope = [\"fake-%s.txt\", \"tests/**\"]\nallow_new_protected = true\n", id))
	}
	f.project, _ = runProject(t)
	write(t, f.project, "tests/guard.txt", "guard\n")
	git(t, f.project, "add", "tests/guard.txt")
	git(t, f.project, "commit", "-q", "-m", "guard")
	return f
}

func (f *advFix) run(env ...string) runResult {
	f.t.Helper()
	return f.runArgs(nil, env...)
}

// runArgs is run with extra gosplan run flags.
func (f *advFix) runArgs(extra []string, env ...string) runResult {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := append([]string{"run", "--spec", f.spec, "--project", f.project, "--state-dir", f.state,
		"--claude", f.fake, "--gosplan-bin", f.gosplan, "--max-parallel", "3", "--max-attempts", "2", "--stall-timeout", "5s",
		"--retry-base", "1s", "--mutation", "off", "--usage-probe=false"}, extra...)
	cmd := exec.CommandContext(ctx, f.gosplan, args...)
	cmd.Dir = f.project
	cmd.Env = append(os.Environ(), append([]string{"ADV_LOG=" + f.logf, "ADV_DB=" + filepath.Join(f.state, "gosplan.db"),
		"SQLITE3=" + f.sqlite3}, env...)...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	_ = cmd.Run()
	if ctx.Err() != nil {
		f.t.Fatalf("gosplan run did not finish within 2m (the run idles)\nstderr:\n%s", se.String())
	}
	return runResult{cmd.ProcessState.ExitCode(), so.String(), se.String()}
}

func (f *advFix) wantTampered(res runResult, want string) {
	f.t.Helper()
	if res.code != 1 || !strings.Contains(res.stderr, "halted (state_tampered: ") || !strings.Contains(res.stderr, want) {
		f.t.Fatalf("want exit 1 and a state_tampered halt naming %q, got exit %d\nstderr:\n%s\nadv.log:\n%s",
			want, res.code, res.stderr, f.log())
	}
}

func (f *advFix) log() string {
	b, _ := os.ReadFile(f.logf)
	return string(b)
}

// noIntegrationOf fails if any integration branch holds content.
func (f *advFix) noIntegrated(path, bad string) {
	f.t.Helper()
	out, _ := exec.Command("git", "-C", f.project, "for-each-ref", "--format=%(refname)", "refs/heads/gosplan/").Output()
	for _, ref := range strings.Fields(string(out)) {
		if !strings.HasSuffix(ref, "/integration") {
			continue
		}
		b, err := exec.Command("git", "-C", f.project, "show", ref+":"+path).Output()
		if err == nil && strings.Contains(string(b), bad) {
			f.t.Fatalf("%s holds %s with %q", ref, path, bad)
		}
	}
}

// dbother: worker 1.a forges 2.a passed with an unverified BAD head.
// Before: complete, exit 0, BAD on the integration branch.
func TestRunE2ETamperOtherTaskPassed(t *testing.T) {
	f := newAdvFix(t, "1.a -> 2.a", "1.a", "2.a")
	res := f.run("ADV_MODE=happy", "ADV_MODE_1_a=dbother")
	f.wantTampered(res, `tasks row 2.a: status db="passed" mem="pending"`)
	f.noIntegrated("fake-2.a.txt", "BAD")
	if strings.Contains(f.log(), " 2.a ") {
		t.Errorf("2.a was launched after the forgery:\n%s", f.log())
	}
}

// dbbase2: worker edits protected tests/guard.txt, then moves its own
// base_sha above the edit. Before: complete, exit 0, the edit integrated.
func TestRunE2ETamperOwnBaseSHA(t *testing.T) {
	f := newAdvFix(t, "1.a -> 2.a", "1.a", "2.a")
	res := f.run("ADV_MODE=happy", "ADV_MODE_1_a=dbbase2")
	f.wantTampered(res, "tasks row 1.a: base_sha db=")
	f.noIntegrated("tests/guard.txt", "tampered")
}

// dbpass: worker sets its own task passed. Before: the run idled forever.
func TestRunE2ETamperOwnStatusPassed(t *testing.T) {
	f := newAdvFix(t, "1.a -> 2.a", "1.a", "2.a")
	res := f.run("ADV_MODE=dbpass")
	f.wantTampered(res, `tasks row 1.a: status db="passed" mem="running"`)
	f.noIntegrated("fake-1.a.txt", "BAD")
}

// conflict: 1.a and 1.b both pass but write different first lines of the
// same new file. Before: complete, exit 0, 1.b silently left out.
func TestRunE2EIntegrationConflictHalts(t *testing.T) {
	f := newAdvFix(t, "(1.a || 1.b)", "1.a", "1.b")
	res := f.run("ADV_MODE=conflict")
	if res.code != 1 || !strings.Contains(res.stderr, "halted (integration_conflict: task 1.b") ||
		!strings.Contains(res.stderr, "tests/shared.txt") {
		t.Fatalf("want exit 1, integration_conflict naming 1.b and tests/shared.txt; got %d\n%s", res.code, res.stderr)
	}
	st := f.store()
	run, _ := st.LatestRun(context.Background())
	tr, err := st.GetTask(context.Background(), run, "1.b")
	if err != nil || tr.Status != "blocked" || !strings.Contains(tr.BlockedReason, "tests/shared.txt") {
		t.Fatalf("1.b = %+v, %v; want blocked naming tests/shared.txt", tr, err)
	}
	rr, _ := st.GetRun(context.Background(), run)
	if rr.Status != "halted" {
		t.Fatalf("run status %s", rr.Status)
	}
	b, err := exec.Command("git", "-C", f.project, "show", rr.IntegrationSHA+":tests/shared.txt").Output()
	if err != nil || strings.TrimSpace(string(b)) != "1.a" {
		t.Fatalf("integration tip %s holds tests/shared.txt %q (%v), want 1.a's", rr.IntegrationSHA, b, err)
	}
}

func (f *advFix) store() *state.Store {
	f.t.Helper()
	s, err := state.OpenReadOnly(filepath.Join(f.state, "gosplan.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { s.Close() })
	return s
}

// setsidPIDs kills (at cleanup) the setsid children the fake logged.
func (f *advFix) killSetsid() {
	for _, l := range strings.Split(f.log(), "\n") {
		var pid int
		if _, err := fmt.Sscanf(l, "setsid %d", &pid); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// A worker leaves a setsid child that holds its stdout. Before: Wait
// blocked on EOF forever and the run never ended. Now the run completes.
// The fake writes its result line first and starts the child after it, so
// the child really holds stdout while the leader exits (with the child
// started before the result line, the test passed on e24295d too).
func TestRunE2ESetsidChildHoldingStdout(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	t.Cleanup(f.killSetsid)
	start := time.Now()
	res := f.run("ADV_MODE=setsidhold")
	if res.code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", res.code, res.stderr)
	}
	if d := time.Since(start); d > time.Minute {
		t.Errorf("run took %v", d)
	}
}

// SIGINT while a worker hangs with a setsid child holding stdout. Before:
// abort waited on that worker forever. Now gosplan run exits (aborted, 2)
// within ExitGrace + AbortWait plus a few seconds for the DB writes.
func TestRunE2ESigintWithSetsidChild(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	t.Cleanup(f.killSetsid)
	cmd := exec.Command(f.gosplan, "run", "--spec", f.spec, "--project", f.project, "--state-dir", f.state,
		"--claude", f.fake, "--gosplan-bin", f.gosplan, "--stall-timeout", "10m", "--mutation", "off", "--usage-probe=false")
	cmd.Dir = f.project
	cmd.Env = append(os.Environ(), "ADV_LOG="+f.logf, "ADV_DB="+filepath.Join(f.state, "gosplan.db"), "SQLITE3="+f.sqlite3, "ADV_MODE=setsidhang")
	var se strings.Builder
	cmd.Stderr = &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(time.Minute)
	for !strings.Contains(f.log(), "setsid ") {
		if time.Now().After(deadline) {
			t.Fatalf("worker never started\n%s", se.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("gosplan run still running 30s after SIGINT\n%s", se.String())
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("SIGINT took %v to end gosplan run", d)
	}
	if code := cmd.ProcessState.ExitCode(); code != 2 || !strings.Contains(se.String(), " aborted") {
		t.Fatalf("exit %d, want 2 (aborted)\n%s", code, se.String())
	}
}

// endedat (re-review finding 2): the worker closes its own attempt row, so
// recording its exit fails on every retry. Before: gosplan run stayed running
// past 200s with no halt. Now it halts state_write_failed within the
// write-failure bounds (10 failures or 60s), exit 1.
func TestRunE2EUnrecordableExitHalts(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	start := time.Now()
	res := f.run("ADV_MODE=endedat")
	if res.code != 1 || !strings.Contains(res.stderr, "halted (state_write_failed: task/1.a: ") ||
		!strings.Contains(res.stderr, "no open attempt") {
		t.Fatalf("want exit 1 and state_write_failed naming task/1.a, got %d\n%s\nadv.log:\n%s", res.code, res.stderr, f.log())
	}
	if d := time.Since(start); d > 70*time.Second {
		t.Errorf("halt took %v, want within the 60s write-fail timeout", d)
	}
}
