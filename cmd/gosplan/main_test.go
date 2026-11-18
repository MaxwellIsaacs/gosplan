package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

var runIDPattern = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{4}$`)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// project builds a repo with a base commit carrying nightrunner.toml and a
// task commit that adds feature (if non-empty). It returns dir, base, head.
func project(t *testing.T, feature string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "nightrunner.toml", "[verify]\ntest = \"test -f src/feature.txt\"\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "checkout", "-q", "-b", "task/x")
	write(t, dir, feature, "x")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "task")
	return dir, base, git(t, dir, "rev-parse", "HEAD")
}

func verifyArgs(t *testing.T, dir, base, head string, extra ...string) []string {
	return append([]string{"--project", dir, "--task", "1.a", "--base", base, "--head", head,
		"--logs", t.TempDir()}, extra...)
}

func readLog(t *testing.T, path string) []events.Event {
	t.Helper()
	evs, err := events.ReadFile(path) // validates seq, types, verdict rule
	if err != nil {
		t.Fatalf("log does not validate: %v", err)
	}
	return evs
}

func types(evs []events.Event) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, string(ev.Type))
	}
	return out
}

func verdictOf(t *testing.T, ev events.Event) events.VerdictData {
	t.Helper()
	if ev.Type != events.Verdict {
		t.Fatalf("want verdict event, got %s", ev.Type)
	}
	var d events.VerdictData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestVerifyLogPass(t *testing.T) {
	dir, base, head := project(t, "src/feature.txt")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	code := cmdVerify(verifyArgs(t, dir, base, head,
		"--log", logPath, "--run", "20261026-120000-abcd", "--task-base", base, "--attempt", "2"))
	if code != exitPass {
		t.Fatalf("exit %d, want %d", code, exitPass)
	}
	evs := readLog(t, logPath)
	want := []string{"launch", "gate", "gate", "gate", "gate", "gate", "gate", "gate", "gate", "verdict"}
	if got := types(evs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	for _, ev := range evs {
		if ev.Run != "20261026-120000-abcd" || ev.Task != "1.a" {
			t.Fatalf("envelope wrong: %+v", ev)
		}
	}
	var launch events.LaunchData
	if err := json.Unmarshal(evs[0].Data, &launch); err != nil {
		t.Fatal(err)
	}
	if launch.Attempt != 2 || launch.BaseSHA != base {
		t.Fatalf("launch = %+v", launch)
	}
	var first events.GateData
	if err := json.Unmarshal(evs[1].Data, &first); err != nil {
		t.Fatal(err)
	}
	if first.Name != "object_integrity" || first.Status != events.GatePass {
		t.Fatalf("first gate = %+v", first)
	}
	v := verdictOf(t, evs[len(evs)-1])
	if v.Result != "pass" || len(v.FailedGates) != 0 || v.CacheHit {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.Commands) != 1 || v.Commands[0].Name != "test" || v.Commands[0].ExitCode != 0 {
		t.Fatalf("verdict commands = %+v", v.Commands)
	}
	if want := git(t, dir, "rev-parse", head+"^{tree}"); v.TreeSHA != want {
		t.Fatalf("tree_sha = %s, want %s", v.TreeSHA, want)
	}
}

// Failed gates in the verdict are exactly the gate events with status fail.
func TestVerifyLogFail(t *testing.T) {
	dir, base, head := project(t, "docs/other.txt")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")

	if code := cmdVerify(verifyArgs(t, dir, base, head, "--log", logPath, "--scope", "src/**")); code != exitFail {
		t.Fatalf("exit %d, want %d", code, exitFail)
	}
	evs := readLog(t, logPath)
	if evs[0].Type != events.Gate {
		t.Fatalf("no --task-base/--attempt: want no launch event, got %s first", evs[0].Type)
	}
	var failed []string
	for _, ev := range evs[:len(evs)-1] {
		var g events.GateData
		if err := json.Unmarshal(ev.Data, &g); err != nil {
			t.Fatal(err)
		}
		if g.Status == events.GateFail {
			failed = append(failed, g.Name)
		}
	}
	v := verdictOf(t, evs[len(evs)-1])
	if v.Result != "fail" || strings.Join(v.FailedGates, ",") != strings.Join(failed, ",") {
		t.Fatalf("verdict %+v does not match failed gate events %v", v, failed)
	}
	if strings.Join(failed, ",") != "scope,cmd:test" {
		t.Fatalf("failed gates = %v, want scope,cmd:test", failed)
	}
}

// Two runs append to one log: seq continues, and the cache hit is recorded.
func TestVerifyLogAppendsAndRecordsCacheHit(t *testing.T) {
	dir, base, head := project(t, "src/feature.txt")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	cache := t.TempDir()

	for i := 0; i < 2; i++ {
		if code := cmdVerify(verifyArgs(t, dir, base, head, "--log", logPath, "--cache", cache)); code != exitPass {
			t.Fatalf("run %d: exit %d", i, code)
		}
	}
	evs := readLog(t, logPath)
	var verdicts []events.VerdictData
	for _, ev := range evs {
		if ev.Type == events.Verdict {
			verdicts = append(verdicts, verdictOf(t, ev))
		}
	}
	if len(verdicts) != 2 || verdicts[0].CacheHit || !verdicts[1].CacheHit {
		t.Fatalf("want fresh then cached verdict, got %+v", verdicts)
	}
	if !runIDPattern.MatchString(evs[0].Run) {
		t.Fatalf("generated run id %q does not match YYYYMMDD-HHMMSS-<rand4>", evs[0].Run)
	}
}

func TestVerifyNoLogFlagWritesNothing(t *testing.T) {
	dir, base, head := project(t, "src/feature.txt")
	wd := t.TempDir()
	t.Chdir(wd)
	if code := cmdVerify(verifyArgs(t, dir, base, head)); code != exitPass {
		t.Fatalf("exit %d", code)
	}
	for _, d := range []string{wd, dir} {
		matches, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
		nested, _ := filepath.Glob(filepath.Join(d, ".nightrunner", "*"))
		if len(matches)+len(nested) != 0 {
			t.Fatalf("no --log, but found %v %v", matches, nested)
		}
	}
}

func TestVerifyLogFlagErrors(t *testing.T) {
	dir, base, head := project(t, "src/feature.txt")
	cases := map[string][]string{
		"attempt without log":   {"--attempt", "1"},
		"task-base without log": {"--task-base", base},
		"run without log":       {"--run", "r"},
		"short task-base":       {"--log", filepath.Join(t.TempDir(), "e.jsonl"), "--task-base", base[:12]},
		"unwritable log":        {"--log", filepath.Join(t.TempDir(), "missing-dir", "e.jsonl")},
		"attempt zero":          {"--log", filepath.Join(t.TempDir(), "e.jsonl"), "--attempt", "0"},
		"attempt negative":      {"--log", filepath.Join(t.TempDir(), "e.jsonl"), "--attempt", "-1"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if code := cmdVerify(verifyArgs(t, dir, base, head, extra...)); code != exitError {
				t.Fatalf("exit %d, want %d", code, exitError)
			}
		})
	}
}

// A crash mid-append leaves an unterminated last line. The next
// `gosplan verify --log` run must not wedge on it (it used to exit 2 forever):
// it moves the torn bytes to <log>.torn-<ts> and appends after them.
func TestVerifyLogRecoversFromTornWrite(t *testing.T) {
	dir, base, head := project(t, "src/feature.txt")
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	run := "20261026-120000-abcd"
	if code := cmdVerify(verifyArgs(t, dir, base, head, "--log", logPath, "--run", run)); code != exitPass {
		t.Fatalf("first run: exit %d", code)
	}
	n := len(readLog(t, logPath))
	torn := `{"v":1,"seq":` + strconv.Itoa(n+1) + `,"ts":"2026-10-26T12:00:00Z","run":"` + run + `","task":"1.a","type":"ga`
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(torn); err != nil {
		t.Fatal(err)
	}
	f.Close()

	for i := 0; i < 2; i++ {
		if code := cmdVerify(verifyArgs(t, dir, base, head, "--log", logPath, "--run", run)); code != exitPass {
			t.Fatalf("run %d after torn write: exit %d, want %d", i+2, code, exitPass)
		}
	}
	evs := readLog(t, logPath)
	if len(evs) != 3*n {
		t.Fatalf("events = %d, want %d", len(evs), 3*n)
	}
	q, _ := filepath.Glob(logPath + ".torn-*")
	if len(q) != 1 {
		t.Fatalf("want one quarantine file, got %v", q)
	}
	if got, _ := os.ReadFile(q[0]); string(got) != torn {
		t.Fatalf("quarantine holds %q, want the torn bytes %q", got, torn)
	}
}

// A log written by `gosplan verify` replays cleanly: it has no task_state events,
// so it matches an empty state and mismatches any recorded task status.
func TestReplayCheckOnVerifyLog(t *testing.T) {
	dir, base, head := project(t, "src/feature.txt")
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "events.jsonl")
	if code := cmdVerify(verifyArgs(t, dir, base, head, "--log", logPath, "--task-base", base, "--attempt", "1")); code != exitPass {
		t.Fatalf("verify exit %d", code)
	}

	empty := filepath.Join(tmp, "empty.json")
	write(t, tmp, "empty.json", "{}")
	if code := cmdReplay([]string{"--log", logPath, "--state", empty, "--check"}); code != exitPass {
		t.Fatalf("replay --check vs {}: exit %d, want %d", code, exitPass)
	}
	stale := filepath.Join(tmp, "stale.json")
	write(t, tmp, "stale.json", `{"1.a":"passed"}`)
	if code := cmdReplay([]string{"--log", logPath, "--state", stale, "--check"}); code != exitFail {
		t.Fatalf("replay --check vs stale state: exit %d, want %d", code, exitFail)
	}

	// The orchestrator appends task_state around the verify events; replay
	// must still read the mixed log.
	l, err := events.Open(logPath, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append("1.a", events.TaskState, events.TaskStateData{From: "", To: "passed"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if code := cmdReplay([]string{"--log", logPath, "--state", stale, "--check"}); code != exitPass {
		t.Fatalf("replay --check after task_state: exit %d, want %d", code, exitPass)
	}
}

// The CLI default --project "." must verify the repo in the current dir.
func TestVerifyDefaultProjectIsCwd(t *testing.T) {
	dir, base, head := project(t, "src/feature.txt")
	t.Chdir(dir)
	if code := cmdVerify([]string{"--task", "1.a", "--base", base, "--head", head}); code != exitPass {
		t.Fatalf("exit %d, want %d", code, exitPass)
	}
}
