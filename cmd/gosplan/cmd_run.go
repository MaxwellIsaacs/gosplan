package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/runner"
)

// onOff is a boolean flag that also accepts on/off (--usage-probe=off).
type onOff bool

func (b *onOff) String() string   { return strconv.FormatBool(bool(*b)) }
func (b *onOff) IsBoolFlag() bool { return true }
func (b *onOff) Set(v string) error {
	switch v {
	case "on":
		*b = true
		return nil
	case "off":
		*b = false
		return nil
	}
	x, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("want true|false|on|off, got %q", v)
	}
	*b = onOff(x)
	return nil
}

// cmdRun implements `gosplan run`: exit 0 complete, 1 halted, 2 error or aborted.
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var o runner.Options
	fs.StringVar(&o.SpecDir, "spec", "", "spec directory (prompts/sequence.txt, prompts/<id>.md)")
	fs.StringVar(&o.Project, "project", "", "project git repo")
	fs.StringVar(&o.StateDir, "state-dir", "", "state directory (default $XDG_STATE_HOME/gosplan/<project>-<hash>)")
	d := runner.Defaults
	fs.StringVar(&o.BaseRef, "base", d.BaseRef, "base ref of a new run, resolved once to a full SHA")
	fs.StringVar(&o.RunID, "run", "", "resume this run id")
	fs.IntVar(&o.MaxParallel, "max-parallel", d.MaxParallel, "most tasks running or verifying at once")
	fs.IntVar(&o.MaxAttempts, "max-attempts", d.MaxAttempts, "attempts per task")
	fs.DurationVar(&o.StallTimeout, "stall-timeout", d.StallTimeout, "kill a worker silent for this long")
	fs.DurationVar(&o.RetryBase, "retry-base", d.RetryBase, "retry backoff base: min(base*2^(n-1), 5m)")
	fs.StringVar(&o.Claude, "claude", d.Claude, "claude binary")
	fs.StringVar(&o.Model, "model", d.Model, "model for attempt 1")
	fs.StringVar(&o.RetryModel, "retry-model", d.RetryModel, "model for later attempts")
	fs.IntVar(&o.MaxTurns, "max-turns", d.MaxTurns, "claude --max-turns")
	fs.StringVar(&o.GosplanBin, "gosplan-bin", "", "gosplan binary used for `gosplan verify` (default: this binary)")
	fs.DurationVar(&o.VerifyTimeout, "verify-timeout", d.VerifyTimeout, "stop one `gosplan verify` after this long (could not verify)")
	fs.StringVar(&o.AttestKey, "attest-key", "", "ed25519 private key to sign verdicts (default: unsigned)")
	fs.BoolVar(&o.RequireAttest, "require-attest", false, "fail before any task runs when no key is usable")
	fs.StringVar(&o.Mutation, "mutation", d.Mutation, "advisory|off: gremlins after each pass (never decides)")
	fs.StringVar(&o.Gremlins, "gremlins", d.Gremlins, "gremlins binary")
	probe := onOff(true)
	fs.Var(&probe, "usage-probe", "run the $0 `claude -p /usage` probe when windows are stale (true|false)")
	dry := fs.Bool("dry-run", false, "validate the spec, print tasks and ready order; touch nothing")
	budgetFlags(fs, &o)
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "gosplan run: unexpected arguments %v\n", fs.Args())
		return exitError
	}
	if *dry {
		if o.SpecDir == "" {
			fmt.Fprintln(os.Stderr, "gosplan run: --spec is required")
			return exitError
		}
		if err := runner.DryRun(o.SpecDir, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "gosplan run:", err)
			return exitError
		}
		return exitPass
	}
	o.UsageProbe = bool(probe)
	o.GosplanVersion = version
	o.Stdout, o.Stderr = os.Stdout, os.Stderr

	// The first SIGINT/SIGTERM aborts the run (bounded: workers killed, at
	// most driver.ExitGrace + reconcile.AbortWait of waiting, then the DB
	// writes). After it, signals get their default action again, so a
	// second one ends gosplan run at once.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)
	out, err := runner.Run(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan run:", err)
		if out.Status != core.RunHalted {
			return exitError
		}
		// Halted, and then the bookkeeping (log flush, report) failed too,
		// e.g. a read-only state dir: still report the halt, exit 1.
	}
	status := out.Status
	if out.Reason != "" && out.Status == core.RunHalted {
		// The reason can be long (it names every task); the report has it all.
		status += " (" + out.Reason + ")"
	}
	fmt.Fprintf(os.Stderr, "gosplan run: run %s %s; report %s\n", out.Run, status, out.ReportPath)
	switch out.Status {
	case core.RunComplete:
		return exitPass
	case core.RunHalted:
		return exitFail
	}
	return exitError
}

// budgetHelp is the claim every budget flag's help carries.
const budgetHelp = "--max-budget-usd is a client-side list-price estimate; it does not cap plan usage. " +
	"Token and USD numbers are estimates and every budget default is policy, not measurement."

// budgetFlags registers the budget flags of gosplan run on fs.
func budgetFlags(fs *flag.FlagSet, o *runner.Options) {
	d := runner.Defaults
	fs.Func("budget", "budget gate mode off|observe|enforce (default: budget.mode of the config file; observe when there is none). "+budgetHelp,
		func(v string) error {
			switch v {
			case "off", "observe", "enforce":
				o.Budget = v
				return nil
			}
			return fmt.Errorf("want off, observe or enforce, got %q", v)
		})
	fs.StringVar(&o.BudgetConfig, "budget-config", "", "budget config TOML (default <state>/budget.toml); a file must set budget.mode. Budget defaults are policy")
	fs.Func("deadline", "no attempt starts after TIME and live workers stop at it (RFC 3339, or HH:MM = the next such local time); enforced in observe mode too",
		func(v string) error {
			t, err := parseDeadline(v, time.Now())
			if err != nil {
				return err
			}
			o.Deadline = &t
			return nil
		})
	fs.Func("cap-window", "POOL=FRACTION: this run may use at most FRACTION (0,1] of the pool's window (repeatable; attributable use is an estimate); reaching it drains the run",
		func(v string) error {
			pool, frac, ok := strings.Cut(v, "=")
			if !ok || strings.TrimSpace(pool) == "" {
				return fmt.Errorf("want POOL=FRACTION, got %q", v)
			}
			f, err := strconv.ParseFloat(frac, 64)
			if err != nil || !(f > 0 && f <= 1) {
				return fmt.Errorf("%s: the fraction must be a number in (0,1], got %q", pool, frac)
			}
			if o.CapWindow == nil {
				o.CapWindow = map[string]float64{}
			}
			o.CapWindow[strings.TrimSpace(pool)] = f
			return nil
		})
	fs.Func("cap-usd-estimate", "drain the run once its client-side USD estimate (settled cost estimates + open belts) reaches X; an estimate, not plan usage",
		func(v string) error {
			x, err := strconv.ParseFloat(v, 64)
			if err != nil || !(x > 0) {
				return fmt.Errorf("want a number > 0, got %q", v)
			}
			o.CapUSD = &x
			return nil
		})
	fs.IntVar(&o.MaxVerifyErrors, "max-verify-errors", d.MaxVerifyErrors, "halt the run verifier_error after N consecutive could-not-verify results (policy; every mode)")
	fs.DurationVar(&o.StallQuietMax, "stall-quiet-max", d.StallQuietMax, "cap on the silence one api_retry line may buy beyond --stall-timeout (policy; every mode)")
}

// parseDeadline reads --deadline: RFC 3339, or HH:MM meaning the next such
// local time after now (today if still ahead, else tomorrow).
func parseDeadline(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	h, m, ok := strings.Cut(v, ":")
	if ok && len(h) == 2 && len(m) == 2 {
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if err1 == nil && err2 == nil && hh >= 0 && hh <= 23 && mm >= 0 && mm <= 59 {
			now = now.Local()
			t := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, time.Local)
			if !t.After(now) {
				t = t.AddDate(0, 0, 1)
			}
			return t, nil
		}
	}
	return time.Time{}, errors.New("want RFC 3339 (2026-11-05T23:00:00Z) or HH:MM (00:00-23:59), got " + strconv.Quote(v))
}
