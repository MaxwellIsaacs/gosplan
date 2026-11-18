// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// runfix_test.go: fixtures for the `gosplan run` / `gosplan status` tests.
// A spec dir and a project repo are generated per test; the worker is the
// suite's fakeclaude, driven by a scenario file; the tests read only the
// documented outputs (exit code, report.json / `gosplan status --json`, the state
// dir layout, gosplan.db via sqlite3, events.jsonl) and the fake's
// own ledger.
package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// makeSpec writes a gosplan spec dir: spec.md, prompts/sequence.txt and
// for every task id in seq a prompt and a sidecar with a one-line scope,
// allow_new_protected = true, and `deps` when deps has the id.
func makeSpec(t *testing.T, seq string, deps map[string][]string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "spec.md", "# conformance fixture spec\n")
	writeFile(t, dir, "prompts/sequence.txt", "# conformance fixture\n"+seq+"\n")
	for _, id := range regexp.MustCompile(`[0-9]+\.[a-z]+`).FindAllString(seq, -1) {
		writeFile(t, dir, "prompts/"+id+".md", "# task "+id+"\nCommit fake-"+id+".txt.\n")
		side := fmt.Sprintf("scope = [\"fake-%s.txt\"]\nallow_new_protected = true\n", id)
		if d, ok := deps[id]; ok {
			q := make([]string, len(d))
			for i, x := range d {
				q[i] = `"` + x + `"`
			}
			side += "deps = [" + strings.Join(q, ", ") + "]\n"
		}
		writeFile(t, dir, "prompts/"+id+".task.toml", side)
	}
	return dir
}

// runFix is one spec + project + state dir + fake claude dir.
type runFix struct {
	t                          *testing.T
	spec, project, state, fake string
	masterSHA                  string
}

func newRunFix(t *testing.T, seq string, deps map[string][]string) *runFix {
	t.Helper()
	f := &runFix{t: t, spec: makeSpec(t, seq, deps), project: t.TempDir(), state: t.TempDir(), fake: t.TempDir()}
	gitc(t, f.project, "init", "-q", "-b", "master")
	writeFile(t, f.project, "nightrunner.toml", "[verify]\ntest = \"sh check.sh\"\n")
	writeFile(t, f.project, "check.sh", "ls fake-*.txt >/dev/null 2>&1 && ! grep -q BAD fake-*.txt\n")
	writeFile(t, f.project, "README", "fixture\n")
	gitc(t, f.project, "add", "-A")
	gitc(t, f.project, "commit", "-q", "-m", "base")
	f.masterSHA = gitc(t, f.project, "rev-parse", "master")
	return f
}

// scenario writes the fake's scenario.json (keys "<task>" or "<task>.<n>").
func (f *runFix) scenario(m map[string][]map[string]any) {
	f.t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	writeFile(f.t, f.fake, "scenario.json", string(b))
}

// sample copies a recorded sample from conformance/testdata/samples into the fake dir.
func (f *runFix) sample(name, as string) {
	f.t.Helper()
	writeFile(f.t, f.fake, as, readFile(f.t, filepath.Join(moduleRoot, "conformance", "testdata", "samples", name)))
}

func (f *runFix) env() []string {
	return []string{"CONF_FAKE_DIR=" + f.fake, "XDG_STATE_HOME=" + filepath.Join(f.state, "xdg"), "TMPDIR=" + f.t.TempDir()}
}

// args is the standard `gosplan run` invocation (5.a flags) plus extra; a later
// flag of the same name wins, as with Go's flag package.
func (f *runFix) args(extra ...string) []string {
	a := []string{"run", "--spec", f.spec, "--project", f.project, "--state-dir", f.state,
		"--claude", fakeClaude, "--gosplan-bin", nrBin,
		"--max-parallel", "3", "--max-attempts", "2",
		"--stall-timeout", "3s", "--retry-base", "1s",
		"--mutation", "off", "--usage-probe=false",
		"--attest-key", filepath.Join(f.state, "no-such-key")}
	return append(a, extra...)
}

// runNR runs `gosplan run` to completion (bounded at 5 minutes).
func (f *runFix) runNR(extra ...string) result {
	f.t.Helper()
	return run(f.t, 5*time.Minute, f.project, f.env(), append([]string{nrBin}, f.args(extra...)...)...)
}

// startNR starts `gosplan run` in its own process group and returns it running.
func (f *runFix) startNR(extra ...string) *exec.Cmd {
	f.t.Helper()
	cmd := exec.Command(nrBin, f.args(extra...)...)
	cmd.Dir = f.project
	cmd.Env = append(os.Environ(), f.env()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logf, err := os.Create(filepath.Join(f.t.TempDir(), "gosplan-run.log"))
	if err != nil {
		f.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		f.t.Fatalf("start gosplan run: %v", err)
	}
	f.t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		logf.Close()
	})
	return cmd
}

// ledgerRec is one record written by fakeclaude.
type ledgerRec struct {
	Event     string   `json:"event"`
	Task      string   `json:"task"`
	Attempt   string   `json:"attempt"`
	PID       int      `json:"pid"`
	PGID      int      `json:"pgid"`
	ChildPID  int      `json:"child_pid"`
	SessionID string   `json:"session_id"`
	Model     string   `json:"model"`
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	Live      int      `json:"live"`
	Code      int      `json:"code"`
	Prompt    string   `json:"prompt"`
	TMS       int64    `json:"t_ms"`
}

func (f *runFix) ledger() []ledgerRec {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.fake, "ledger.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out []ledgerRec
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var r ledgerRec
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			f.t.Fatalf("ledger line %q: %v", l, err)
		}
		out = append(out, r)
	}
	return out
}

// starts returns the "start" records for task (all tasks if task == "").
func (f *runFix) starts(task string) []ledgerRec {
	var out []ledgerRec
	for _, r := range f.ledger() {
		if r.Event == "start" && (task == "" || r.Task == task) {
			out = append(out, r)
		}
	}
	return out
}

func (f *runFix) find(event, task, attempt string) (ledgerRec, bool) {
	for _, r := range f.ledger() {
		if r.Event == event && r.Task == task && (attempt == "" || r.Attempt == attempt) {
			return r, true
		}
	}
	return ledgerRec{}, false
}

// runIDs lists <state>/runs/<run> directories, sorted.
func (f *runFix) runIDs() []string {
	ents, _ := os.ReadDir(filepath.Join(f.state, "runs"))
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func (f *runFix) onlyRun() string {
	f.t.Helper()
	ids := f.runIDs()
	if len(ids) != 1 {
		f.t.Fatalf("want exactly one run dir under %s/runs, got %v", f.state, ids)
	}
	return ids[0]
}

func (f *runFix) db() string { return filepath.Join(f.state, "gosplan.db") }

// report is the documented run report.
type report struct {
	Schema            string    `json:"schema"`
	Run               string    `json:"run"`
	SpecDir           string    `json:"spec_dir"`
	Project           string    `json:"project"`
	BaseSHA           string    `json:"base_sha"`
	Status            string    `json:"status"`
	StartedAt         string    `json:"started_at"`
	FinishedAt        string    `json:"finished_at"`
	IntegrationBranch string    `json:"integration_branch"`
	IntegrationSHA    string    `json:"integration_sha"`
	Tasks             []rTask   `json:"tasks"`
	Windows           []rWindow `json:"windows"`
	Notes             []string  `json:"notes"`
}

type rTask struct {
	ID            string     `json:"id"`
	Tier          int        `json:"tier"`
	Status        string     `json:"status"`
	Deps          []string   `json:"deps"`
	DepsSource    string     `json:"deps_source"`
	BlockedReason string     `json:"blocked_reason"`
	Attempts      []rAttempt `json:"attempts"`
}

type rAttempt struct {
	N               int      `json:"n"`
	Model           string   `json:"model"`
	Outcome         string   `json:"outcome"`
	ExitCode        *int     `json:"exit_code"`
	ResultSubtype   string   `json:"result_subtype"`
	HeadSHA         string   `json:"head_sha"`
	CostUSDEstimate *float64 `json:"cost_usd_estimate"`
	Verdict         *struct {
		Result      string   `json:"result"`
		FailedGates []string `json:"failed_gates"`
		TreeSHA     string   `json:"tree_sha"`
		CacheHit    bool     `json:"cache_hit"`
	} `json:"verdict"`
	AttestationPath string         `json:"attestation_path"`
	Mutation        map[string]any `json:"mutation"`
}

type rWindow struct {
	Window      string   `json:"window"`
	Utilization *float64 `json:"utilization"`
	ResetsAt    *int64   `json:"resets_at"`
	Status      string   `json:"status"`
	Source      string   `json:"source"`
	Documented  bool     `json:"documented"`
	ObservedAt  string   `json:"observed_at"`
	AgeSeconds  *float64 `json:"age_seconds"`
}

// status runs `gosplan status --state-dir S --json` and decodes the report.
func (f *runFix) status(extra ...string) (report, map[string]any) {
	f.t.Helper()
	res := gosplan(f.t, f.project, f.env(), append([]string{"status", "--state-dir", f.state, "--json"}, extra...)...)
	if res.code != 0 {
		f.t.Fatalf("gosplan status --json: want exit 0, got %s", res)
	}
	var r report
	var raw map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &r); err != nil {
		f.t.Fatalf("gosplan status --json is not a report: %v\n%s", err, res)
	}
	_ = json.Unmarshal([]byte(res.stdout), &raw)
	return r, raw
}

func (r report) task(t *testing.T, id string) rTask {
	t.Helper()
	for _, x := range r.Tasks {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("report has no task %s", id)
	return rTask{}
}

// mustComplete asserts `gosplan run` exited 0 (run complete), the documented
// success code (5.a: exit 0 complete, 1 halted, 2 error or aborted).
func mustExit(t *testing.T, what string, res result, want int) {
	t.Helper()
	if res.timedOut || res.code != want {
		t.Fatalf("%s: want exit %d, got %s", what, want, res)
	}
}
