package mutation

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a repo with a base and a head commit.
func newRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	sh(t, dir, "git", "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1\n"), 0o644)
	sh(t, dir, "git", "add", "a.txt")
	sh(t, dir, "git", "commit", "-qm", "base")
	base = sh(t, dir, "git", "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("2\n"), 0o644)
	sh(t, dir, "git", "commit", "-qam", "head")
	head = sh(t, dir, "git", "rev-parse", "HEAD")
	return
}

// fakeGremlins writes a script that records argv and cwd to rec, copies canned
// to the -o path (when non-empty), and exits with $FAKE_GREMLINS_EXIT.
func fakeGremlins(t *testing.T, canned string) (bin, rec string) {
	t.Helper()
	d := t.TempDir()
	bin = filepath.Join(d, "gremlins")
	rec = filepath.Join(d, "rec")
	script := `#!/bin/sh
pwd > "` + rec + `.cwd"
git rev-parse HEAD > "` + rec + `.head" 2>/dev/null
echo "$@" > "` + rec + `.argv"
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; fi
  shift
done
if [ -n "$FAKE_GREMLINS_SLEEP" ]; then sleep "$FAKE_GREMLINS_SLEEP"; fi
if [ -n "` + canned + `" ]; then cp "` + canned + `" "$out"; fi
echo "boom on stderr" >&2
exit ${FAKE_GREMLINS_EXIT:-0}
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func canned(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMutationArgvNoThresholdFlag(t *testing.T) {
	argv := Argv(Options{BaseSHA: "abc", ThresholdEfficacy: 80}, "/c.yaml", "/o.json")
	joined := " " + strings.Join(argv, " ") + " "
	for _, want := range []string{" --diff abc ", " --config /c.yaml ", " --workers 2 ", " --timeout-coefficient 60 ", " -o /o.json "} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
	if argv[0] != "unleash" || argv[len(argv)-1] != "." {
		t.Errorf("argv shape: %v", argv)
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "--threshold") {
			t.Errorf("threshold flag passed: %q", a)
		}
	}
}

func TestMutationConfigYAMLThreshold(t *testing.T) {
	y := ConfigYAML(Options{ThresholdEfficacy: 80})
	re := regexp.MustCompile(`(?m)^unleash:\n  threshold:\n    efficacy: 80\n`)
	if !re.MatchString(y) {
		t.Errorf("yaml:\n%s", y)
	}
	if strings.Contains(ConfigYAML(Options{}), "efficacy") {
		t.Error("no threshold requested but yaml has efficacy")
	}
}

func TestMutationParseCounts(t *testing.T) {
	b, err := os.ReadFile(canned(t, "counts.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseOutput(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Killed != 7 || r.Lived != 2 || r.TimedOut != 3 || r.NotCovered != 1 || r.NotViable != 0 {
		t.Errorf("counts: %+v", r)
	}
	if r.Efficacy == nil || math.Abs(*r.Efficacy-7.0/9.0*100) > 1e-9 {
		t.Errorf("efficacy: %v", r.Efficacy)
	}
	if len(r.LivedMutants) != 2 {
		t.Fatalf("lived mutants: %+v", r.LivedMutants)
	}
	if m := r.LivedMutants[0]; m.File != "a.go" || m.Line != 14 {
		t.Errorf("first lived: %+v", m)
	}
	if m := r.LivedMutants[1]; m.File != "b/b.go" || m.Line != 20 {
		t.Errorf("second lived: %+v", m)
	}
	if !r.Advisory {
		t.Error("advisory must be true")
	}
}

func TestMutationTooFewMutants(t *testing.T) {
	b, _ := os.ReadFile(canned(t, "few.json"))
	r, err := ParseOutput(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Efficacy != nil || r.Note != "too few mutants" {
		t.Errorf("efficacy=%v note=%q", r.Efficacy, r.Note)
	}
	if r.Killed != 2 || r.Lived != 1 {
		t.Errorf("counts: %+v", r)
	}
}

func opts(t *testing.T, bin string) Options {
	proj, base, head := newRepo(t)
	return Options{Project: proj, BaseSHA: base, HeadSHA: head, Gremlins: bin, WorkDir: t.TempDir(),
		OutPath: filepath.Join(t.TempDir(), "out", "m.json"), Timeout: 30 * time.Second}
}

func TestMutationBelowThresholdIsAdvisory(t *testing.T) {
	bin, _ := fakeGremlins(t, canned(t, "counts.json"))
	t.Setenv("FAKE_GREMLINS_EXIT", "10")
	r := Run(context.Background(), opts(t, bin))
	if r.Status != "below_threshold" || !r.Advisory {
		t.Errorf("status=%q advisory=%v note=%q", r.Status, r.Advisory, r.Note)
	}
	if r.Killed != 7 || r.Lived != 2 || r.Efficacy == nil {
		t.Errorf("counts not parsed: %+v", r)
	}
}

func TestMutationRunOK(t *testing.T) {
	bin, rec := fakeGremlins(t, canned(t, "counts.json"))
	o := opts(t, bin)
	r := Run(context.Background(), o)
	if r.Status != "ok" || r.Killed != 7 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(o.OutPath); err != nil {
		t.Errorf("OutPath not kept: %v", err)
	}
	argv, _ := os.ReadFile(rec + ".argv")
	if !strings.Contains(string(argv), "--diff "+o.BaseSHA) || strings.Contains(string(argv), "--threshold") {
		t.Errorf("argv: %s", argv)
	}
	if len(r.Argv) == 0 {
		t.Error("report argv empty")
	}
}

func TestMutationUnavailable(t *testing.T) {
	o := opts(t, filepath.Join(t.TempDir(), "nope"))
	r := Run(context.Background(), o)
	if r.Status != "unavailable" || !r.Advisory {
		t.Errorf("%+v", r)
	}
}

func TestMutationErrorFolded(t *testing.T) {
	bin, _ := fakeGremlins(t, "")
	t.Setenv("FAKE_GREMLINS_EXIT", "1")
	r := Run(context.Background(), opts(t, bin))
	if r.Status != "error" || !strings.Contains(r.Note, "boom on stderr") {
		t.Errorf("%+v", r)
	}
}

func TestMutationTimeout(t *testing.T) {
	bin, _ := fakeGremlins(t, canned(t, "counts.json"))
	t.Setenv("FAKE_GREMLINS_SLEEP", "30")
	o := opts(t, bin)
	o.Timeout = 300 * time.Millisecond
	start := time.Now()
	r := Run(context.Background(), o)
	if r.Status != "timeout" {
		t.Errorf("status=%q note=%q", r.Status, r.Note)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
}

func TestMutationRunsInDetachedWorktree(t *testing.T) {
	bin, rec := fakeGremlins(t, canned(t, "counts.json"))
	o := opts(t, bin)
	if r := Run(context.Background(), o); r.Status != "ok" {
		t.Fatalf("%+v", r)
	}
	cwd, _ := os.ReadFile(rec + ".cwd")
	head, _ := os.ReadFile(rec + ".head")
	if c := strings.TrimSpace(string(cwd)); c == "" || c == o.Project {
		t.Errorf("cwd %q is the project dir or empty", c)
	} else if _, err := os.Stat(c); !os.IsNotExist(err) {
		t.Errorf("worktree dir %s still exists", c)
	}
	if h := strings.TrimSpace(string(head)); h != o.HeadSHA {
		t.Errorf("worktree HEAD %q, want %q", h, o.HeadSHA)
	}
	list := sh(t, o.Project, "git", "worktree", "list", "--porcelain")
	if n := strings.Count(list, "worktree "); n != 1 {
		t.Errorf("worktrees left: %s", list)
	}
}
