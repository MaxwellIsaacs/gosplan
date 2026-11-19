package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/spec"
)

func TestCommandWords(t *testing.T) {
	for _, c := range []struct {
		shell string
		want  []string
	}{
		{"go test ./...", []string{"go"}},
		{"nix shell nixpkgs#go -c bash -c 'gofmt -l . ; go vet ./... && go test ./...'", []string{"nix"}},
		{"if [ -e flake.nix ]; then nix flake check; fi", []string{"nix"}},
		{"! gofmt -l . | tee /dev/stderr | grep -q . && CGO_ENABLED=0 go build ./cmd/...", []string{"gofmt", "tee", "grep", "go"}},
		{"pytest -q 2>&1 >/dev/null", []string{"pytest"}},
		{"cd web && npm test", []string{"npm"}},
		{"FOO=1 BAR=x make check", []string{"make"}},
		{`sh -c "echo hi; exit 0"`, []string{"sh"}},
		{"./scripts/check.sh", []string{"./scripts/check.sh"}},
	} {
		if got := commandWords(c.shell); !reflect.DeepEqual(got, c.want) {
			t.Errorf("commandWords(%q) = %v, want %v", c.shell, got, c.want)
		}
	}
}

func TestApplyEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/u")
	opt := Options{Claude: "claude", GosplanBin: "/self"}
	applyEnv(&opt, EnvConfig{Path: "$HOME/bin:$PATH:$UNSET", Claude: "/opt/claude", Gosplan: "/opt/gosplan"})
	if got := os.Getenv("PATH"); got != "/home/u/bin:/usr/bin:$UNSET" {
		t.Errorf("PATH = %q", got)
	}
	if opt.Claude != "/opt/claude" || opt.GosplanBin != "/opt/gosplan" {
		t.Errorf("binaries = %q %q", opt.Claude, opt.GosplanBin)
	}
	opt = Options{Claude: "/flag/claude", ClaudeExplicit: true, GosplanBin: "/flag/gosplan", GosplanBinExplicit: true}
	applyEnv(&opt, EnvConfig{Claude: "/opt/claude", Gosplan: "/opt/gosplan"})
	if opt.Claude != "/flag/claude" || opt.GosplanBin != "/flag/gosplan" {
		t.Errorf("explicit flags were overridden: %q %q", opt.Claude, opt.GosplanBin)
	}
}

// TestPreflightFindsMissingPieces: a claude without gosplan's flags, a gate
// command that is not installed and a disk floor above what is free are
// each one problem line.
func TestPreflightFindsMissingPieces(t *testing.T) {
	bin := t.TempDir()
	oldClaude := filepath.Join(bin, "claude")
	os.WriteFile(oldClaude, []byte("#!/bin/sh\ncase $1 in --version) echo '0.2.109 (Claude Code)';; *) echo 'Usage: claude [options]'; echo '  -p, --print'; echo '  --model <m>';; esac\n"), 0o755)
	project := t.TempDir()
	gitInit(t, project, map[string]string{"nightrunner.toml": "[verify]\ntest = \"surely-not-installed-xyz run\"\nlint = \"sh -c true\"\n"})
	opt := Options{Claude: oldClaude, Project: project, StateDir: t.TempDir(), MinFreeDisk: 1 << 40}
	problems, _ := Preflight(context.Background(), opt, "HEAD")
	all := strings.Join(problems, "\n")
	for _, want := range []string{"does not list", "--permission-mode", `"surely-not-installed-xyz" not found`, "under --min-free-disk"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems lack %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, `"sh"`) {
		t.Errorf("sh reported missing:\n%s", all)
	}
}

func TestFilterSpec(t *testing.T) {
	sp := &spec.Spec{Dir: "/s", ContentSHA256: "full", Tasks: []spec.Task{
		{ID: "1.a", Tier: 1},
		{ID: "2.a", Tier: 2, Deps: []string{"1.a"}},
		{ID: "2.b", Tier: 2, Deps: []string{"1.a"}},
		{ID: "3.a", Tier: 3, Deps: []string{"2.a", "2.b"}},
	}}
	passed := map[string]bool{"1.a": true, "2.a": true}
	got, err := filterSpec(sp, []string{"3.a", "2.b"}, "R1", passed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 2 || got.Tasks[0].ID != "2.b" || got.Tasks[1].ID != "3.a" {
		t.Fatalf("tasks = %+v", got.Tasks)
	}
	if len(got.Tasks[0].Deps) != 0 || !reflect.DeepEqual(got.Tasks[1].Deps, []string{"2.b"}) {
		t.Errorf("deps = %v, %v; want [] and [2.b] (1.a and 2.a are in the base)", got.Tasks[0].Deps, got.Tasks[1].Deps)
	}
	if got.ContentSHA256 == "full" || len(sp.Tasks) != 4 {
		t.Error("filterSpec changed the input or kept the full spec hash")
	}
	again, _ := filterSpec(sp, []string{"2.b", "3.a"}, "R1", passed)
	if again.ContentSHA256 != got.ContentSHA256 {
		t.Error("the hash depends on --only order; a resume would see a changed spec")
	}
	if _, err := filterSpec(sp, []string{"3.a"}, "R1", passed); err == nil || !strings.Contains(err.Error(), "add 2.b to --only") {
		t.Errorf("missing failed dep: err = %v", err)
	}
	if _, err := filterSpec(sp, []string{"9.z"}, "R1", passed); err == nil || !strings.Contains(err.Error(), "9.z") {
		t.Errorf("unknown task: err = %v", err)
	}
}

// gitInit makes a repo at dir with files in one commit.
func gitInit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
