// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// run_test.go: RED BY DESIGN. `gosplan run` scheduling, worker launch and
// verdict rules. Every test
// drives the built `gosplan` with the suite's fake claude.
package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const chainSeq = "1.a -> (2.a || 2.b) -> 3.a"

func sortedCopy(s []string) []string {
	c := append([]string{}, s...)
	sort.Strings(c)
	return c
}

// endOf returns the t_ms of the "end" record of task/attempt, or 0.
func (f *runFix) endOf(task, attempt string) int64 {
	if r, ok := f.find("end", task, attempt); ok {
		return r.TMS
	}
	return 0
}

// lastEnd returns the latest "end" t_ms of task over all attempts.
func (f *runFix) lastEnd(task string) int64 {
	var m int64
	for _, r := range f.ledger() {
		if r.Event == "end" && r.Task == task && r.TMS > m {
			m = r.TMS
		}
	}
	return m
}

func argvHas(argv []string, flag, val string) bool {
	for i, a := range argv {
		if a == flag && (val == "" || (i+1 < len(argv) && argv[i+1] == val)) {
			return true
		}
	}
	return false
}

// TestRunSequenceE2E: a three-tier spec runs end to end.
// Clause: orchestrator#DY-RUN-E2E
// Clause: orchestrator#DY-RUN-EXIT
func TestRunSequenceE2E(t *testing.T) {
	f := newRunFix(t, chainSeq, nil)
	res := f.runNR()
	mustExit(t, "gosplan run", res, 0)
	r, _ := f.status()
	if r.Status != "complete" {
		t.Fatalf("run status: want complete, got %q", r.Status)
	}
	if len(r.Tasks) != 4 {
		t.Fatalf("want 4 tasks in report, got %d", len(r.Tasks))
	}
	for _, x := range r.Tasks {
		if x.Status != "passed" {
			t.Fatalf("task %s: want passed, got %s", x.ID, x.Status)
		}
	}
	if r.BaseSHA != f.masterSHA {
		t.Fatalf("base_sha: want %s, got %s", f.masterSHA, r.BaseSHA)
	}
}

// TestRunTierDerivedEdges: without `deps` keys a task depends on every task
// of the previous tier; --dry-run prints them and touches nothing.
// Clause: orchestrator#DY-SEQ-TIERS
func TestRunTierDerivedEdges(t *testing.T) {
	f := newRunFix(t, "# a comment line\n\n1.a -> ( 2.a || 2.b ) -> 3.a", nil)
	dry := f.runNR("--dry-run")
	mustExit(t, "gosplan run --dry-run", dry, 0)
	if !strings.Contains(dry.stdout, "3.a") {
		t.Fatalf("dry-run output does not list task 3.a:\n%s", dry)
	}
	if _, err := os.Stat(f.db()); err == nil {
		t.Fatalf("--dry-run created %s; it must touch nothing", f.db())
	}
	mustExit(t, "gosplan run", f.runNR(), 0)
	r, _ := f.status()
	want := map[string]struct {
		tier int
		deps string
	}{"1.a": {1, ""}, "2.a": {2, "1.a"}, "2.b": {2, "1.a"}, "3.a": {3, "2.a,2.b"}}
	for id, w := range want {
		x := r.task(t, id)
		if x.Tier != w.tier || strings.Join(sortedCopy(x.Deps), ",") != w.deps || x.DepsSource != "tiers" {
			t.Fatalf("task %s: want tier %d deps [%s] source tiers, got tier %d deps %v source %q", id, w.tier, w.deps, x.Tier, x.Deps, x.DepsSource)
		}
	}
}

// TestRunSpecValidation: an invalid spec is refused and nothing is written.
// Clause: orchestrator#DY-SEQ-VALIDATE
func TestRunSpecValidation(t *testing.T) {
	ctl := newRunFix(t, chainSeq, nil)
	mustExit(t, "control: valid spec gosplan run --dry-run", ctl.runNR("--dry-run"), 0)

	cases := []struct {
		name string
		seq  string
		deps map[string][]string
		edit func(f *runFix)
	}{
		{"unknown-dep", "1.a -> 2.a", map[string][]string{"2.a": {"9.z"}}, nil},
		{"self-dep", "1.a -> 2.a", map[string][]string{"2.a": {"2.a"}}, nil},
		{"same-tier-dep", "1.a -> (2.a || 2.b)", map[string][]string{"2.b": {"2.a"}}, nil},
		{"duplicate-id", "1.a -> 1.a", nil, nil},
		{"missing-prompt", "1.a -> 2.a", nil, func(f *runFix) { _ = os.Remove(filepath.Join(f.spec, "prompts", "2.a.md")) }},
		{"malformed-sidecar", "1.a -> 2.a", nil, func(f *runFix) { writeFile(t, f.spec, "prompts/2.a.task.toml", "scope = [unterminated\n") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRunFix(t, c.seq, c.deps)
			if c.edit != nil {
				c.edit(f)
			}
			res := f.runNR()
			if res.code != 2 {
				t.Fatalf("invalid spec (%s): want exit 2, got %s", c.name, res)
			}
			if _, err := os.Stat(f.db()); err == nil {
				if n := sql(t, f.db(), "SELECT count(*) FROM runs"); n != "0" {
					t.Fatalf("invalid spec (%s) wrote %s run rows", c.name, n)
				}
			}
			if len(f.starts("")) != 0 {
				t.Fatalf("invalid spec (%s) launched a worker", c.name)
			}
		})
	}
}

// TestRunExplicitDepsReplaceTiers: a sidecar `deps` key replaces the
// tier-derived edges: 2.b (deps = []) runs while tier-1 1.a is still working.
// Clause: orchestrator#DY-DEPS-EXPLICIT
func TestRunExplicitDepsReplaceTiers(t *testing.T) {
	f := newRunFix(t, chainSeq, map[string][]string{"2.b": {}, "3.a": {"2.a"}})
	f.scenario(map[string][]map[string]any{"1.a": {{"op": "sleep", "seconds": 4, "heartbeat": true}, {"op": "commit"}}})
	mustExit(t, "gosplan run", f.runNR(), 0)
	r, _ := f.status()
	if x := r.task(t, "2.b"); x.DepsSource != "sidecar" || len(x.Deps) != 0 {
		t.Fatalf("2.b: want sidecar deps [], got %v source %q", x.Deps, x.DepsSource)
	}
	if x := r.task(t, "3.a"); x.DepsSource != "sidecar" || strings.Join(x.Deps, ",") != "2.a" {
		t.Fatalf("3.a: want sidecar deps [2.a], got %v source %q", x.Deps, x.DepsSource)
	}
	s2b := f.starts("2.b")
	end1a := f.endOf("1.a", "1")
	if len(s2b) == 0 || end1a == 0 || s2b[0].TMS >= end1a {
		t.Fatalf("2.b (deps = []) must start before 1.a ends: 2.b starts %v, 1.a end %d", s2b, end1a)
	}
}

// TestRunNoTierBarrier: a failing task blocks only its descendants; an
// unrelated later-tier task starts while the failing one is still retrying.
// Clause: orchestrator#DY-NO-BARRIER
func TestRunNoTierBarrier(t *testing.T) {
	f := newRunFix(t, "1.a -> (2.a || 2.b) -> (3.a || 3.b)", map[string][]string{"3.a": {"2.a"}, "3.b": {"2.b"}})
	bad := []map[string]any{{"op": "sleep", "seconds": 4, "heartbeat": true}, {"op": "commit", "content": "BAD\n"}}
	f.scenario(map[string][]map[string]any{"2.b": bad})
	res := f.runNR()
	mustExit(t, "gosplan run (2.b always fails)", res, 1)
	r, _ := f.status()
	for id, want := range map[string]string{"1.a": "passed", "2.a": "passed", "3.a": "passed", "2.b": "failed", "3.b": "abandoned"} {
		if got := r.task(t, id).Status; got != want {
			t.Fatalf("task %s: want %s, got %s", id, want, got)
		}
	}
	s3a := f.starts("3.a")
	end2b := f.endOf("2.b", "2")
	if len(s3a) == 0 || end2b == 0 || s3a[0].TMS >= end2b {
		t.Fatalf("3.a must start before 2.b's last attempt ends (no tier barrier): 3.a starts %v, 2.b attempt 2 end %d", s3a, end2b)
	}
}

// TestRunAbandonsDescendants: a failed task's descendants never start and end
// abandoned; the run is halted (exit 1).
// Clause: orchestrator#DY-ABANDON
// Clause: orchestrator#DY-RUN-EXIT
func TestRunAbandonsDescendants(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a -> 3.a", nil)
	f.scenario(map[string][]map[string]any{"2.a": {{"op": "commit", "content": "BAD\n"}}})
	mustExit(t, "gosplan run (2.a fails)", f.runNR(), 1)
	r, _ := f.status()
	if r.Status != "halted" {
		t.Fatalf("run status: want halted, got %q", r.Status)
	}
	if s := r.task(t, "2.a").Status; s != "failed" {
		t.Fatalf("2.a: want failed, got %s", s)
	}
	if s := r.task(t, "3.a").Status; s != "abandoned" {
		t.Fatalf("3.a: want abandoned, got %s", s)
	}
	if n := len(f.starts("3.a")); n != 0 {
		t.Fatalf("3.a has a failed ancestor and must never start; started %d times", n)
	}
}

// TestRunDependencyOrder: every task starts after each of its deps finished.
// Clause: orchestrator#DY-DEP-ORDER
func TestRunDependencyOrder(t *testing.T) {
	f := newRunFix(t, chainSeq, nil)
	slow := []map[string]any{{"op": "sleep", "seconds": 2, "heartbeat": true}, {"op": "commit"}}
	f.scenario(map[string][]map[string]any{"1.a": slow, "2.a": slow})
	mustExit(t, "gosplan run", f.runNR(), 0)
	deps := map[string][]string{"2.a": {"1.a"}, "2.b": {"1.a"}, "3.a": {"2.a", "2.b"}}
	for task, ds := range deps {
		st := f.starts(task)
		if len(st) == 0 {
			t.Fatalf("task %s never started", task)
		}
		for _, d := range ds {
			if e := f.lastEnd(d); e == 0 || st[0].TMS < e {
				t.Fatalf("task %s started at %d before dep %s ended at %d", task, st[0].TMS, d, e)
			}
		}
	}
}

// TestRunNeverTwiceAtOnce: across retries, no task ever has two workers alive.
// Clause: orchestrator#DY-READY-ONCE
func TestRunNeverTwiceAtOnce(t *testing.T) {
	f := newRunFix(t, "1.a || 1.b || 1.c", nil)
	fail := []map[string]any{{"op": "sleep", "seconds": 2, "heartbeat": true}, {"op": "exit", "code": 1}}
	f.scenario(map[string][]map[string]any{"1.a.1": fail, "1.b.1": fail, "1.c.1": fail})
	mustExit(t, "gosplan run", f.runNR("--max-attempts", "3"), 0)
	for _, rec := range f.ledger() {
		if rec.Event == "overlap" {
			t.Fatalf("task %s attempt %s started while another worker for it was alive", rec.Task, rec.Attempt)
		}
	}
	r, _ := f.status()
	for _, id := range []string{"1.a", "1.b", "1.c"} {
		seen := map[string]bool{}
		for _, s := range f.starts(id) {
			if seen[s.Attempt] {
				t.Fatalf("task %s attempt %s launched twice", id, s.Attempt)
			}
			seen[s.Attempt] = true
		}
		if x := r.task(t, id); len(x.Attempts) != len(seen) || len(seen) != 2 {
			t.Fatalf("task %s: want 2 attempts (fail, pass), report has %d, ledger %d", id, len(x.Attempts), len(seen))
		}
	}
}

// TestRunMaxParallel: never more than --max-parallel workers at once, and the
// limit is actually reached.
// Clause: orchestrator#DY-CAPACITY
func TestRunMaxParallel(t *testing.T) {
	f := newRunFix(t, "1.a || 1.b || 1.c || 1.d || 1.e", nil)
	slow := []map[string]any{{"op": "sleep", "seconds": 3, "heartbeat": true}, {"op": "commit"}}
	f.scenario(map[string][]map[string]any{"1.a": slow, "1.b": slow, "1.c": slow, "1.d": slow, "1.e": slow})
	mustExit(t, "gosplan run --max-parallel 2", f.runNR("--max-parallel", "2"), 0)
	maxLive := 0
	for _, s := range f.starts("") {
		if s.Live > maxLive {
			maxLive = s.Live
		}
	}
	if maxLive > 2 {
		t.Fatalf("saw %d workers alive at once with --max-parallel 2", maxLive)
	}
	if maxLive < 2 {
		t.Fatalf("five independent tasks never ran 2 at once (max live %d)", maxLive)
	}
}

// TestRunUsesWorkqueue: dispatch is built on client-go's workqueue.
// Clause: orchestrator#DY-WORKQUEUE
func TestRunUsesWorkqueue(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/gosplan")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/gosplan: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m)^k8s\.io/client-go/util/workqueue$`).Match(out) {
		t.Fatalf("gosplan does not link k8s.io/client-go/util/workqueue")
	}
}

// TestRunRetryModelAndSession: attempt 1 runs on --model (default sonnet),
// the retry on --retry-model (default opus), each with a fresh session id.
// Clause: orchestrator#DY-RETRY-MODEL
func TestRunRetryModelAndSession(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a.1": {{"op": "exit", "code": 1}}})
	mustExit(t, "gosplan run", f.runNR(), 0)
	st := f.starts("1.a")
	if len(st) != 2 {
		t.Fatalf("want 2 launches of 1.a, got %d", len(st))
	}
	if st[0].Model != "sonnet" || st[1].Model != "opus" {
		t.Fatalf("models: want sonnet then opus, got %q then %q", st[0].Model, st[1].Model)
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !uuid.MatchString(st[0].SessionID) || !uuid.MatchString(st[1].SessionID) || st[0].SessionID == st[1].SessionID {
		t.Fatalf("session ids must be distinct UUIDs: %q, %q", st[0].SessionID, st[1].SessionID)
	}
}

// TestRunWorkerArgv: the worker gets the documented flags.
// Clause: orchestrator#DY-ARGV
func TestRunWorkerArgv(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	mustExit(t, "gosplan run --max-turns 7", f.runNR("--max-turns", "7"), 0)
	st := f.starts("1.a")
	if len(st) == 0 {
		t.Fatalf("1.a never started")
	}
	a := st[0].Argv
	for _, w := range [][2]string{{"-p", ""}, {"--output-format", "stream-json"}, {"--verbose", ""},
		{"--permission-mode", "bypassPermissions"}, {"--session-id", ""}, {"--model", ""}, {"--max-turns", "7"}} {
		if !argvHas(a, w[0], w[1]) {
			t.Fatalf("worker argv lacks %s %s: %v", w[0], w[1], a)
		}
	}
	if st[0].Prompt == "" {
		t.Fatalf("worker prompt is empty")
	}
}

// TestRunWorkerEnv: the worker env drops GOSPLAN_ATTEST* / NIGHTRUNNER_* and has
// GOSPLAN_RUN / GOSPLAN_TASK / GOSPLAN_ATTEMPT; the key path is in neither argv nor prompt.
// Clause: orchestrator#DY-ENV
func TestRunWorkerEnv(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a": {{"op": "write_env"}, {"op": "commit"}}})
	key := filepath.Join(f.state, "no-such-key")
	env := append(f.env(), "GOSPLAN_ATTEST_KEY="+key, "GOSPLAN_ATTEST_OTHER=x", "NIGHTRUNNER_X=1")
	res := run(t, 5*time.Minute, f.project, env, append([]string{nrBin}, f.args()...)...)
	mustExit(t, "gosplan run", res, 0)
	b, err := os.ReadFile(filepath.Join(f.fake, "env-1.a-1.txt"))
	if err != nil {
		t.Fatalf("worker env file missing: %v", err)
	}
	envs := "\n" + string(b)
	for _, bad := range []string{"\nGOSPLAN_ATTEST_KEY=", "\nGOSPLAN_ATTEST_OTHER=", "\nNIGHTRUNNER_X="} {
		if strings.Contains(envs, bad) {
			t.Fatalf("worker env leaks %s", strings.TrimSpace(bad))
		}
	}
	run := f.onlyRun()
	for _, good := range []string{"\nGOSPLAN_RUN=" + run + "\n", "\nGOSPLAN_TASK=1.a\n", "\nGOSPLAN_ATTEMPT=1\n"} {
		if !strings.Contains(envs, good) {
			t.Fatalf("worker env lacks %s", strings.TrimSpace(good))
		}
	}
	for _, s := range f.starts("1.a") {
		if strings.Contains(strings.Join(s.Argv, " "), key) || strings.Contains(s.Prompt, key) {
			t.Fatalf("attest key path reaches the worker argv or prompt")
		}
	}
}

// TestRunVerdictOnlyFromVerifier: DONE.md / VERDICT files claiming success
// do not matter; the committed tree fails the verifier, so the task fails.
// Clause: orchestrator#DY-VERDICT-SOURCE
func TestRunVerdictOnlyFromVerifier(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a": {
		{"op": "write", "file": "DONE.md", "content": "## Summary\nPASS\n"},
		{"op": "write", "file": "VERDICT", "content": "pass\n"},
		{"op": "commit", "content": "BAD\n"},
		{"op": "result", "text": "All tests pass. VERDICT: PASS"},
	}})
	mustExit(t, "gosplan run (worker claims pass, tree fails)", f.runNR("--max-attempts", "1"), 1)
	r, _ := f.status()
	x := r.task(t, "1.a")
	if x.Status != "failed" {
		t.Fatalf("1.a: want failed, got %s", x.Status)
	}
	if len(x.Attempts) == 0 || x.Attempts[0].Verdict == nil || x.Attempts[0].Verdict.Result != "fail" {
		t.Fatalf("1.a attempt 1: want a fail verdict from gosplan verify, got %+v", x.Attempts)
	}
}

// TestRunGosplanBlocked: an GOSPLAN_BLOCKED line in the result routes the task to
// blocked without verifying; its descendant is abandoned.
// Clause: orchestrator#DY-NR-BLOCKED
func TestRunGosplanBlocked(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a", nil)
	f.scenario(map[string][]map[string]any{"1.a": {{"op": "result", "text": "I stopped.\nGOSPLAN_BLOCKED: cannot do this honestly"}}})
	mustExit(t, "gosplan run (1.a blocked)", f.runNR(), 1)
	r, _ := f.status()
	x := r.task(t, "1.a")
	if x.Status != "blocked" || !strings.Contains(x.BlockedReason, "cannot do this honestly") {
		t.Fatalf("1.a: want blocked with the GOSPLAN_BLOCKED reason, got %s %q", x.Status, x.BlockedReason)
	}
	for _, a := range x.Attempts {
		if a.Verdict != nil {
			t.Fatalf("blocked task must not be verified, got verdict %+v", a.Verdict)
		}
	}
	m, _ := filepath.Glob(filepath.Join(f.state, "runs", r.Run, "verify", "1.a.*"))
	if len(m) != 0 {
		t.Fatalf("blocked task has verify output: %v", m)
	}
	if s := r.task(t, "2.a").Status; s != "abandoned" {
		t.Fatalf("2.a: want abandoned, got %s", s)
	}
}

// TestRunIntegrationBranch: passed heads land on gosplan/<run>/integration;
// master and the checked-out branch are untouched.
// Clause: orchestrator#DY-INTEGRATION
func TestRunIntegrationBranch(t *testing.T) {
	f := newRunFix(t, chainSeq, nil)
	mustExit(t, "gosplan run", f.runNR(), 0)
	r, _ := f.status()
	br := "gosplan/" + r.Run + "/integration"
	if r.IntegrationBranch != br {
		t.Fatalf("integration_branch: want %s, got %q", br, r.IntegrationBranch)
	}
	tip := gitc(t, f.project, "rev-parse", "refs/heads/"+br)
	if tip != r.IntegrationSHA {
		t.Fatalf("integration_sha %q != branch tip %s", r.IntegrationSHA, tip)
	}
	for _, id := range []string{"1.a", "2.a", "2.b", "3.a"} {
		gitc(t, f.project, "cat-file", "-e", tip+":fake-"+id+".txt")
	}
	if got := gitc(t, f.project, "rev-parse", "master"); got != f.masterSHA {
		t.Fatalf("master moved: %s -> %s", f.masterSHA, got)
	}
	if b := gitc(t, f.project, "symbolic-ref", "--short", "HEAD"); b != "master" {
		t.Fatalf("project checkout changed to %s", b)
	}
}

// TestRunEventsOutbox: the run's events.jsonl is valid, has no duplicate
// outbox event, and replays to state.json.
// Clause: orchestrator#DY-OUTBOX
func TestRunEventsOutbox(t *testing.T) {
	f := newRunFix(t, chainSeq, nil)
	mustExit(t, "gosplan run", f.runNR(), 0)
	run := f.onlyRun()
	dir := filepath.Join(f.state, "runs", run)
	logPath := filepath.Join(dir, "events.jsonl")
	evs := readEvents(t, logPath)
	types := map[string]int{}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) || ev.V != 1 {
			t.Fatalf("event %d: seq %d v %d", i+1, ev.Seq, ev.V)
		}
		types[ev.Type]++
	}
	if types["run_started"] != 1 || types["run_finished"] != 1 || types["task_state"] < 4 || types["verdict"] < 4 {
		t.Fatalf("event type counts look wrong: %v", types)
	}
	if res := gosplan(t, dir, nil, "replay", "--log", logPath, "--state", filepath.Join(dir, "state.json"), "--check"); res.code != 0 {
		t.Fatalf("gosplan replay --check on the run: want exit 0, got %s", res)
	}
	eids := map[string]bool{}
	for _, ev := range evs {
		m := regexp.MustCompile(`"eid":\s*([0-9]+)`).FindSubmatch(ev.Data)
		if m == nil {
			continue
		}
		if eids[string(m[1])] {
			t.Fatalf("outbox event eid %s appended twice", m[1])
		}
		eids[string(m[1])] = true
	}
	if len(eids) == 0 {
		t.Fatalf("no orchestrator event carries an eid")
	}
}
