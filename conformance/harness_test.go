// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// harness_test.go: shared black-box helpers. Everything here drives the
// built `gosplan` binary, git, the suite's fake `claude`, and the sqlite3 CLI.
// Nothing imports gosplan packages.
package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

var (
	moduleRoot string // repo root (parent of conformance/)
	binDir     string // temp dir holding the built binaries
	nrBin      string // the `gosplan` under test
	fakeClaude string // conformance/fakeclaude
)

// TestMain builds `gosplan` and the fake `claude` once. A build failure of `gosplan`
// itself is reported loudly: every test would fail for the same reason.
func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(2)
	}
	moduleRoot = filepath.Dir(wd)
	binDir, err = os.MkdirTemp("", "gosplan-conformance-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(2)
	}
	nrBin = filepath.Join(binDir, "gosplan")
	fakeClaude = filepath.Join(binDir, "claude")
	for _, b := range []struct{ out, pkg string }{{nrBin, "./cmd/gosplan"}, {fakeClaude, "./conformance/fakeclaude"}} {
		cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
		cmd.Dir = moduleRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: build %s: %v\n%s", b.pkg, err, out)
			os.RemoveAll(binDir)
			os.Exit(2)
		}
	}
	code := m.Run()
	os.RemoveAll(binDir)
	os.Exit(code)
}

// result is one finished process.
type result struct {
	code           int // exit code; -1 if killed by a signal
	stdout, stderr string
	timedOut       bool
}

func (r result) String() string {
	return fmt.Sprintf("exit=%d timedOut=%v\nstdout:\n%s\nstderr:\n%s", r.code, r.timedOut, clip(r.stdout), clip(r.stderr))
}

func clip(s string) string {
	if len(s) > 4000 {
		return s[:2000] + "\n...[clipped]...\n" + s[len(s)-2000:]
	}
	return s
}

// run executes argv in dir with extra env, killing its process group after
// timeout (a generous bound, never a behaviour under test).
func run(t *testing.T, timeout time.Duration, dir string, env []string, argv ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timedOut := false
	select {
	case <-done:
	case <-ctx.Done():
		timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return result{code: cmd.ProcessState.ExitCode(), stdout: so.String(), stderr: se.String(), timedOut: timedOut}
}

// gosplan runs the binary under test with a 3-minute bound.
func gosplan(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	return run(t, 3*time.Minute, dir, env, append([]string{nrBin}, args...)...)
}

// gitc runs git with a fixed identity and no signing, and fails the test on error.
func gitc(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=master"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// repo is a project with a base commit on `master` and a task branch checked out.
type repo struct {
	t    *testing.T
	dir  string
	base string
}

// newRepo commits nightrunner.toml (toml) plus files on master, then checks
// out branch task/x.
func newRepo(t *testing.T, toml string, files map[string]string) *repo {
	t.Helper()
	dir := t.TempDir()
	gitc(t, dir, "init", "-q", "-b", "master")
	if toml != "" {
		writeFile(t, dir, "nightrunner.toml", toml)
	}
	for p, c := range files {
		writeFile(t, dir, p, c)
	}
	gitc(t, dir, "add", "-A")
	gitc(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	r := &repo{t: t, dir: dir, base: gitc(t, dir, "rev-parse", "HEAD")}
	gitc(t, dir, "checkout", "-q", "-b", "task/x")
	return r
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	gitc(r.t, r.dir, "add", "-A")
	gitc(r.t, r.dir, "commit", "-q", "-m", msg)
	return r.head()
}

func (r *repo) head() string { r.t.Helper(); return gitc(r.t, r.dir, "rev-parse", "HEAD") }

const featureToml = "[verify]\ntest = \"test -f src/feature.txt\"\n"

// verdict is the documented JSON of `gosplan verify --json`.
type verdict struct {
	Task         string `json:"task"`
	BaseSHA      string `json:"base_sha"`
	HeadSHA      string `json:"head_sha"`
	TreeSHA      string `json:"tree_sha"`
	Unrestricted bool   `json:"scope_unrestricted"`
	CacheHit     bool   `json:"cache_hit"`
	Gates        []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"gates"`
	Result      string   `json:"result"`
	FailedGates []string `json:"failed_gates"`
}

func (v verdict) gate(name string) (status, detail string) {
	for _, g := range v.Gates {
		if g.Name == name {
			return g.Status, g.Detail
		}
	}
	return "", ""
}

func (v verdict) gateNames() []string {
	var out []string
	for _, g := range v.Gates {
		out = append(out, g.Name)
	}
	return out
}

// verify runs `gosplan verify --json` for base..head of r with extra args and
// returns the parsed verdict (zero if exit 2) and the process result.
func (r *repo) verify(extra ...string) (verdict, result) {
	r.t.Helper()
	return verifyAt(r.t, r.dir, r.base, r.head(), extra...)
}

func verifyAt(t *testing.T, dir, base, head string, extra ...string) (verdict, result) {
	t.Helper()
	args := append([]string{"verify", "--project", dir, "--task", "1.a", "--base", base, "--head", head, "--json"}, extra...)
	res := gosplan(t, dir, []string{"TMPDIR=" + t.TempDir()}, args...)
	var v verdict
	if res.code == 0 || res.code == 1 {
		if err := json.Unmarshal([]byte(res.stdout), &v); err != nil {
			t.Fatalf("gosplan verify exit %d but stdout is not verdict JSON: %v\n%s", res.code, err, res)
		}
	}
	return v, res
}

// event is one line of events.jsonl (00-event-schema.md envelope).
type event struct {
	V    int             `json:"v"`
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Run  string          `json:"run"`
	Task string          `json:"task"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// readEvents parses every non-empty line of path strictly.
func readEvents(t *testing.T, path string) []event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var out []event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var ev event
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("log line %d is not an event: %v: %s", n, err, sc.Text())
		}
		out = append(out, ev)
	}
	return out
}

// ---------- golden fixtures (testdata/golden, reused, never edited) ----------

type golden struct {
	dir, base, head string
	Scope           []string          `json:"scope"`
	Result          string            `json:"result"`
	FailedGates     []string          `json:"failed_gates"`
	DetailContains  map[string]string `json:"detail_contains"`
}

// goldenFixture builds testdata/golden/<name> with build.sh into a temp dir,
// checks the SHAs against shas.txt and copies uncommitted/ into the tree.
func goldenFixture(t *testing.T, name string) golden {
	t.Helper()
	src := filepath.Join(moduleRoot, "testdata", "golden")
	out := t.TempDir()
	res := run(t, time.Minute, src, nil, "bash", filepath.Join(src, "build.sh"), out)
	if res.code != 0 {
		t.Fatalf("golden build.sh: %s", res)
	}
	var g golden
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(src, name, "expect.json"))), &g); err != nil {
		t.Fatalf("expect.json: %v", err)
	}
	for _, line := range strings.Split(readFile(t, filepath.Join(src, "shas.txt")), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == name {
			g.base, g.head = f[1], f[2]
		}
	}
	if g.base == "" {
		t.Fatalf("golden %s: no line in shas.txt", name)
	}
	g.dir = filepath.Join(out, name)
	if gitc(t, g.dir, "rev-parse", "main") != g.base || gitc(t, g.dir, "rev-parse", "task") != g.head {
		t.Fatalf("golden %s: built SHAs drift from shas.txt", name)
	}
	unc := filepath.Join(src, name, "uncommitted")
	_ = filepath.Walk(unc, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(unc, p)
		writeFile(t, g.dir, rel, readFile(t, p))
		return nil
	})
	return g
}

func (g golden) verify(t *testing.T) (verdict, result) {
	t.Helper()
	var extra []string
	for _, s := range g.Scope {
		extra = append(extra, "--scope", s)
	}
	return verifyAt(t, g.dir, g.base, g.head, extra...)
}

// checkGolden asserts the verdict matches expect.json exactly.
func checkGolden(t *testing.T, name string) verdict {
	t.Helper()
	g := goldenFixture(t, name)
	v, res := g.verify(t)
	wantCode := 1
	if g.Result == "pass" {
		wantCode = 0
	}
	if res.code != wantCode || v.Result != g.Result {
		t.Fatalf("golden %s: want exit %d result %s, got %s", name, wantCode, g.Result, res)
	}
	got := append([]string{}, v.FailedGates...)
	want := append([]string{}, g.FailedGates...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("golden %s: failed_gates want %v got %v", name, want, got)
	}
	for gate, sub := range g.DetailContains {
		if _, d := v.gate(gate); !strings.Contains(d, sub) {
			t.Fatalf("golden %s: gate %s detail %q lacks %q", name, gate, d, sub)
		}
	}
	return v
}

// ---------- sqlite3 CLI (reads the documented gosplan.db schema) ----------

func sqlite3Bin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("GOSPLAN_CONF_SQLITE3"); p != "" {
		return p
	}
	p, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Fatalf("infra: sqlite3 CLI not found (set GOSPLAN_CONF_SQLITE3; conformance/run.sh finds one)")
	}
	return p
}

// sql runs one read query against db and returns trimmed stdout.
func sql(t *testing.T, db, query string) string {
	t.Helper()
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("state DB %s does not exist: %v", db, err)
	}
	res := run(t, time.Minute, "", nil, sqlite3Bin(t), "-bail", "-readonly", db, query)
	if res.code != 0 {
		t.Fatalf("sqlite3 %q: %s", query, res)
	}
	return strings.TrimSpace(res.stdout)
}

// ---------- misc ----------

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		// A zombie still answers kill(0); read its state.
		b, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if rerr == nil {
			s := string(b)
			if i := strings.LastIndexByte(s, ')'); i > 0 && i+2 < len(s) && s[i+2] == 'Z' {
				return false
			}
		}
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// eventually polls cond once per second until it holds or timeout passes.
func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}
