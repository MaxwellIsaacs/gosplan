// Package mutation runs gremlins mutation testing on the lines a task changed
// and summarizes the result. The result is an advisory signal about test
// strength on changed lines: it never fails a task, never blocks a dependent,
// and never enters the signed verdict. gremlins is an external binary, not a
// module dependency (pinned: github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0).
package mutation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/wt"
)

// Report statuses.
const (
	StatusOK             = "ok"
	StatusBelowThreshold = "below_threshold"
	StatusUnavailable    = "unavailable"
	StatusError          = "error"
	StatusTimeout        = "timeout"
)

// minMutants is the least killed+lived needed to report an efficacy.
const minMutants = 5

// exitBelowThreshold is gremlins' exit code when efficacy is under the
// threshold from the config file.
const exitBelowThreshold = 10

// Options configures Run.
type Options struct {
	Project           string
	BaseSHA, HeadSHA  string        // full SHAs
	Gremlins          string        // binary (runner.Defaults has gosplan run's)
	Workers           int           // default 2
	TimeoutCoeff      int           // default 60
	ThresholdEfficacy float64       // 0 = none; written to the YAML only
	Timeout           time.Duration // whole run; default 20m
	WorkDir           string        // parent for the detached worktree and temp files
	OutPath           string        // where gremlins' JSON is kept
}

// Mutant is one surviving mutant.
type Mutant struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Type   string `json:"type"`
}

// Report is the advisory result. Advisory is always true.
type Report struct {
	Advisory     bool     `json:"advisory"`
	Status       string   `json:"status"`
	Killed       int      `json:"killed"`
	Lived        int      `json:"lived"`
	TimedOut     int      `json:"timed_out"` // inconclusive, never counted as killed
	NotCovered   int      `json:"not_covered"`
	NotViable    int      `json:"not_viable"`
	Efficacy     *float64 `json:"efficacy"` // percent; nil when killed+lived < 5
	LivedMutants []Mutant `json:"lived_mutants"`
	Note         string   `json:"note,omitempty"`
	ElapsedSec   float64  `json:"elapsed_sec"`
	Argv         []string `json:"argv"`
}

func (o Options) withDefaults() Options {
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.TimeoutCoeff <= 0 {
		o.TimeoutCoeff = 60
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Minute
	}
	return o
}

// Argv builds the gremlins argv (without the binary name).
//
// Thresholds are deliberately absent: gremlins v0.6.0 ignores
// --threshold-efficacy and --threshold-mcover given on the command line
// (exit 0 at 0% efficacy). They only work from the --config YAML.
func Argv(opt Options, cfgPath, outPath string) []string {
	opt = opt.withDefaults()
	return []string{
		"unleash", "--diff", opt.BaseSHA, "--config", cfgPath,
		"--workers", strconv.Itoa(opt.Workers),
		"--timeout-coefficient", strconv.Itoa(opt.TimeoutCoeff),
		"-o", outPath, ".",
	}
}

// ConfigYAML renders the --config file. The efficacy threshold lives under
// unleash.threshold.efficacy; it is the only place gremlins honors it.
func ConfigYAML(opt Options) string {
	var b strings.Builder
	b.WriteString("unleash:\n")
	if opt.ThresholdEfficacy > 0 {
		b.WriteString("  threshold:\n")
		b.WriteString("    efficacy: " + strconv.FormatFloat(opt.ThresholdEfficacy, 'f', -1, 64) + "\n")
	} else {
		b.WriteString("  dry-run: false\n")
	}
	return b.String()
}

type gremlinsJSON struct {
	Killed     int     `json:"mutants_killed"`
	Lived      int     `json:"mutants_lived"`
	NotViable  int     `json:"mutants_not_viable"`
	NotCovered int     `json:"mutants_not_covered"`
	Elapsed    float64 `json:"elapsed_time"`
	Files      []struct {
		Name      string `json:"file_name"`
		Mutations []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Line   int    `json:"line"`
			Column int    `json:"column"`
		} `json:"mutations"`
	} `json:"files"`
}

// ParseOutput reads gremlins' JSON into a Report with Status "ok". Killed and
// Lived come from the top-level counts; TIMED OUT is counted from the files.
func ParseOutput(b []byte) (Report, error) {
	var g gremlinsJSON
	if err := json.Unmarshal(b, &g); err != nil {
		return Report{}, fmt.Errorf("parse gremlins output: %w", err)
	}
	r := Report{
		Advisory: true, Status: StatusOK,
		Killed: g.Killed, Lived: g.Lived,
		NotCovered: g.NotCovered, NotViable: g.NotViable,
		ElapsedSec:   g.Elapsed,
		LivedMutants: []Mutant{},
	}
	for _, f := range g.Files {
		for _, m := range f.Mutations {
			switch m.Status {
			case "TIMED OUT":
				r.TimedOut++
			case "LIVED":
				r.LivedMutants = append(r.LivedMutants, Mutant{File: f.Name, Line: m.Line, Column: m.Column, Type: m.Type})
			}
		}
	}
	if r.Killed+r.Lived >= minMutants {
		e := float64(r.Killed) / float64(r.Killed+r.Lived) * 100
		r.Efficacy = &e
	} else {
		r.Note = "too few mutants"
	}
	return r, nil
}

// Run executes gremlins against a detached worktree of HeadSHA. It never
// fails the caller: every problem is folded into Report.Status and Note.
func Run(ctx context.Context, opt Options) Report {
	opt = opt.withDefaults()
	start := time.Now()
	rep := run(ctx, opt)
	rep.Advisory = true
	if rep.ElapsedSec == 0 {
		rep.ElapsedSec = time.Since(start).Seconds()
	}
	return rep
}

func fail(status, format string, a ...any) Report {
	return Report{Advisory: true, Status: status, Note: fmt.Sprintf(format, a...), LivedMutants: []Mutant{}}
}

func run(ctx context.Context, opt Options) Report {
	bin, err := exec.LookPath(opt.Gremlins)
	if err != nil {
		return fail(StatusUnavailable, "gremlins not found: %v", err)
	}
	tmp, err := os.MkdirTemp(opt.WorkDir, "mutation-")
	if err != nil {
		return fail(StatusError, "temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	// The worktree is a child dir of tmp; config and output stay outside it.
	tree := filepath.Join(tmp, "tree")
	cfg := filepath.Join(tmp, "gremlins.yaml")
	out := opt.OutPath
	if out == "" {
		out = filepath.Join(tmp, "mutation.json")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return fail(StatusError, "output dir: %v", err)
	}
	if err := os.WriteFile(cfg, []byte(ConfigYAML(opt)), 0o644); err != nil {
		return fail(StatusError, "write config: %v", err)
	}

	g := wt.Git{}
	if err := g.AddDetached(ctx, opt.Project, opt.HeadSHA, tree); err != nil {
		return fail(StatusError, "add worktree: %v", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = g.RemoveDetached(cctx, opt.Project, tree)
	}()

	argv := Argv(opt, cfg, out)
	rctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	cmd := exec.Command(bin, argv...)
	cmd.Dir = tree
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return fail(StatusError, "start gremlins: %v", err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-rctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	werr := cmd.Wait()
	close(done)
	// Reap stragglers that outlived the leader.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	var rep Report
	switch {
	case rctx.Err() != nil:
		rep = fail(StatusTimeout, "gremlins exceeded %s", opt.Timeout)
	default:
		code := 0
		if werr != nil {
			var ee *exec.ExitError
			if !errors.As(werr, &ee) {
				return withArgv(fail(StatusError, "wait gremlins: %v", werr), argv)
			}
			code = ee.ExitCode()
		}
		if code != 0 && code != exitBelowThreshold {
			rep = fail(StatusError, "gremlins exited %d: %s", code, lastLines(stderr.String(), 5))
			break
		}
		b, rerr := os.ReadFile(out)
		if rerr != nil {
			rep = fail(StatusError, "no gremlins output: %v: %s", rerr, lastLines(stderr.String(), 5))
			break
		}
		rep, err = ParseOutput(b)
		if err != nil {
			rep = fail(StatusError, "%v", err)
			break
		}
		if code == exitBelowThreshold {
			rep.Status = StatusBelowThreshold
		}
	}
	return withArgv(rep, argv)
}

func withArgv(r Report, argv []string) Report {
	r.Argv = argv
	return r
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
