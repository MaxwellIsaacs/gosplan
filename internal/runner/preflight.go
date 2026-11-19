package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/MaxwellIsaacs/gosplan/internal/gitx"
	"github.com/MaxwellIsaacs/gosplan/internal/verify"
	"github.com/MaxwellIsaacs/gosplan/internal/wt"
)

// DefaultMinFreeDisk is the free-space floor: below it a running run halts
// disk_low and preflight refuses to start; under twice it preflight warns. Policy.
const DefaultMinFreeDisk int64 = 3 << 30

// EnvConfig is the [env] table of nightrunner.toml, read from the run base
// like the rest of the config, so a tmux or cron PATH cannot change what a
// run sees:
//
//	[env]
//	path = "/run/current-system/sw/bin:$PATH"  # PATH for workers, verify and gates
//	claude = "/home/me/.local/bin/claude"      # worker binary, unless --claude is given
//	gosplan = "/home/me/.local/bin/gosplan"    # verify binary, unless --gosplan-bin is given
//
// In path, $PATH and $HOME expand to the launching environment's values;
// nothing else expands.
type EnvConfig struct {
	Path    string `toml:"path"`
	Claude  string `toml:"claude"`
	Gosplan string `toml:"gosplan"`
}

// LoadEnvConfig reads [env] from nightrunner.toml at base. A missing file or
// table is an empty config (the verifier reports a missing config itself).
func LoadEnvConfig(project, base string) (EnvConfig, error) {
	src, ok, err := gitx.Repo{Dir: project}.ShowFile(base, verify.ConfigPath)
	if err != nil || !ok {
		return EnvConfig{}, err
	}
	var c struct {
		Env EnvConfig `toml:"env"`
	}
	if _, err := toml.Decode(src, &c); err != nil {
		return EnvConfig{}, fmt.Errorf("%s at %s: %w", verify.ConfigPath, short(base), err)
	}
	return c.Env, nil
}

// applyEnv pins PATH and fills the claude and gosplan binaries from [env].
// Explicit flags win. It changes this process's PATH, which every worker,
// `gosplan verify` and gate command inherits.
func applyEnv(opt *Options, e EnvConfig) []string {
	var notes []string
	if e.Path != "" {
		p := os.Expand(e.Path, func(k string) string {
			switch k {
			case "PATH", "HOME":
				return os.Getenv(k)
			}
			return "$" + k
		})
		os.Setenv("PATH", p)
		notes = append(notes, "PATH pinned by [env] path")
	}
	if e.Claude != "" && !opt.ClaudeExplicit {
		opt.Claude = e.Claude
		notes = append(notes, "claude = "+e.Claude+" ([env])")
	}
	if e.Gosplan != "" && !opt.GosplanBinExplicit {
		opt.GosplanBin = e.Gosplan
		notes = append(notes, "gosplan = "+e.Gosplan+" ([env])")
	}
	return notes
}

// claudeVersion is how `claude --version` identifies Claude Code.
var claudeVersion = regexp.MustCompile(`(?m)^\d+\.\d+\.\d+.*\(Claude Code\)`)

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// workerFlags are the claude flags gosplan passes that `claude --help`
// lists. --max-turns is passed too but is not in claude's help text.
var workerFlags = []string{"-p", "--output-format", "--verbose", "--permission-mode", "--session-id", "--model"}

// Preflight checks, in a few seconds, what otherwise surfaces hours later as
// failed attempts: the claude binary and its flags, every verify gate's
// commands on PATH, and free disk (refused under the floor, warned under twice it). It returns one line per problem, plus
// notes for the operator.
func Preflight(ctx context.Context, opt Options, base string) (problems, notes []string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// claude. It runs in a scratch dir with only PATH and HOME, so a wrapper
	// or test fake that logs its launches sees nothing of this run.
	if path, err := exec.LookPath(opt.Claude); err != nil {
		problems = append(problems, fmt.Sprintf("claude: %q not found on PATH", opt.Claude))
	} else {
		scratch, _ := os.MkdirTemp("", "gosplan-preflight-*")
		defer os.RemoveAll(scratch)
		probe := func(arg string) string {
			cmd := exec.CommandContext(ctx, path, arg)
			cmd.Dir, cmd.Env = scratch, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
			out, _ := cmd.CombinedOutput()
			return string(out)
		}
		v := strings.TrimSpace(probe("--version"))
		if !claudeVersion.MatchString(v) {
			notes = append(notes, fmt.Sprintf("claude: %s is not Claude Code by its --version; flag check skipped", path))
		} else {
			notes = append(notes, "claude "+firstLine(v)+" ("+path+")")
			help := probe("--help")
			var missing []string
			for _, f := range workerFlags {
				if !strings.Contains(help, f) {
					missing = append(missing, f)
				}
			}
			if len(missing) > 0 {
				problems = append(problems, fmt.Sprintf("claude: %s (%s) does not list %s; it is too old for gosplan", path, firstLine(v), strings.Join(missing, " ")))
			}
		}
	}

	// verify gates
	if cfg, err := verify.LoadConfig(gitx.Repo{Dir: opt.Project}, base); err != nil {
		problems = append(problems, "config: "+err.Error())
	} else {
		var checked []string
		seen := map[string]bool{}
		for _, c := range cfg.Commands() {
			for _, w := range commandWords(c.Shell) {
				if seen[w] {
					continue
				}
				seen[w] = true
				if strings.Contains(w, "/") && !filepath.IsAbs(w) {
					continue // relative to the export (./scripts/x): exists only there
				}
				if _, err := exec.LookPath(w); err != nil {
					problems = append(problems, fmt.Sprintf("gate %s: %q not found on PATH (it would exit 127 in every verify)", c.Name, w))
					continue
				}
				checked = append(checked, w)
			}
		}
		if len(checked) > 0 {
			notes = append(notes, "gate commands on PATH: "+strings.Join(checked, ", "))
		}
	}

	// disk
	floor := opt.MinFreeDisk
	for _, dir := range []string{opt.StateDir, opt.Project} {
		free, at, err := FreeDisk(dir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("disk: %s: %v", dir, err))
			continue
		}
		switch {
		case free < floor:
			problems = append(problems, fmt.Sprintf("disk: %s free on %s, under --min-free-disk %s: the run would halt disk_low at once",
				gib(free), at, gib(floor)))
		case free/2 < floor: // under twice the floor, without overflow
			notes = append(notes, fmt.Sprintf("disk: WARNING %s free on %s, under twice --min-free-disk %s: the run may halt disk_low",
				gib(free), at, gib(floor)))
		default:
			notes = append(notes, fmt.Sprintf("disk: %s free on %s", gib(free), at))
		}
	}
	return problems, notes
}

// shellWords are words that start a shell command but are not programs.
var shellWords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true, "do": true, "done": true,
	"while": true, "until": true, "for": true, "case": true, "esac": true, "in": true,
	"!": true, "{": true, "}": true, "(": true, ")": true, "time": true,
	"[": true, "[[": true, "test": true, "echo": true, "printf": true, "cd": true, "export": true,
	"true": true, "false": true, ":": true, "exit": true, "set": true, "unset": true,
	"command": true, "eval": true, "exec": true, "source": true, ".": true, "read": true,
	"shift": true, "trap": true, "wait": true, "type": true, "ulimit": true, "umask": true,
	"local": true, "return": true,
}

// commandWords returns the program each top-level simple command of a shell
// string runs: the first word after a separator (; && || | newline) or a
// keyword, skipping VAR=value assignments and builtins. Words inside quotes
// are never split out, so `nix shell ... -c bash -c 'go test'` yields nix
// and bash, not go: go exists only inside the nix shell.
func commandWords(s string) []string {
	var segs [][]string
	var cur []string
	var word strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			cur = append(cur, word.String())
			word.Reset()
			inWord = false
		}
	}
	endSeg := func() {
		flush()
		if len(cur) > 0 {
			segs = append(segs, cur)
		}
		cur = nil
	}
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				word.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(rs):
			i++
			word.WriteRune(rs[i])
			inWord = true
		case c == '>' || c == '<':
			flush() // a redirection: 2>&1, >/dev/null, <in; its target is not a command
			if i+1 < len(rs) && (rs[i+1] == '&' || rs[i+1] == '>') {
				i++
			}
			for i+1 < len(rs) && (rs[i+1] == ' ' || rs[i+1] == '\t') {
				i++
			}
			for i+1 < len(rs) && !strings.ContainsRune(" \t;|&\n", rs[i+1]) {
				i++
			}
		case c == ';' || c == '\n' || c == '|' || c == '&':
			endSeg()
		case c == ' ' || c == '\t':
			flush()
		case c == '(' || c == ')':
			flush()
			cur = append(cur, string(c))
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	endSeg()
	var out []string
	for _, seg := range segs {
		for _, w := range seg {
			if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.ContainsAny(w[:strings.Index(w, "=")], "/.-") {
				continue // VAR=value
			}
			if w == "then" || w == "do" || w == "else" || w == "elif" || w == "!" || w == "(" || w == "time" || w == "{" {
				continue
			}
			if !shellWords[w] {
				out = append(out, w)
			}
			break
		}
	}
	return out
}

// FreeDisk returns the bytes available to this user on the filesystem
// holding dir, walking up to the nearest existing parent (a state dir may
// not exist yet), and the directory it measured.
func FreeDisk(dir string) (int64, string, error) {
	d := dir
	for {
		if _, err := os.Stat(d); err == nil {
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			return 0, dir, errors.New("no existing parent directory")
		}
		d = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(d, &st); err != nil {
		return 0, d, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), d, nil
}

func gib(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/(1<<30)) }

// PreflightError lists everything preflight found wrong.
type PreflightError struct{ Problems []string }

func (e *PreflightError) Error() string {
	return "preflight failed (fix these, or --skip-preflight):\n  " + strings.Join(e.Problems, "\n  ")
}

// prepareEnv applies [env] from the config at base, then runs the
// preflight. It runs before anything is written for a new run, and before
// a resume takes the run lock.
func prepareEnv(ctx context.Context, opt *Options, base string) error {
	e, err := LoadEnvConfig(opt.Project, base)
	if err != nil {
		return err
	}
	for _, n := range applyEnv(opt, e) {
		fmt.Fprintf(opt.Stderr, "gosplan run: env: %s\n", n)
	}
	if opt.SkipPreflight {
		fmt.Fprintln(opt.Stderr, "gosplan run: preflight skipped (--skip-preflight)")
		return nil
	}
	problems, notes := Preflight(ctx, *opt, base)
	for _, n := range notes {
		fmt.Fprintf(opt.Stderr, "gosplan run: preflight: %s\n", n)
	}
	if len(problems) > 0 {
		return &PreflightError{Problems: problems}
	}
	return nil
}

// DryRunPreflight prints the [env] settings and preflight results for a new
// run at opt.BaseRef to w. It writes nothing, and problems are printed, not
// returned: a dry-run's exit status is about the spec.
func DryRunPreflight(ctx context.Context, opt Options, w io.Writer) error {
	opt, err := opt.withDefaults()
	if err != nil {
		return err
	}
	base, err := (wt.Git{}).ResolveCommit(ctx, opt.Project, opt.BaseRef)
	if err != nil {
		fmt.Fprintf(opt.Stderr, "gosplan run: dry-run: preflight skipped: %v\n", err)
		return nil
	}
	e, err := LoadEnvConfig(opt.Project, base)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "preflight at %s (%s):\n", opt.BaseRef, short(base))
	for _, n := range applyEnv(&opt, e) {
		fmt.Fprintf(w, "  env: %s\n", n)
	}
	problems, notes := Preflight(ctx, opt, base)
	for _, n := range notes {
		fmt.Fprintf(w, "  ok: %s\n", n)
	}
	for _, p := range problems {
		fmt.Fprintf(w, "  FAIL: %s\n", p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(w, "  %d preflight problem(s): gosplan run would refuse to start\n", len(problems))
	}
	return nil
}
