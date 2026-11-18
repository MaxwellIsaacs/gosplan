// Package runner is the assembly behind `gosplan run`: it imports a
// spec into the SQLite state, wires the reconciler to the real launcher
// (claude subprocesses), workspace (git worktrees), verifier (`gosplan verify` as
// a subprocess), window parsing, verdict signing and the advisory mutation
// run, then integrates the passed heads and writes the run report.
//
// The verdict comes only from `gosplan verify`. Mutation results and observed
// windows are recorded, never used to decide anything. Workers run
// unsandboxed as the same OS user; the attest key is kept out of their argv,
// prompt and environment, but a worker can still read the key file.
package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/attest"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetrt"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/driver"
	"github.com/MaxwellIsaacs/gosplan/internal/mutation"
	"github.com/MaxwellIsaacs/gosplan/internal/reconcile"
	"github.com/MaxwellIsaacs/gosplan/internal/report"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/verifyexec"
	"github.com/MaxwellIsaacs/gosplan/internal/window"
	"github.com/MaxwellIsaacs/gosplan/internal/wt"
)

// Mutation modes.
const (
	MutationAdvisory = "advisory"
	MutationOff      = "off"
)

// noteUnsigned is the report note when no attest key is in use.
const noteUnsigned = "Verdicts are unsigned: signing off until D7 (pass --attest-key PATH to opt in)."

// Options configures Run. Zero values take Defaults.
type Options struct {
	SpecDir, Project, StateDir string        // StateDir default state.DefaultDir(Project)
	BaseRef                    string        // default "HEAD"; resolved once to a full SHA
	RunID                      string        // resume this run if set; else a new id
	MaxParallel                int           // 3
	MaxAttempts                int           // 3
	StallTimeout               time.Duration // 5m
	RetryBase                  time.Duration // 10s: delay = min(RetryBase·2^(n-1), 5m)
	Claude                     string        // "claude"
	Model, RetryModel          string        // "sonnet", "opus"
	MaxTurns                   int           // 200
	GosplanBin                 string        // default os.Executable()
	VerifyTimeout              time.Duration // 2h
	AttestKey                  string        // "" = unsigned; a missing file = unsigned + warning
	RequireAttest              bool          // no usable key -> error before any task runs
	Mutation                   string        // "advisory" | "off"; default "advisory"
	Gremlins                   string        // "gremlins"
	UsageProbe                 bool          // run the $0 /usage probe when due
	GosplanVersion             string        // recorded in signed statements; default "dev"
	Stdout, Stderr             io.Writer     // default io.Discard

	// Budget gate. Budget is the --budget mode flag
	// ("" = from the config file, else observe); BudgetConfig the config
	// file (default <state>/budget.toml). Deadline, CapWindow and CapUSD are
	// run flags, enforced in observe and enforce mode, rejected with off.
	Budget       string
	BudgetConfig string
	Deadline     *time.Time
	CapWindow    map[string]float64
	CapUSD       *float64
	// MaxVerifyErrors and StallQuietMax are reconciler/driver rules, on in
	// every mode (0 = their policy defaults, 3 and 15m).
	MaxVerifyErrors int
	StallQuietMax   time.Duration

	budget *budgetSetup // resolved once by Run
	spec   *spec.Spec   // the spec in memory (sidecar budget keys)
}

// Outcome is how a run ended. Reason is the halt reason (see
// docs/gosplan-run.md, "Halt reasons").
type Outcome struct{ Run, Status, Reason, ReportPath, IntegrationBranch, IntegrationSHA string }

// Defaults are gosplan run's defaults, in one place: cmd/gosplan shows them as flag
// defaults and withDefaults fills any zero field from them.
var Defaults = Options{
	BaseRef:        "HEAD",
	MaxParallel:    3,
	MaxAttempts:    3,
	StallTimeout:   core.StallTimeout,
	RetryBase:      core.BaseRetryDelay,
	Claude:         "claude",
	Model:          "sonnet",
	RetryModel:     "opus",
	MaxTurns:       200,
	VerifyTimeout:  2 * time.Hour,
	Mutation:       MutationAdvisory,
	Gremlins:       "gremlins",
	GosplanVersion: "dev",

	MaxVerifyErrors: 3,                           // policy
	StallQuietMax:   driver.DefaultStallQuietMax, // policy
}

func (o Options) withDefaults() (Options, error) {
	d := Defaults
	str := func(v *string, def string) {
		if *v == "" {
			*v = def
		}
	}
	num := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	dur := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	str(&o.BaseRef, d.BaseRef)
	num(&o.MaxParallel, d.MaxParallel)
	num(&o.MaxAttempts, d.MaxAttempts)
	dur(&o.StallTimeout, d.StallTimeout)
	dur(&o.RetryBase, d.RetryBase)
	str(&o.Claude, d.Claude)
	str(&o.Model, d.Model)
	str(&o.RetryModel, d.RetryModel)
	num(&o.MaxTurns, d.MaxTurns)
	dur(&o.VerifyTimeout, d.VerifyTimeout)
	num(&o.MaxVerifyErrors, d.MaxVerifyErrors)
	dur(&o.StallQuietMax, d.StallQuietMax)
	str(&o.Mutation, d.Mutation)
	str(&o.Gremlins, d.Gremlins)
	str(&o.GosplanVersion, d.GosplanVersion)
	if o.Mutation != MutationAdvisory && o.Mutation != MutationOff {
		return o, fmt.Errorf("--mutation must be %q or %q, got %q", MutationAdvisory, MutationOff, o.Mutation)
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.SpecDir == "" || o.Project == "" {
		return o, errors.New("--spec and --project are required")
	}
	var err error
	if o.Project, err = filepath.Abs(o.Project); err != nil {
		return o, err
	}
	if o.SpecDir, err = filepath.Abs(o.SpecDir); err != nil {
		return o, err
	}
	if o.StateDir == "" {
		if o.StateDir, err = state.DefaultDir(o.Project); err != nil {
			return o, err
		}
	}
	if o.StateDir, err = filepath.Abs(o.StateDir); err != nil {
		return o, err
	}
	if o.GosplanBin == "" {
		if o.GosplanBin, err = os.Executable(); err != nil {
			return o, fmt.Errorf("locate gosplan binary: %w", err)
		}
	}
	return o, nil
}

// RetryDelay returns the backoff function min(base·2^(n-1), core.MaxRetryDelay).
// n < 1 is treated as 1.
func RetryDelay(base time.Duration) func(n int) time.Duration {
	return func(n int) time.Duration {
		if n < 1 {
			n = 1
		}
		d := base
		for i := 1; i < n; i++ {
			d *= 2
			if d >= core.MaxRetryDelay {
				return core.MaxRetryDelay
			}
		}
		if d > core.MaxRetryDelay {
			return core.MaxRetryDelay
		}
		return d
	}
}

// workerArgv is the claude worker argv (argv[0] is the binary).
func workerArgv(claude, prompt, sessionID, model string, maxTurns int) []string {
	return []string{claude, "-p", prompt,
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		"--session-id", sessionID,
		"--model", model,
		"--max-turns", strconv.Itoa(maxTurns)}
}

// secretEnv reports whether an environment variable must not reach a worker.
// GOSPLAN_RUN/GOSPLAN_TASK/GOSPLAN_ATTEMPT/GOSPLAN_ATTEMPT_NONCE are dropped too, so the
// per-attempt values the orchestrator adds are the only ones.
func secretEnv(name string) bool {
	switch name {
	case "GOSPLAN_RUN", "GOSPLAN_TASK", "GOSPLAN_ATTEMPT", driver.EnvNonce:
		return true
	}
	return strings.HasPrefix(name, "GOSPLAN_ATTEST") || strings.HasPrefix(name, "NIGHTRUNNER_")
}

// filterEnv returns base without any GOSPLAN_ATTEST* or NIGHTRUNNER_* variable
// (and without GOSPLAN_RUN, GOSPLAN_TASK, GOSPLAN_ATTEMPT).
func filterEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if secretEnv(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// workerEnv is filterEnv(base) plus GOSPLAN_RUN, GOSPLAN_TASK and GOSPLAN_ATTEMPT.
func workerEnv(base []string, run, task string, attempt int) []string {
	return append(filterEnv(base), "GOSPLAN_RUN="+run, "GOSPLAN_TASK="+task, "GOSPLAN_ATTEMPT="+strconv.Itoa(attempt))
}

// newRunID returns a run id in the events schema form YYYYMMDD-HHMMSS-<rand4>.
func newRunID(now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// attemptLauncher wraps the real launcher. It records each attempt's argv
// next to its stream file (<task>.<n>.argv.json) before launching. The
// environment is not recorded.
type attemptLauncher struct {
	next core.Launcher
}

func (l attemptLauncher) Launch(ctx context.Context, req core.LaunchRequest) (core.Handle, error) {
	if b, err := json.Marshal(req.Argv); err == nil {
		_ = os.WriteFile(strings.TrimSuffix(req.StreamPath, ".stream.jsonl")+".argv.json", append(b, '\n'), 0o600)
	}
	return l.next.Launch(ctx, req)
}

// runDirs are the state directories of one run.
func runDirs(stateDir, run string) []string {
	rd := filepath.Join(stateDir, "runs", run)
	return []string{
		filepath.Join(stateDir, "worktrees", run),
		filepath.Join(rd, "attempts"),
		filepath.Join(rd, "verify"),
		filepath.Join(rd, "verify-logs"),
		filepath.Join(rd, "attest"),
		filepath.Join(rd, "mutation"),
	}
}

// lockRun takes an exclusive flock on <runDir>/lock for the life of this
// gosplan run, so a second `gosplan run --run ID` on the same run fails at once
// instead of stealing leases. The lock dies with the process (crash
// included). A worker, same uid, could take it too; that only stops the
// next resume.
func lockRun(runDir, run string, create bool) (func(), error) {
	path := filepath.Join(runDir, "lock")
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if !create && errors.Is(err, os.ErrNotExist) {
		return func() {}, nil // nobody has locked this run
	}
	if err != nil {
		return nil, fmt.Errorf("run lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("run %s is in use by another gosplan run (lock %s is held)", run, path)
		}
		return nil, fmt.Errorf("run lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// loadKey applies the --attest-key / --require-attest rules.
func loadKey(opt Options) (ed25519.PrivateKey, []string, error) {
	if opt.AttestKey == "" {
		if opt.RequireAttest {
			return nil, nil, errors.New("--require-attest needs --attest-key PATH")
		}
		return nil, []string{noteUnsigned}, nil
	}
	if _, err := os.Stat(opt.AttestKey); errors.Is(err, os.ErrNotExist) {
		if opt.RequireAttest {
			return nil, nil, fmt.Errorf("--require-attest: attest key %s does not exist", opt.AttestKey)
		}
		fmt.Fprintf(opt.Stderr, "gosplan run: warning: attest key %s does not exist; verdicts will be unsigned\n", opt.AttestKey)
		return nil, []string{noteUnsigned, "The attest key given with --attest-key was missing; verdicts are unsigned."}, nil
	}
	key, err := attest.LoadPrivateKey(opt.AttestKey)
	if err != nil {
		return nil, nil, err
	}
	return key, nil, nil
}

// Run executes (or resumes) one run to a terminal status and writes its
// report. The returned error is for setup and bookkeeping failures; a halted
// or aborted run is an Outcome, not an error.
func Run(ctx context.Context, opt Options) (Outcome, error) {
	opt, err := opt.withDefaults()
	if err != nil {
		return Outcome{}, err
	}
	key, notes, err := loadKey(opt)
	if err != nil {
		return Outcome{}, err
	}
	sp, err := spec.Load(opt.SpecDir)
	if err != nil {
		return Outcome{}, err
	}
	for _, w := range sp.Warnings {
		fmt.Fprintf(opt.Stderr, "gosplan run: warning: %s\n", w)
	}
	// The budget config is read once, here, before anything is written:
	// an invalid one stops the run before any task runs.
	if opt.budget, err = resolveBudget(opt, sp); err != nil {
		return Outcome{}, err
	}
	opt.spec = sp
	var base string
	if opt.RunID == "" {
		if base, err = (wt.Git{}).ResolveCommit(ctx, opt.Project, opt.BaseRef); err != nil {
			return Outcome{}, err
		}
	}

	store, err := state.Open(filepath.Join(opt.StateDir, "gosplan.db"))
	if err != nil {
		return Outcome{}, err
	}
	defer store.Close()

	run := opt.RunID
	if run == "" {
		run = newRunID(store.Now())
		if err := store.ImportRun(ctx, run, opt.Project, base, sp); err != nil {
			return Outcome{}, err
		}
		if err := store.ImportBudgetTasks(ctx, run, sp); err != nil {
			return Outcome{}, err
		}
	} else {
		got, err := store.SpecSHA(ctx, run)
		if err != nil {
			return Outcome{}, err
		}
		if got != sp.ContentSHA256 {
			return Outcome{}, fmt.Errorf("run %s: spec changed since import (stored %s, now %s)", run, short(got), short(sp.ContentSHA256))
		}
		rr, err := store.GetRun(ctx, run)
		if err != nil {
			return Outcome{}, err
		}
		if rr.Project != opt.Project {
			return Outcome{}, fmt.Errorf("run %s belongs to project %s, not %s", run, rr.Project, opt.Project)
		}
		if reconcile.Ended(rr.Status) {
			// Still refuse while another gosplan run holds the run; an existing
			// lock file is only opened, never created.
			unlock, err := lockRun(filepath.Join(opt.StateDir, "runs", run), run, false)
			if err != nil {
				return Outcome{}, err
			}
			defer unlock()
			return reportEnded(ctx, opt, store, rr, notes)
		}
	}
	runDir := filepath.Join(opt.StateDir, "runs", run)
	for _, d := range runDirs(opt.StateDir, run) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Outcome{}, err
		}
	}
	unlock, err := lockRun(runDir, run, true)
	if err != nil {
		return Outcome{}, err
	}
	defer unlock()
	fmt.Fprintf(opt.Stderr, "gosplan run: run %s, state %s\n", run, opt.StateDir)

	gate, bnotes, err := newBudgetGate(opt, store, run, runDir)
	if err != nil {
		return Outcome{}, err
	}
	notes = append(notes, bnotes...)
	bgate, _ := gate.(*budgetrt.Gate)
	observed := func(obs []state.WindowObservation) {
		if bgate == nil {
			return
		}
		for _, o := range obs {
			bgate.Observed(o)
		}
	}
	// budget.jsonl is flushed once more after the reconciler returns, on
	// every exit path: a halt through haltLocked must not lose the last
	// breaker or halt events.
	defer func() {
		if bgate != nil {
			bgate.Wait()
			if err := bgate.Flush(context.WithoutCancel(ctx)); err != nil {
				fmt.Fprintf(opt.Stderr, "gosplan run: warning: budget: flush: %v\n", err)
			}
		}
	}()

	if opt.UsageProbe {
		p := window.Prober{ClaudeBin: opt.Claude, Env: filterEnv(os.Environ()), Now: store.Now}
		due, err := p.Due(ctx, store)
		if err != nil {
			fmt.Fprintf(opt.Stderr, "gosplan run: warning: usage probe: %v\n", err)
		} else if due {
			obs, err := p.Probe(ctx, store)
			if err != nil {
				fmt.Fprintf(opt.Stderr, "gosplan run: warning: usage probe: %v\n", err)
			} else {
				observed(obs)
			}
		}
	}

	// Window observations are written off the stdout read path: a locked
	// gosplan.db must not stall reading a worker.
	win := make(chan []state.WindowObservation, 64)
	var winDone sync.WaitGroup
	winDone.Add(1)
	go func() {
		defer winDone.Done()
		for obs := range win {
			if err := store.RecordWindows(context.WithoutCancel(ctx), obs); err != nil {
				fmt.Fprintf(opt.Stderr, "gosplan run: warning: record windows: %v\n", err)
				continue
			}
			observed(obs)
		}
	}()
	var mut sync.WaitGroup
	cfg := buildConfig(opt, sp, store, run, runDir, key, &mut, win)
	if gate != nil {
		cfg.Budget = gate
	}
	rec, err := reconcile.New(cfg)
	if err != nil {
		close(win)
		return Outcome{}, err
	}
	status, err := rec.Run(ctx)
	mut.Wait()
	close(win)
	winDone.Wait()
	if bgate != nil {
		bgate.Wait()
		if ferr := bgate.Flush(context.WithoutCancel(ctx)); ferr != nil {
			fmt.Fprintf(opt.Stderr, "gosplan run: warning: budget: flush: %v\n", ferr)
		}
	}
	if err != nil {
		return Outcome{Run: run}, err
	}
	ro := rec.Outcome()
	out := Outcome{Run: run, Status: status, Reason: ro.Reason, IntegrationBranch: ro.IntegrationBranch, IntegrationSHA: ro.IntegrationSHA}

	// Bookkeeping below must finish even after a cancel.
	post := context.WithoutCancel(ctx)
	logPath := filepath.Join(runDir, "events.jsonl")
	if _, err := store.FlushEvents(post, run, logPath); err != nil {
		return out, err
	}
	if err := store.WriteStateJSON(post, run, filepath.Join(runDir, "state.json")); err != nil {
		return out, err
	}
	now := store.Now()
	rep, err := report.Build(post, store, run, now)
	if err != nil {
		return out, err
	}
	rep.Notes = append(rep.Notes, notes...)
	out.ReportPath = filepath.Join(runDir, "report.json")
	if err := report.Write(out.ReportPath, rep); err != nil {
		return out, err
	}
	fmt.Fprint(opt.Stdout, report.Text(rep, now))
	return out, nil
}

// reportEnded is a resume of a run that already ended: it prints the
// report rebuilt from gosplan.db and returns the recorded status. Nothing is
// written: no DB row, log line, state.json or report.json.
func reportEnded(ctx context.Context, opt Options, store *state.Store, rr state.RunRow, notes []string) (Outcome, error) {
	runDir := filepath.Join(opt.StateDir, "runs", rr.ID)
	out := Outcome{Run: rr.ID, Status: rr.Status, IntegrationBranch: rr.IntegrationBranch, IntegrationSHA: rr.IntegrationSHA}
	if _, err := os.Stat(filepath.Join(runDir, "report.json")); err == nil {
		out.ReportPath = filepath.Join(runDir, "report.json")
	}
	fmt.Fprintf(opt.Stderr, "gosplan run: run %s already ended %s; reporting it, nothing written\n", rr.ID, rr.Status)
	now := store.Now()
	rep, err := report.Build(ctx, store, rr.ID, now)
	if err != nil {
		return out, err
	}
	rep.Notes = append(rep.Notes, notes...)
	fmt.Fprint(opt.Stdout, report.Text(rep, now))
	return out, nil
}

// buildConfig wires the reconciler to the real parts.
func buildConfig(opt Options, sp *spec.Spec, store *state.Store, run, runDir string, key ed25519.PrivateKey, mut *sync.WaitGroup,
	win chan<- []state.WindowObservation) reconcile.Config {
	base := os.Environ()
	turns := sidecarMaxTurns(sp)
	return reconcile.Config{
		Run:               run,
		Project:           opt.Project,
		WorktreeRoot:      filepath.Join(opt.StateDir, "worktrees"),
		RunDir:            runDir,
		IntegrationBranch: IntegrationBranch(run),
		Resume:            opt.RunID != "",
		Spec:              sp,
		MaxParallel:       opt.MaxParallel,
		MaxAttempts:       opt.MaxAttempts,
		StallTimeout:      opt.StallTimeout,
		MaxVerifyErrors:   opt.MaxVerifyErrors,
		StallQuietMax:     opt.StallQuietMax,
		RetryDelay:        RetryDelay(opt.RetryBase),
		Store:             store,
		Launcher:          attemptLauncher{next: driver.Exec{}},
		Verifier:          verifyexec.Exec{GosplanBin: opt.GosplanBin, Timeout: opt.VerifyTimeout, OutDir: filepath.Join(runDir, "verify")},
		Workspace:         wt.Git{},
		// Before every launch: back to the recomputed base, so a retry starts
		// fresh with only the failure summary in its prompt.
		ResetWorktree: wt.Git{}.Reset,
		EventLog:      filepath.Join(runDir, "events.jsonl"),
		Now:           store.Now,
		Model: func(n int) string {
			if n <= 1 {
				return opt.Model
			}
			return opt.RetryModel
		},
		BuildLaunch: func(t state.TaskRow, ref core.AttemptRef, prompt, sessionID, model string, p core.Prepared) core.LaunchRequest {
			maxTurns := opt.MaxTurns
			if n, ok := turns[t.ID]; ok {
				maxTurns = n // sidecar max_turns, from the spec in memory
			}
			return core.LaunchRequest{
				Argv: workerArgv(opt.Claude, prompt, sessionID, model, maxTurns),
				Dir:  p.Dir,
				Env:  workerEnv(base, run, t.ID, ref.Attempt),
			}
		},
		OnLine: func(ref core.AttemptRef, line []byte) {
			id := ref.AttemptID
			obs := window.FromLine(line, &id, store.Now())
			if len(obs) == 0 {
				return
			}
			select {
			case win <- obs:
			default:
				fmt.Fprintf(opt.Stderr, "gosplan run: warning: window observations dropped (writer busy)\n")
			}
		},
		OnVerdict: func(ctx context.Context, ref core.AttemptRef, req core.VerifyRequest, out core.VerifyOutcome) {
			if key != nil && len(out.VerdictJSON) > 0 {
				if err := sign(ctx, store, key, opt.GosplanVersion, runDir, ref, out.VerdictJSON); err != nil {
					fmt.Fprintf(opt.Stderr, "gosplan run: warning: sign %s attempt %d: %v\n", ref.Task, ref.Attempt, err)
				}
			}
			if out.Exit == 0 && out.Result == "pass" && opt.Mutation == MutationAdvisory {
				mut.Add(1)
				go func() {
					defer mut.Done()
					rep := mutation.Run(ctx, mutation.Options{
						Project: req.Project, BaseSHA: req.Base, HeadSHA: req.Head, Gremlins: opt.Gremlins,
						OutPath: filepath.Join(runDir, "mutation", fmt.Sprintf("%s.%d.json", ref.Task, ref.Attempt)),
					})
					b, err := json.Marshal(rep)
					if err == nil {
						err = store.SetMutation(context.WithoutCancel(ctx), ref, b)
					}
					if err != nil {
						fmt.Fprintf(opt.Stderr, "gosplan run: warning: mutation %s attempt %d: %v\n", ref.Task, ref.Attempt, err)
					}
				}()
			}
		},
	}
}

// sign writes the DSSE envelope for one verdict and records its path.
func sign(ctx context.Context, store *state.Store, key ed25519.PrivateKey, version, runDir string, ref core.AttemptRef, verdictJSON []byte) error {
	path := filepath.Join(runDir, "attest", fmt.Sprintf("%s.%d.dsse.json", ref.Task, ref.Attempt))
	in := attest.Input{Run: ref.Run, Attempt: ref.Attempt, GosplanVersion: version, Time: store.Now()}
	if err := attest.SignVerdictJSON(ctx, key, verdictJSON, in, path); err != nil {
		return err
	}
	return store.SetAttestation(context.WithoutCancel(ctx), ref, path)
}

// IntegrationBranch is the branch passed heads are merged into.
func IntegrationBranch(run string) string { return "gosplan/" + run + "/integration" }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
