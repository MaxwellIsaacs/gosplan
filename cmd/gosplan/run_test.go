package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/attest"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/report"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// runBuilt holds the binaries built once for the `gosplan run` tests.
type runBuilt struct{ gosplan, fake string }

// runBins builds this gosplan and the conformance suite's fake claude once per
// test binary.
var runBins = sync.OnceValues(func() (runBuilt, error) {
	dir, err := os.MkdirTemp("", "gosplan-run-test-bin-*")
	if err != nil {
		return runBuilt{}, err
	}
	b := runBuilt{gosplan: filepath.Join(dir, "gosplan"), fake: filepath.Join(dir, "fakeclaude")}
	for _, x := range [][2]string{{b.gosplan, "."}, {b.fake, "../../conformance/fakeclaude"}} {
		cmd := exec.Command("go", "build", "-o", x[0], x[1])
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return runBuilt{}, &runBuildError{x[1], err, out}
		}
	}
	return b, nil
})

type runBuildError struct {
	pkg string
	err error
	out []byte
}

func (e *runBuildError) Error() string {
	return "build " + e.pkg + ": " + e.err.Error() + "\n" + string(e.out)
}

func runBinaries(t *testing.T) runBuilt {
	t.Helper()
	b, err := runBins()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runProject builds the fixture project: a repo on master with a committed
// nightrunner.toml whose test gate needs fake-*.txt without BAD in it.
func runProject(t *testing.T) (dir, master string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-q", "-b", "master")
	write(t, dir, "nightrunner.toml", "[verify]\ntest = \"sh check.sh\"\n")
	write(t, dir, "check.sh", "ls fake-*.txt >/dev/null 2>&1 && ! grep -q BAD fake-*.txt\n")
	write(t, dir, "README", "fixture\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	return dir, git(t, dir, "rev-parse", "master")
}

// runCopySpec copies testdata/<name> into a temp dir (tests may edit it).
func runCopySpec(t *testing.T, name string) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("testdata", name)
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		write(t, dst, rel, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// runFakeGremlins writes a gremlins stand-in: canned JSON to the -o path, exit 10.
func runFakeGremlins(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gremlins")
	script := `#!/bin/sh
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
cat > "$out" <<'EOF'
{"go_module":"fixture","test_efficacy":0,"mutations_coverage":100,"mutants_total":6,"mutants_killed":0,"mutants_lived":6,"mutants_not_viable":0,"mutants_not_covered":0,"elapsed_time":1,"files":[{"file_name":"a.go","mutations":[{"type":"CONDITIONALS_NEGATION","status":"LIVED","line":1,"column":1}]}]}
EOF
exit 10
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// runKey generates a temp ed25519 key outside any repo.
func runKey(t *testing.T) string {
	t.Helper()
	k := filepath.Join(t.TempDir(), "ed25519.key")
	if err := attest.GenerateKey(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// runScenario adds task's steps to <dir>/scenario.json for the conformance
// fake: attempt "*" is every attempt of the task, "1" only attempt 1.
func runScenario(t *testing.T, dir, task string, attempts map[string][]map[string]any) {
	t.Helper()
	sc := map[string][]map[string]any{}
	if b, err := os.ReadFile(filepath.Join(dir, "scenario.json")); err == nil {
		if err := json.Unmarshal(b, &sc); err != nil {
			t.Fatal(err)
		}
	}
	for k, steps := range attempts {
		key := task
		if k != "*" {
			key += "." + k
		}
		sc[key] = steps
	}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "scenario.json", string(b))
}

// runCommitSteps are the default fake steps: commit fake-<task>.txt with content.
func runCommitSteps(task, content string) []map[string]any {
	return []map[string]any{{"op": "commit", "file": "fake-" + task + ".txt", "content": content}}
}

// runUsageText is the recorded /usage reply the fake prints.
const runUsageText = "You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 43% used · resets Oct 27, 10:20pm (America/New_York)\nCurrent week (all models): 11% used · resets Oct 31, 5pm (America/New_York)\n"

// runFix is one spec + project + state dir + scenario dir.
type runFix struct {
	t                                 *testing.T
	bins                              runBuilt
	spec, project, master, state, scn string
	key                               string
}

func runNewFix(t *testing.T, specName string) *runFix {
	t.Helper()
	f := &runFix{t: t, bins: runBinaries(t), spec: runCopySpec(t, specName), state: t.TempDir(), scn: t.TempDir(), key: runKey(t)}
	f.project, f.master = runProject(t)
	usage, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"num_turns": 0, "local_command": "usage", "result": runUsageText})
	write(t, f.scn, "usage.json", string(usage))
	return f
}

// args is the standard invocation from TASK.md plus extra (later flags win).
func (f *runFix) args(extra ...string) []string {
	a := []string{"run", "--spec", f.spec, "--project", f.project, "--state-dir", f.state,
		"--stall-timeout", "2s", "--retry-base", "100ms", "--max-attempts", "2", "--usage-probe=true",
		"--claude", f.bins.fake, "--gosplan-bin", f.bins.gosplan, "--gremlins", runFakeGremlins(f.t),
		"--attest-key", f.key}
	return append(a, extra...)
}

type runResult struct {
	code           int
	stdout, stderr string
}

// exec runs the built gosplan with env added to the test's environment.
func (f *runFix) exec(env []string, args ...string) runResult {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bins.gosplan, args...)
	cmd.Dir = f.project
	cmd.Env = append(append(os.Environ(), "CONF_FAKE_DIR="+f.scn), env...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	_ = cmd.Run()
	if ctx.Err() != nil {
		f.t.Fatalf("gosplan %v timed out\nstderr:\n%s", args, se.String())
	}
	return runResult{cmd.ProcessState.ExitCode(), so.String(), se.String()}
}

func (f *runFix) run(extra ...string) runResult {
	f.t.Helper()
	return f.exec(nil, f.args(extra...)...)
}

func (f *runFix) mustExit(res runResult, want int) {
	f.t.Helper()
	if res.code != want {
		f.t.Fatalf("gosplan run: want exit %d, got %d\nstdout:\n%s\nstderr:\n%s", want, res.code, res.stdout, res.stderr)
	}
}

func (f *runFix) store() *state.Store {
	f.t.Helper()
	s, err := state.OpenReadOnly(filepath.Join(f.state, "gosplan.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { s.Close() })
	return s
}

func (f *runFix) runID() string {
	f.t.Helper()
	id, err := f.store().LatestRun(context.Background())
	if err != nil || id == "" {
		f.t.Fatalf("no run in state: %v", err)
	}
	return id
}

func (f *runFix) runDir() string { return filepath.Join(f.state, "runs", f.runID()) }

// report reads <run>/report.json as written by gosplan run.
func (f *runFix) report() report.Report {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.runDir(), "report.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func runTask(t *testing.T, r report.Report, id string) report.Task {
	t.Helper()
	for _, x := range r.Tasks {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("report has no task %s", id)
	return report.Task{}
}

func (f *runFix) attempts(task string) []state.AttemptRow {
	f.t.Helper()
	a, err := f.store().ListAttempts(context.Background(), f.runID(), task)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestRunE2EComplete(t *testing.T) {
	f := runNewFix(t, "e2e-spec")
	rl, err := os.ReadFile(filepath.Join("..", "..", "internal", "streamjson", "testdata", "rate_limit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := json.RawMessage(strings.SplitN(strings.TrimSpace(string(rl)), "\n", 2)[0])
	steps := append([]map[string]any{{"op": "emit", "line": string(line)}}, runCommitSteps("1.a", "ok\n")...)
	runScenario(t, f.scn, "1.a", map[string][]map[string]any{"*": steps})
	runScenario(t, f.scn, "2.a", map[string][]map[string]any{"1": {{"op": "hang"}}, "*": runCommitSteps("2.a", "ok\n")})

	f.mustExit(f.run(), 0)
	r := f.report()
	if r.Schema != "gosplan.run-report/v1" || r.Status != "complete" {
		t.Fatalf("report: schema %q status %q", r.Schema, r.Status)
	}
	pub := f.key + ".pub"
	for _, x := range r.Tasks {
		if x.Status != "passed" {
			t.Fatalf("task %s: want passed, got %s", x.ID, x.Status)
		}
		for _, a := range x.Attempts {
			if a.Verdict == nil || a.Verdict.Result != "pass" {
				continue
			}
			if a.AttestationPath == "" {
				t.Fatalf("task %s attempt %d: passed verdict not signed", x.ID, a.N)
			}
			code, out := attestCapture(t, func() int {
				return cmdVerifyAttestation([]string{"--envelope", a.AttestationPath, "--pubkey", pub, "--tree", a.Verdict.TreeSHA})
			})
			if code != 0 {
				t.Fatalf("task %s attempt %d: verify-attestation exit %d: %s", x.ID, a.N, code, out)
			}
			if a.Mutation == nil || a.Mutation["status"] != "below_threshold" || a.Mutation["advisory"] != true {
				t.Fatalf("task %s attempt %d: want advisory below_threshold mutation, got %v", x.ID, a.N, a.Mutation)
			}
		}
	}
	var outcomes []string
	for _, a := range runTask(t, r, "2.a").Attempts {
		outcomes = append(outcomes, a.Outcome)
	}
	if strings.Join(outcomes, ",") != "stalled,exited" {
		t.Fatalf("2.a outcomes: want stalled,exited, got %v", outcomes)
	}

	// Windows: every row, straight from the store.
	rows, err := f.store().DB().Query(`SELECT window, source, documented FROM window_observations`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var w, src string
		var doc int
		if err := rows.Scan(&w, &src, &doc); err != nil {
			t.Fatal(err)
		}
		seen[w+"|"+src+"|"+map[int]string{0: "undoc", 1: "doc"}[doc]] = true
	}
	rows.Close()
	for _, want := range []string{
		"five_hour|rate_limit_event.unifiedWindows|undoc",
		"seven_day|rate_limit_event.unifiedWindows|undoc",
		"session|usage_text|undoc",
	} {
		if !seen[want] {
			t.Fatalf("window_observations lack %s; have %v", want, seen)
		}
	}
	reported := map[string]report.Window{}
	for _, w := range r.Windows {
		reported[w.Window] = w
	}
	if w, ok := reported["seven_day"]; !ok || w.Documented || w.Source != "rate_limit_event.unifiedWindows" {
		t.Fatalf("report window seven_day: %+v", w)
	}

	dir := f.runDir()
	logPath := filepath.Join(dir, "events.jsonl")
	evs, err := events.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Validate(evs); err != nil {
		t.Fatalf("events.jsonl does not validate: %v", err)
	}
	code, out := attestCapture(t, func() int {
		return cmdReplay([]string{"--log", logPath, "--state", filepath.Join(dir, "state.json"), "--check"})
	})
	if code != 0 {
		t.Fatalf("gosplan replay --check: exit %d: %s", code, out)
	}

	br := "gosplan/" + r.Run + "/integration"
	if r.IntegrationBranch != br {
		t.Fatalf("integration branch: want %s, got %q", br, r.IntegrationBranch)
	}
	tip := git(t, f.project, "rev-parse", "refs/heads/"+br)
	if tip != r.IntegrationSHA {
		t.Fatalf("integration sha %q != branch tip %s", r.IntegrationSHA, tip)
	}
	for _, id := range []string{"1.a", "2.a", "2.b", "2.c", "3.a"} {
		git(t, f.project, "cat-file", "-e", tip+":fake-"+id+".txt")
	}
	if got := git(t, f.project, "rev-parse", "master"); got != f.master {
		t.Fatalf("master moved: %s -> %s", f.master, got)
	}
}

func TestRunE2EHaltedNoBarrier(t *testing.T) {
	f := runNewFix(t, "e2e-spec")
	slow := append([]map[string]any{{"op": "sleep", "seconds": 2, "heartbeat": true}}, runCommitSteps("1.a", "ok\n")...)
	runScenario(t, f.scn, "1.a", map[string][]map[string]any{"*": slow})
	runScenario(t, f.scn, "2.b", map[string][]map[string]any{"*": runCommitSteps("2.b", "BAD\n")})

	f.mustExit(f.run("--mutation", "off"), 1)
	r := f.report()
	if r.Status != "halted" {
		t.Fatalf("run status: want halted, got %s", r.Status)
	}
	for id, want := range map[string]string{"1.a": "passed", "2.a": "passed", "2.c": "passed", "2.b": "failed", "3.a": "abandoned"} {
		if got := runTask(t, r, id).Status; got != want {
			t.Fatalf("task %s: want %s, got %s", id, want, got)
		}
	}
	if n := len(runTask(t, r, "2.b").Attempts); n != 2 {
		t.Fatalf("2.b: want 2 attempts, got %d", n)
	}
	if n := len(runTask(t, r, "3.a").Attempts); n != 0 {
		t.Fatalf("3.a has a failed dep and must never launch; got %d attempts", n)
	}
	c := f.attempts("2.c")
	b := f.attempts("2.b")
	a := f.attempts("1.a")
	if len(c) == 0 || len(b) != 2 || len(a) == 0 {
		t.Fatalf("attempt rows: 2.c %d, 2.b %d, 1.a %d", len(c), len(b), len(a))
	}
	// The retry starts from the task base: the worker's identical commit
	// succeeds again, so attempt 2 exits 0 and fails only in gosplan verify.
	if b[1].ExitCode == nil || *b[1].ExitCode != 0 || b[1].VerdictResult != "fail" {
		t.Fatalf("2.b attempt 2: want exit 0 and a fail verdict from a fresh worktree, got exit %v verdict %q", b[1].ExitCode, b[1].VerdictResult)
	}
	if !c[0].StartedAt.Before(b[1].EndedAt) {
		t.Fatalf("2.c started %v, not before 2.b's last attempt ended %v (tier barrier?)", c[0].StartedAt, b[1].EndedAt)
	}
	// 2.c has deps = []: it does not wait for tier 1 either.
	if !c[0].StartedAt.Before(a[0].EndedAt) {
		t.Fatalf("2.c (deps = []) started %v, after 1.a ended %v", c[0].StartedAt, a[0].EndedAt)
	}
}

func TestRunE2EBlockedRequest(t *testing.T) {
	f := runNewFix(t, "e2e-spec")
	runScenario(t, f.scn, "2.a", map[string][]map[string]any{"*": {
		{"op": "result", "text": "I stopped here.\nGOSPLAN_BLOCKED: cannot"},
	}})
	f.mustExit(f.run("--mutation", "off"), 1)
	r := f.report()
	x := runTask(t, r, "2.a")
	if x.Status != "blocked" || !strings.Contains(x.BlockedReason, "cannot") {
		t.Fatalf("2.a: want blocked with the GOSPLAN_BLOCKED reason, got %s %q", x.Status, x.BlockedReason)
	}
	if m, _ := filepath.Glob(filepath.Join(f.runDir(), "verify", "2.a.*")); len(m) != 0 {
		t.Fatalf("blocked task was verified: %v", m)
	}
	if s := runTask(t, r, "3.a").Status; s != "abandoned" {
		t.Fatalf("3.a: want abandoned, got %s", s)
	}
}

func TestRunWorkerEnvHasNoAttestKey(t *testing.T) {
	f := runNewFix(t, "e2e-spec")
	envFile := filepath.Join(f.scn, "env-1.a-1.txt") // where the fake's write_env puts it
	steps := append([]map[string]any{{"op": "write_env"}}, runCommitSteps("1.a", "ok\n")...)
	runScenario(t, f.scn, "1.a", map[string][]map[string]any{"*": steps})
	res := f.exec([]string{"GOSPLAN_ATTEST_KEY=" + f.key, "NIGHTRUNNER_X=1"}, f.args("--mutation", "off", "--usage-probe=false")...)
	f.mustExit(res, 0)

	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("worker env file: %v", err)
	}
	env := "\n" + string(b)
	for _, bad := range []string{"\nGOSPLAN_ATTEST_KEY=", "\nNIGHTRUNNER_X="} {
		if strings.Contains(env, bad) {
			t.Fatalf("worker env leaks %s", strings.TrimSpace(bad))
		}
	}
	run := f.runID()
	for _, good := range []string{"\nGOSPLAN_RUN=" + run + "\n", "\nGOSPLAN_TASK=1.a\n", "\nGOSPLAN_ATTEMPT=1\n"} {
		if !strings.Contains(env, good) {
			t.Fatalf("worker env lacks %s", strings.TrimSpace(good))
		}
	}

	dir := f.runDir()
	ab, err := os.ReadFile(filepath.Join(dir, "attempts", "1.a.1.argv.json"))
	if err != nil {
		t.Fatalf("recorded argv: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(ab, &argv); err != nil {
		t.Fatal(err)
	}
	if len(argv) < 3 || argv[0] != f.bins.fake || argv[1] != "-p" || argv[2] == "" {
		t.Fatalf("recorded argv is not claude -p <prompt>: %q", argv)
	}
	if strings.Contains(string(ab), f.key) {
		t.Fatalf("attest key path is in the worker argv or prompt: %s", ab)
	}
	sb, err := os.ReadFile(filepath.Join(dir, "attempts", "1.a.1.stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var init struct {
		Type, Subtype string
		SessionID     string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(string(sb), "\n", 2)[0]), &init); err != nil || init.Type != "system" || init.Subtype != "init" {
		t.Fatalf("stream file does not start with system/init: %v %+v", err, init)
	}
	if strings.Join(argv, " ") == "" || !strings.Contains(strings.Join(argv, " "), "--session-id "+init.SessionID) {
		t.Fatalf("system/init session %q is not the argv --session-id: %q", init.SessionID, argv)
	}
	if strings.Contains(string(sb), f.key) {
		t.Fatalf("attest key path reached the worker stream")
	}
}

func TestRunDryRunTierOnlySpec(t *testing.T) {
	stateDir := t.TempDir()
	code, out := attestCapture(t, func() int {
		return cmdRun([]string{"--dry-run", "--spec", filepath.Join("testdata", "tiers-only"), "--project", t.TempDir(), "--state-dir", stateDir})
	})
	if code != 0 {
		t.Fatalf("dry-run exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"3.a      tier 3  deps [2.a, 2.b] (tiers)",
		"3.b      tier 3  deps [2.a, 2.b] (tiers)",
		"2.a      tier 2  deps [1.a] (tiers)",
		"1.a      tier 1  deps [] (tiers)",
		"1: 1.a\n", "2: 2.a 2.b\n", "3: 3.a 3.b\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if ents, _ := os.ReadDir(stateDir); len(ents) != 0 {
		t.Fatalf("--dry-run wrote into the state dir: %v", ents)
	}
}

func TestRunResumeRefusesChangedSpec(t *testing.T) {
	f := runNewFix(t, "e2e-spec")
	f.mustExit(f.run("--mutation", "off", "--usage-probe=false"), 0)
	run := f.runID()
	write(t, f.spec, "prompts/2.b.md", "# Task 2.b\n\nSomething else now.\n")
	res := f.run("--mutation", "off", "--usage-probe=false", "--run", run)
	if res.code != 2 || !strings.Contains(res.stderr, "spec changed since import") {
		t.Fatalf("resume with an edited spec: want exit 2 and 'spec changed since import', got %d\n%s", res.code, res.stderr)
	}
}
