package verifyexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

var realNR string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "verifyexec-gosplan-")
	if err != nil {
		panic(err)
	}
	realNR = filepath.Join(dir, "gosplan")
	build := exec.Command("go", "build", "-o", realNR, "github.com/MaxwellIsaacs/gosplan/cmd/gosplan")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	code := 1
	if err := build.Run(); err != nil {
		os.Stderr.WriteString("go build ./cmd/gosplan: " + err.Error() + "\n")
	} else {
		code = m.Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

const sha1 = "1111111111111111111111111111111111111111"
const sha2 = "2222222222222222222222222222222222222222"

const passJSON = `{"result":"pass","tree_sha":"abc","gates":[{"name":"cmd:test","status":"pass"}],"failed_gates":[]}`
const failJSON = `{"result":"fail","tree_sha":"abc","gates":[{"name":"scope","status":"pass"},{"name":"cmd:test","status":"fail","detail":"exit 1"}],"failed_gates":["cmd:test"]}`

// fakeNR writes a shell script that records its argv to <dir>/argv and
// behaves per FAKE_* env vars.
func fakeNR(t *testing.T) (bin, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "gosplan")
	argvFile = filepath.Join(dir, "argv")
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done > "` + argvFile + `"
if [ -n "$FAKE_SLEEP" ]; then
  sleep "$FAKE_SLEEP" &
  echo $! > "$FAKE_CHILD_PID"
  wait
fi
[ -n "$FAKE_STDERR" ] && printf '%s\n' "$FAKE_STDERR" >&2
[ -n "$FAKE_SIGNAL" ] && kill -$FAKE_SIGNAL $$
printf '%s' "$FAKE_STDOUT"
exit "${FAKE_EXIT:-0}"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvFile
}

func req() core.VerifyRequest {
	return core.VerifyRequest{Project: "/p", Run: "r1", Task: "t1", Attempt: 1, Base: sha1, Head: sha2}
}

func run(t *testing.T, bin string, r core.VerifyRequest) core.VerifyOutcome {
	t.Helper()
	out, err := Exec{GosplanBin: bin, OutDir: t.TempDir(), Timeout: time.Minute}.Verify(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerifyExecArgsNoCache(t *testing.T) {
	r := req()
	r.Scope = []string{"a/**", "--cache", "--cache=/tmp/x", "b/*.go"}
	r.AllowNewProtected = true
	r.EventLog, r.LogsDir = "/l/events.jsonl", "/l/logs"
	got := Args(r)
	want := []string{"verify", "--project", "/p", "--task", "t1", "--base", sha1, "--head", sha2,
		"--scope", "a/**", "--scope", "--cache", "--scope", "--cache=/tmp/x", "--scope", "b/*.go",
		"--allow-new-protected", "--json", "--log", "/l/events.jsonl", "--run", "r1", "--logs", "/l/logs"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	// Every --cache in argv is a value of --scope, never a flag.
	for i, a := range got {
		if (a == "--cache" || strings.HasPrefix(a, "--cache=")) && got[i-1] != "--scope" {
			t.Errorf("--cache flag at %d: %q", i, got)
		}
	}
	for _, f := range []string{"--task-base", "--attempt"} {
		if slices.Contains(got, f) {
			t.Errorf("argv has %s", f)
		}
	}
	// nil scope: no --scope. No EventLog: no --log and no --run.
	plain := Args(req())
	if slices.Contains(plain, "--scope") || slices.Contains(plain, "--log") || slices.Contains(plain, "--run") ||
		slices.Contains(plain, "--allow-new-protected") {
		t.Errorf("plain argv = %q", plain)
	}
	// And the real exec path passes the same argv.
	bin, argvFile := fakeNR(t)
	t.Setenv("FAKE_STDOUT", passJSON)
	run(t, bin, req())
	b, _ := os.ReadFile(argvFile)
	if strings.Split(strings.TrimSpace(string(b)), "\n")[0] != "verify" || strings.Contains(string(b), "--cache") {
		t.Errorf("recorded argv:\n%s", b)
	}
}

func TestVerifyExecPassNeedsJSON(t *testing.T) {
	bin, _ := fakeNR(t)
	cases := []struct {
		name, stdout string
		want         int
	}{
		{"exit0 but fail json", `{"result":"fail","failed_gates":["x"]}`, 2},
		{"exit0 garbage", `not json at all`, 2},
		{"exit0 empty", ``, 2},
		{"exit0 no result", `{}`, 2},
		{"exit0 pass with failed gates", `{"result":"pass","failed_gates":["x"]}`, 2},
		{"exit0 pass json", passJSON, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FAKE_EXIT", "0")
			t.Setenv("FAKE_STDOUT", c.stdout)
			out := run(t, bin, req())
			if out.Exit != c.want {
				t.Fatalf("Exit = %d, want %d (%+v)", out.Exit, c.want, out)
			}
			if c.want == 0 && (out.Result != "pass" || out.TreeSHA != "abc" || string(out.VerdictJSON) != c.stdout) {
				t.Errorf("pass outcome = %+v", out)
			}
			if c.want == 2 && (out.Result != "" || out.VerdictJSON != nil || out.Detail == "") {
				t.Errorf("exit 2 outcome = %+v", out)
			}
		})
	}
	// Exit 1 needs a fail verdict too.
	t.Setenv("FAKE_EXIT", "1")
	t.Setenv("FAKE_STDOUT", passJSON)
	if out := run(t, bin, req()); out.Exit != 2 {
		t.Errorf("exit 1 with pass json: Exit = %d", out.Exit)
	}
}

func TestVerifyExecFailMapping(t *testing.T) {
	bin, _ := fakeNR(t)
	t.Setenv("FAKE_EXIT", "1")
	t.Setenv("FAKE_STDOUT", failJSON)
	out := run(t, bin, req())
	if out.Exit != 1 || out.Result != "fail" || out.TreeSHA != "abc" {
		t.Fatalf("outcome = %+v", out)
	}
	if !slices.Equal(out.FailedGates, []string{"cmd:test"}) {
		t.Errorf("FailedGates = %v", out.FailedGates)
	}
	if out.Detail != "cmd:test: exit 1" {
		t.Errorf("Detail = %q", out.Detail)
	}
	if string(out.VerdictJSON) != failJSON {
		t.Errorf("VerdictJSON = %q", out.VerdictJSON)
	}
}

func TestVerifyExecOtherExitIsError(t *testing.T) {
	bin, _ := fakeNR(t)
	t.Setenv("FAKE_STDERR", "boom: could not verify")
	for _, c := range []struct{ name, exit, sig string }{
		{"exit2", "2", ""}, {"exit3", "3", ""}, {"signal", "0", "KILL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FAKE_EXIT", c.exit)
			t.Setenv("FAKE_SIGNAL", c.sig)
			// Valid pass JSON must not rescue a non-0/1 exit.
			t.Setenv("FAKE_STDOUT", passJSON)
			out := run(t, bin, req())
			if out.Exit != 2 || out.VerdictJSON != nil {
				t.Fatalf("outcome = %+v", out)
			}
			if c.sig == "" && !strings.Contains(out.Detail, "exited "+c.exit) {
				t.Errorf("Detail = %q", out.Detail)
			}
			if c.sig != "" && !strings.Contains(out.Detail, "signal") {
				t.Errorf("Detail = %q", out.Detail)
			}
		})
	}
}

func TestVerifyExecKeepsOutputFiles(t *testing.T) {
	bin, _ := fakeNR(t)
	t.Setenv("FAKE_EXIT", "1")
	t.Setenv("FAKE_STDOUT", failJSON)
	t.Setenv("FAKE_STDERR", "some stderr")
	dir := t.TempDir()
	if _, err := (Exec{GosplanBin: bin, OutDir: dir, Timeout: time.Minute}).Verify(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"t1.1.1/verdict.json": failJSON, "t1.1.1/verdict.stderr": "some stderr\n", "t1.1.1/verdict.exit": "1\n"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
}

func TestVerifyExecTimeout(t *testing.T) {
	bin, _ := fakeNR(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_SLEEP", "30")
	t.Setenv("FAKE_CHILD_PID", pidFile)
	t.Setenv("FAKE_STDOUT", passJSON)
	start := time.Now()
	out, err := Exec{GosplanBin: bin, OutDir: t.TempDir(), Timeout: 300 * time.Millisecond}.Verify(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("took %s", el)
	}
	if out.Exit != 2 || !strings.Contains(out.Detail, "timed out") {
		t.Fatalf("outcome = %+v", out)
	}
	assertChildGone(t, pidFile)
}

func TestVerifyExecCancelKillsGroup(t *testing.T) {
	bin, _ := fakeNR(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_SLEEP", "30")
	t.Setenv("FAKE_CHILD_PID", pidFile)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, err := Exec{GosplanBin: bin, OutDir: t.TempDir(), Timeout: time.Minute}.Verify(ctx, req())
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 3*time.Second || out.Exit != 2 {
		t.Fatalf("took %s, outcome %+v", el, out)
	}
	assertChildGone(t, pidFile)
}

func assertChildGone(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake never recorded a child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A zombie that nobody reaped still answers kill(0); check its state.
	if st, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); strings.Contains(string(st), ") Z ") {
		return
	}
	t.Errorf("child process %d survived the group kill", pid)
}

func TestVerifyExecRejectsRefs(t *testing.T) {
	bin, argvFile := fakeNR(t)
	for _, c := range []struct{ base, head string }{
		{sha1, "HEAD"}, {"main", sha2}, {sha1, strings.ToUpper(strings.Repeat("ab", 20))}, {sha1, sha2[:39]}, {sha1, ""},
	} {
		r := req()
		r.Base, r.Head = c.base, c.head
		out := run(t, bin, r)
		if out.Exit != 2 {
			t.Errorf("base %q head %q: Exit = %d", c.base, c.head, out.Exit)
		}
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Error("fake gosplan was run for an invalid ref")
	}
}

func TestVerifyExecEmptyOutDirAndMissingBin(t *testing.T) {
	// An empty OutDir is a programming error; a missing binary is could-not-verify.
	if _, err := (Exec{GosplanBin: "x", Timeout: time.Minute}).Verify(context.Background(), req()); err == nil {
		t.Error("empty OutDir: want error")
	}
	out, err := Exec{GosplanBin: filepath.Join(t.TempDir(), "missing"), OutDir: t.TempDir(), Timeout: time.Minute}.Verify(context.Background(), req())
	if err != nil || out.Exit != 2 {
		t.Errorf("missing binary: %+v, %v", out, err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=master"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestVerifyExecRealPassAndFail(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	cfg := "[verify]\ntest = \"test -f feature.txt\"\n"
	if err := os.WriteFile(filepath.Join(repo, "nightrunner.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "nightrunner.toml")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	base := gitIn(t, repo, "rev-parse", "HEAD")

	gitIn(t, repo, "checkout", "-q", "-b", "good")
	os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("x\n"), 0o644)
	gitIn(t, repo, "add", "feature.txt")
	gitIn(t, repo, "commit", "-q", "-m", "feature")
	good := gitIn(t, repo, "rev-parse", "HEAD")

	gitIn(t, repo, "checkout", "-q", "-b", "bad", base)
	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("x\n"), 0o644)
	gitIn(t, repo, "add", "other.txt")
	gitIn(t, repo, "commit", "-q", "-m", "other")
	bad := gitIn(t, repo, "rev-parse", "HEAD")

	work := t.TempDir()
	logPath := filepath.Join(work, "events.jsonl")
	ex := Exec{GosplanBin: realNR, OutDir: filepath.Join(work, "out"), Timeout: 5 * time.Minute}
	mk := func(task string, head string) core.VerifyRequest {
		return core.VerifyRequest{Project: repo, Run: "run1", Task: task, Attempt: 1, Base: base, Head: head,
			EventLog: logPath, LogsDir: filepath.Join(work, "logs")}
	}

	out, err := ex.Verify(context.Background(), mk("good", good))
	if err != nil {
		t.Fatal(err)
	}
	if out.Exit != 0 || out.Result != "pass" || out.TreeSHA == "" {
		t.Fatalf("good: %+v", out)
	}

	out, err = ex.Verify(context.Background(), mk("bad", bad))
	if err != nil {
		t.Fatal(err)
	}
	if out.Exit != 1 || !slices.Contains(out.FailedGates, "cmd:test") || !strings.Contains(out.Detail, "cmd:test") {
		t.Fatalf("bad: %+v", out)
	}

	evs, err := events.ReadFile(logPath)
	if err != nil || len(evs) == 0 {
		t.Fatalf("read log: %d events, %v", len(evs), err)
	}
	if err := events.Validate(evs); err != nil {
		t.Errorf("log does not validate: %v", err)
	}
}

// A verify that times out must not leave its gate commands running. The
// gates run in their own process group under the real gosplan verify; SIGTERM
// lets gosplan verify cancel and kill them before its own group is SIGKILLed.
func TestVerifyExecTimeoutKillsGates(t *testing.T) {
	repo := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "gate.pid")
	gitIn(t, repo, "init", "-q")
	cfg := "[verify]\ntest = \"echo $$ > " + pidFile + "; exec sleep 300\"\n"
	if err := os.WriteFile(filepath.Join(repo, "nightrunner.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "nightrunner.toml")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	base := gitIn(t, repo, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644)
	gitIn(t, repo, "add", "f.txt")
	gitIn(t, repo, "commit", "-q", "-m", "work")
	head := gitIn(t, repo, "rev-parse", "HEAD")

	ex := Exec{GosplanBin: realNR, OutDir: t.TempDir(), Timeout: 3 * time.Second}
	start := time.Now()
	out, err := ex.Verify(context.Background(), core.VerifyRequest{Project: repo, Task: "t", Attempt: 1, Base: base, Head: head})
	if err != nil {
		t.Fatal(err)
	}
	if out.Exit != 2 || !strings.Contains(out.Detail, "timed out") {
		t.Fatalf("outcome = %+v", out)
	}
	if d := time.Since(start); d > 3*time.Second+KillGrace {
		t.Errorf("took %v", d)
	}
	assertChildGone(t, pidFile)
}

// Re-review 5b: every invocation gets its own directory (a straggler
// `gosplan verify` of a crashed run can never write the files a live one
// reads) and its own logs dir, and runs with the run's verify nonce so a
// resume can find and kill stragglers.
func TestVerifyExecOwnDirAndNonce(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gosplan")
	argv := filepath.Join(dir, "argv")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "` + argv + `"
echo "nonce=$GOSPLAN_ATTEMPT_NONCE" >&2
printf '%s' '` + passJSON + `'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOSPLAN_ATTEMPT_NONCE", "inherited")
	out := filepath.Join(dir, "verify")
	r := req()
	r.LogsDir = filepath.Join(dir, "logs")
	for i := 0; i < 2; i++ {
		if o, err := (Exec{GosplanBin: bin, OutDir: out, Timeout: time.Minute}).Verify(context.Background(), r); err != nil || o.Exit != 0 {
			t.Fatalf("verify %d: %+v, %v", i, o, err)
		}
	}
	for _, seq := range []string{"1", "2"} {
		b, err := os.ReadFile(filepath.Join(out, "t1.1."+seq, "verdict.stderr"))
		if err != nil || string(b) != "nonce=r1.verify.t1.1\n" {
			t.Errorf("t1.1.%s/verdict.stderr = %q, %v", seq, b, err)
		}
	}
	b, _ := os.ReadFile(argv)
	for _, seq := range []string{"1", "2"} {
		if !strings.Contains(string(b), filepath.Join(dir, "logs", seq)+"\n") {
			t.Errorf("no --logs .../logs/%s in argv:\n%s", seq, b)
		}
	}
}
