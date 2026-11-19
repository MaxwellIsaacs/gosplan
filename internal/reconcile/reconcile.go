// Package reconcile is the level-triggered reconciler behind `gosplan run`.
// Each Reconcile call reads the current SQLite rows plus the
// in-memory table of live worker handles and in-flight verifies, and takes at
// most one step toward "every task passed". Process exits, verifier
// completions and timers only Add keys to the client-go workqueue; they never
// change state themselves.
//
// The verdict comes only from core.Verifier. Nothing here reads a file in a
// task worktree.
package reconcile

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"k8s.io/client-go/util/workqueue"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/driver"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/streamjson"
)

// Config wires a Reconciler. Zero durations and counts take the defaults
// documented on each field.
type Config struct {
	Run               string
	Project           string        // absolute
	WorktreeRoot      string        // <state>/worktrees
	RunDir            string        // <state>/runs/<run>
	IntegrationBranch string        // default gosplan/<run>/integration
	Resume            bool          // the run existed before this process: treat the DB as untrusted
	Spec              *spec.Spec    // the loaded spec; required with Resume (the static task columns are checked against it)
	MaxParallel       int           // required (runner.Defaults has gosplan run's)
	MaxAttempts       int           // required
	StallTimeout      time.Duration // required
	LeaseCheck        time.Duration // default 15s
	VerifyRetryDelay  time.Duration // default 30s
	LockTimeout       time.Duration // default 1m: gosplan.db busy this long halts state_locked
	ProgressTimeout   time.Duration // default 2m: a task stuck this long halts no_progress
	MaxInfraFailures  int           // default 10: this many in a row (no launch between) halt infrastructure
	WriteFailTimeout  time.Duration // default 1m: a step whose write keeps failing (not busy) this long halts state_write_failed
	MaxWriteFailures  int           // default 10: this many consecutive failed writes of one step halt state_write_failed
	// DiskFree reports the free bytes (and where) on the filesystem holding
	// the state dir and worktrees; nil turns the disk floor off. Below
	// MinFreeDisk the run halts disk_low before state writes start failing.
	DiskFree          func() (int64, string, error)
	MinFreeDisk       int64
	UsageResetSlack   time.Duration             // default 2m: added to a usage limit's reset before the run dispatches again
	UsageLimitBackoff time.Duration             // default 5m: the wait after a usage limit whose stream names no future reset
	RetryDelay        func(n int) time.Duration // required: runner.RetryDelay(base)
	Workers           int                       // queue workers, default 2

	Store    *state.Store
	Launcher core.Launcher
	Verifier core.Verifier
	// Workspace must also implement BaseComputer.
	Workspace core.Workspace
	// ResetWorktree, if set, puts a worktree back at base before every
	// launch, so a retry (or a reused worktree) starts clean.
	ResetWorktree func(ctx context.Context, dir, base string) error
	// BuildLaunch turns a task + attempt into argv/env (5.a supplies the real
	// claude argv). Prompt text comes from BuildPrompt. The reconciler
	// overrides Ref and StreamPath, and fills Dir and StderrPath when empty.
	BuildLaunch func(t state.TaskRow, ref core.AttemptRef, prompt, sessionID, model string, wt core.Prepared) core.LaunchRequest
	BuildPrompt func(t state.TaskRow, attempt int, lastFailure string) (string, error) // default: DefaultPrompt
	Model       func(attempt int) string                                               // required: model for the n-th consumed attempt
	OnLine      core.LineHook                                                          // optional
	OnVerdict   core.VerdictHook                                                       // optional, runs after the verdict is recorded
	EventLog    string                                                                 // default <RunDir>/events.jsonl, flushed after each applied step
	Now         func() time.Time                                                       // default time.Now
	Budget      Budget                                                                 // optional budget gate; nil keeps master's behaviour
	// MaxVerifyErrors consecutive could-not-verify verdicts across the run
	// halt it verifier_error, with or without a gate. Policy,
	// not a measurement; default 3.
	MaxVerifyErrors int
	// StallQuietMax caps the extra silence one api_retry line buys,
	// with or without a gate. Policy, not a measurement; default 15m.
	StallQuietMax time.Duration

	bst *budgetRunState // run-wide budget/verifier state; New allocates it
}

// BaseComputer recomputes a task base from the run base and the dependency
// heads (wt.Git.ComputeBase). It must be deterministic: the same inputs give
// the same SHA, so a base is recomputed, never read back from the DB.
type BaseComputer interface {
	ComputeBase(ctx context.Context, project, base string, heads []string) (string, error)
}

// conflictInfo is implemented by merge-conflict errors that name the head
// and the paths (wt.MergeConflict).
type conflictInfo interface {
	ConflictHead() string
	ConflictPaths() []string
}

// Outcome is how Run ended.
type Outcome struct {
	Status            string
	Reason            string // why the run halted: a Reason* prefix, or "not passed: ..."
	IntegrationBranch string
	IntegrationSHA    string
}

// Result tells the worker loop when to look at the key again.
type Result struct{ RequeueAfter time.Duration }

const (
	runKey     = "run"
	taskPrefix = "task/"

	// pollInterval is the requeue for the run key while the run is not done
	// and for a ready task waiting for a free slot.
	pollInterval = 2 * time.Second
	// maxFailureBytes bounds tasks.last_failure.
	maxFailureBytes = 4096
	// verifyErrorLimit consecutive could-not-verify results block the task.
	verifyErrorLimit = 2
	// stallRecheck is how soon a stall-killed task is looked at again, and
	// stallExitWait how long its exit may take before it is recorded anyway.
	stallRecheck  = 500 * time.Millisecond
	stallExitWait = 30 * time.Second
	// AbortWait bounds how long abort (SIGINT/SIGTERM) and a halt wait for
	// killed workers and cancelled verifies to return.
	AbortWait = 10 * time.Second
)

// Halt reasons added by the budget gate (the others are in memory.go).
const (
	ReasonVerifierError = "verifier_error" // MaxVerifyErrors consecutive could-not-verify verdicts
	ReasonDeadline      = "deadline"       // budget stop halt at --deadline
	ReasonCap           = "cap"            // budget drain halt at a per-run cap
)

type liveTask struct {
	ref        core.AttemptRef
	h          core.Handle
	streamPath string
	lastLine   time.Time
	quietUntil time.Time // stall backstop deadline: last line + StallTimeout + its capped quiet allowance
	exited     bool
	exit       core.Exit
	stallKill  bool      // killed by Reconcile's own heartbeat check
	killedAt   time.Time // when stallKill was set
	budgetStop bool      // killed by a budget stop halt: refunded, back to pending
}

// budgetRunState is the run-wide state of the always-on verifier_error rule
// and of a budget halt. verifyErrs is read and written under stateMu (in the
// verdict apply); halt and stopped under mu.
type budgetRunState struct {
	verifyErrs int // consecutive verdicts recorded "error", across the run

	mu      sync.Mutex
	halt    budget.Halt // the first budget halt Tick returned (Kind "" = none)
	stopped bool        // the stop kill has been sent
}

type verifyTask struct {
	ref  core.AttemptRef
	req  core.VerifyRequest
	done bool
	out  core.VerifyOutcome
	err  error
}

// Reconciler drives one run. Create it with New; call Run once.
type Reconciler struct {
	cfg   Config
	base  BaseComputer
	owner string
	queue workqueue.TypedRateLimitingInterface[string]

	// stateMu serialises every DB write that touches tasks or runs with the
	// memory update that mirrors it (see apply).
	stateMu     sync.Mutex
	mem         *memState
	haltReason  string
	busySince   time.Time
	stuckSince  map[string]time.Time
	finalReason string // run_finished reason of a normal finish
	infra       int    // infrastructure failures since the last launch
	writeFails  map[string]*writeFail
	infraTask   map[string]int // task -> lost/start_failed attempts this process recorded

	mu       sync.Mutex
	live     map[string]*liveTask   // task id -> worker started by this process
	verifies map[string]*verifyTask // task id -> verify started by this process
	verifyAt map[string]time.Time   // task id -> earliest verify retry

	mine map[int64]bool // attempt ids this process started and has not finished

	childCtx       context.Context // launches and verifies; cancelled on halt
	cancelChildren context.CancelFunc

	finished   chan struct{}
	finishOnce sync.Once
	bg         sync.WaitGroup // supervisor and verify goroutines

	// Test hooks.
	reconcileFn    func(ctx context.Context, key string) (Result, error)
	afterPlan      func(key string)
	afterReconcile func(key string, res Result, err error)
}

// New validates cfg, applies defaults and builds the work queue.
func New(cfg Config) (*Reconciler, error) {
	switch {
	case cfg.Run == "":
		return nil, errors.New("reconcile: Config.Run is required")
	case cfg.RunDir == "":
		return nil, errors.New("reconcile: Config.RunDir is required")
	case cfg.Store == nil, cfg.Launcher == nil, cfg.Verifier == nil, cfg.Workspace == nil:
		return nil, errors.New("reconcile: Config.Store, Launcher, Verifier and Workspace are required")
	case cfg.BuildLaunch == nil:
		return nil, errors.New("reconcile: Config.BuildLaunch is required")
	case cfg.MaxParallel <= 0, cfg.MaxAttempts <= 0, cfg.StallTimeout <= 0, cfg.Model == nil, cfg.RetryDelay == nil:
		// gosplan run's defaults live in runner.Defaults only.
		return nil, errors.New("reconcile: Config.MaxParallel, MaxAttempts, StallTimeout, Model and RetryDelay are required")
	}
	if cfg.Resume && cfg.Spec == nil {
		return nil, errors.New("reconcile: Config.Spec is required with Resume")
	}
	bc, ok := cfg.Workspace.(BaseComputer)
	if !ok {
		return nil, errors.New("reconcile: Config.Workspace must implement BaseComputer")
	}
	if cfg.IntegrationBranch == "" {
		cfg.IntegrationBranch = "gosplan/" + cfg.Run + "/integration"
	}
	if cfg.LeaseCheck <= 0 {
		cfg.LeaseCheck = 15 * time.Second
	}
	if cfg.VerifyRetryDelay <= 0 {
		cfg.VerifyRetryDelay = 30 * time.Second
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = time.Minute
	}
	if cfg.ProgressTimeout <= 0 {
		cfg.ProgressTimeout = 2 * time.Minute
	}
	if cfg.MaxInfraFailures <= 0 {
		cfg.MaxInfraFailures = 10
	}
	if cfg.WriteFailTimeout <= 0 {
		cfg.WriteFailTimeout = time.Minute
	}
	if cfg.MaxWriteFailures <= 0 {
		cfg.MaxWriteFailures = 10
	}
	if cfg.UsageResetSlack <= 0 {
		cfg.UsageResetSlack = 2 * time.Minute
	}
	if cfg.UsageLimitBackoff <= 0 {
		cfg.UsageLimitBackoff = 5 * time.Minute
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.BuildPrompt == nil {
		cfg.BuildPrompt = DefaultPrompt
	}
	if cfg.EventLog == "" {
		cfg.EventLog = filepath.Join(cfg.RunDir, "events.jsonl")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxVerifyErrors <= 0 {
		cfg.MaxVerifyErrors = 3 // policy
	}
	if cfg.StallQuietMax <= 0 {
		cfg.StallQuietMax = driver.DefaultStallQuietMax // policy
	}
	cfg.bst = &budgetRunState{}
	if err := os.MkdirAll(filepath.Join(cfg.RunDir, "attempts"), 0o700); err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	r := &Reconciler{
		cfg:        cfg,
		base:       bc,
		owner:      driver.LeaseOwner(),
		queue:      workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		live:       map[string]*liveTask{},
		verifies:   map[string]*verifyTask{},
		verifyAt:   map[string]time.Time{},
		stuckSince: map[string]time.Time{},
		writeFails: map[string]*writeFail{},
		infraTask:  map[string]int{},
		finished:   make(chan struct{}),
	}
	r.childCtx, r.cancelChildren = context.WithCancel(context.Background())
	r.reconcileFn = r.Reconcile
	return r, nil
}

// Outcome reports how the run ended (valid after Run returns).
func (r *Reconciler) Outcome() Outcome {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	o := Outcome{Reason: r.haltReason}
	if o.Reason == "" {
		o.Reason = r.finalReason
	}
	if r.mem != nil {
		o.Status = r.mem.run.Status
		o.IntegrationBranch, o.IntegrationSHA = r.mem.run.IntegrationBranch, r.mem.run.IntegrationSHA
	}
	return o
}

func taskKey(id string) string { return taskPrefix + id }

// Run seeds the queue with every task key and "run", processes keys with
// cfg.Workers goroutines until the run reaches a terminal status (complete,
// halted) or ctx is cancelled (then: kill every live group, finish attempts
// as killed, run aborted). Returns the final run status; Outcome has the
// rest. Cancellation is a normal outcome: it returns core.RunAborted with a
// nil error.
//
// A new run's rows are loaded once at the start: this process imported
// them and no worker has run yet. Resuming a run that already ended
// returns its recorded status and writes nothing. On resume (Config.Resume)
// the DB is untrusted: the log is replayed against it, the integration tip is
// cross-checked, and every passed task is verified again by SHA before
// anything is dispatched.
func (r *Reconciler) Run(ctx context.Context) (string, error) {
	defer r.cancelChildren()
	stop := context.AfterFunc(ctx, r.cancelChildren)
	defer stop()

	if r.cfg.Resume {
		// A run that already ended (complete or halted) has nothing left
		// to decide: report what gosplan.db records and write nothing. (An old
		// binary's log, without integration_sha=, would otherwise fail the
		// checks below and the run would be rewritten halted.)
		if err := r.load(ctx); err != nil {
			return "", err
		}
		if st := r.snapshot().run.Status; Ended(st) {
			return st, nil
		}
	}
	if err := r.flush(ctx); err != nil {
		return "", err
	}
	if err := r.load(ctx); err != nil {
		return "", err
	}
	run := r.snapshot().run
	tasks := r.snapshot().rows()
	if r.cfg.Resume {
		// The crashed orchestrator's workers and verifies go first, whatever
		// the checks find: a resume that halts must not leave them running
		// (each kill is identity-checked, so a forged lease cannot aim it).
		if err := r.killLeaseGroups(ctx); err != nil {
			return "", err
		}
		r.sweepOrphans()
		reason, err := r.resumeChecks(ctx)
		if err != nil {
			return "", err
		}
		if reason != "" {
			r.halt(ctx, reason)
			return core.RunHalted, nil
		}
	}
	if run.Status == core.RunAborted && r.cfg.Resume {
		// An interrupted run continues: back to running.
		if err := r.apply(ctx, func(x *txn) error { return x.reopenRun() }); err != nil {
			return "", err
		}
		run.Status = core.RunRunning
	}
	if run.Status != core.RunRunning {
		return run.Status, nil
	}
	if r.cfg.Resume {
		reason, err := r.loadResume(ctx)
		if err != nil {
			return "", err
		}
		if reason != "" {
			r.halt(ctx, reason)
			return core.RunHalted, nil
		}
	}
	for _, t := range tasks {
		r.queue.Add(taskKey(t.ID))
	}
	r.queue.Add(runKey)

	var workers sync.WaitGroup
	for i := 0; i < r.cfg.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for r.processNextWorkItem(r.childCtx) {
			}
		}()
	}

	select {
	case <-r.finished:
		r.queue.ShutDown()
		workers.Wait()
		r.waitBG()
		if !r.isHalted() {
			if reason := r.selfCheck(context.WithoutCancel(ctx)); reason != "" {
				r.halt(ctx, reason)
			} else if r.cfg.Budget != nil {
				if reason := r.cfg.Budget.SelfCheck(context.WithoutCancel(ctx)); reason != "" {
					r.halt(ctx, reason)
				}
			}
		}
		return r.Outcome().Status, nil
	case <-ctx.Done():
		r.queue.ShutDown()
		workers.Wait()
		return r.abort(context.WithoutCancel(ctx))
	}
}

// Ended reports whether a run status is final: complete or halted. An
// aborted run is not: a resume continues it.
func Ended(status string) bool { return status == core.RunComplete || status == core.RunHalted }

// load reads the run and task rows into memory.
func (r *Reconciler) load(ctx context.Context) error {
	run, err := r.cfg.Store.GetRun(ctx, r.cfg.Run)
	if err != nil {
		return err
	}
	tasks, err := r.cfg.Store.ListTasks(ctx, r.cfg.Run)
	if err != nil {
		return err
	}
	r.stateMu.Lock()
	r.mem = newMemState(run, tasks)
	r.stateMu.Unlock()
	return nil
}

func (r *Reconciler) isHalted() bool {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.halted()
}

// snapshot is a copy of memory for planning outside the lock.
func (r *Reconciler) snapshot() *memState {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.mem.clone()
}

// abort kills every live group, finishes this process's open attempts as
// killed and marks the run aborted. Task statuses are left as they are;
// a later reconciler recovers them (no live handle -> retry path).
func (r *Reconciler) abort(ctx context.Context) (string, error) {
	r.mu.Lock()
	lives := make(map[string]*liveTask, len(r.live))
	for id, lt := range r.live {
		lives[id] = lt
	}
	r.mu.Unlock()
	for _, lt := range lives {
		_ = lt.h.Kill()
	}
	r.waitBG()

	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.mem.run.Status != core.RunRunning || r.halted() {
		return r.mem.run.Status, nil // halted (or finished) before the cancel
	}
	now := r.cfg.Now()
	status := core.RunAborted
	err := r.cfg.Store.Immediate(ctx, func(tx *sql.Tx) error {
		for id, ref := range r.mem.attempt {
			if t := r.mem.tasks[id]; t == nil || t.Status != core.StatusRunning || !r.isMine(ref) {
				continue
			}
			end := state.AttemptEnd{Outcome: "killed"}
			r.mu.Lock()
			if lt := lives[id]; lt != nil && lt.exited {
				code := lt.exit.Code
				end.ExitCode = &code
			}
			r.mu.Unlock()
			if err := state.FinishAttempt(tx, now, ref, end); err != nil {
				return err
			}
			if r.cfg.Budget != nil {
				if err := r.cfg.Budget.Settle(tx, ref, end); err != nil {
					return err
				}
			}
		}
		if r.mem.run.Status != core.RunRunning {
			status = r.mem.run.Status
			return nil
		}
		return state.FinishRun(tx, now, r.cfg.Run, core.RunAborted, "context cancelled")
	})
	if errors.Is(err, ErrBudgetTampered) {
		r.haltLocked(ctx, ReasonTampered+": budget: "+err.Error()) // stateMu is held
		return core.RunHalted, nil
	}
	if err != nil {
		return "", err
	}
	r.mem.run.Status = status
	r.mu.Lock()
	r.live = map[string]*liveTask{}
	r.mu.Unlock()
	if err := r.flush(ctx); err != nil {
		return status, err
	}
	return status, nil
}

// waitBG waits for supervisor and verify goroutines, at most AbortWait: a
// handle that never returns must not keep gosplan run from exiting.
func (r *Reconciler) waitBG() {
	done := make(chan struct{})
	go func() { r.bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(AbortWait):
	}
}

func (r *Reconciler) markMine(ref core.AttemptRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mine == nil {
		r.mine = map[int64]bool{}
	}
	r.mine[ref.AttemptID] = true
}

func (r *Reconciler) isMine(ref core.AttemptRef) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mine[ref.AttemptID]
}

// processNextWorkItem handles one key with the controller-runtime contract.
// It returns false once the queue is shut down and drained.
func (r *Reconciler) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(key)
	res, err := r.reconcileFn(ctx, key)
	if r.afterReconcile != nil {
		r.afterReconcile(key, res, err)
	}
	switch {
	case errors.Is(err, errHalted):
		r.queue.Forget(key)
	case err != nil:
		r.queue.AddRateLimited(key)
	case res.RequeueAfter > 0:
		r.queue.Forget(key)
		r.queue.AddAfter(key, res.RequeueAfter)
	default:
		r.queue.Forget(key)
	}
	return true
}

// Reconcile takes one step for key "task/<id>" or "run". A failed apply-step
// re-check returns core.ErrConflict (the worker requeues it).
func (r *Reconciler) Reconcile(ctx context.Context, key string) (Result, error) {
	ctx = context.WithValue(ctx, stepKey{}, key)
	if key == runKey {
		return r.reconcileRun(ctx)
	}
	id, ok := strings.CutPrefix(key, taskPrefix)
	if !ok || id == "" {
		return Result{}, fmt.Errorf("reconcile: bad key %q", key)
	}
	return r.reconcileTask(ctx, id)
}

func (r *Reconciler) reconcileTask(ctx context.Context, id string) (Result, error) {
	snap := r.snapshot()
	if r.isHalted() || snap.run.Status != core.RunRunning {
		return Result{}, nil
	}
	tp := snap.tasks[id]
	if tp == nil {
		return Result{}, fmt.Errorf("reconcile: no task %s in run %s", id, r.cfg.Run)
	}
	t := *tp
	switch t.Status {
	case core.StatusPending:
		return r.reconcilePending(ctx, snap, t)
	case core.StatusRunning:
		return r.reconcileRunning(ctx, snap, t)
	case core.StatusVerifying:
		return r.reconcileVerifying(ctx, snap, t)
	default:
		return Result{}, nil
	}
}

// depsPassed reports whether every dep of t is passed in m.
func depsPassed(m *memState, t state.TaskRow) bool {
	for _, d := range t.Deps {
		if x := m.tasks[d]; x == nil || x.Status != core.StatusPassed {
			return false
		}
	}
	return true
}

func countActive(m *memState) int {
	n := 0
	for _, t := range m.tasks {
		if t.Status == core.StatusRunning || t.Status == core.StatusVerifying {
			n++
		}
	}
	return n
}

// depHeads are the heads of t's deps, in dependency order, from memory.
func depHeads(m *memState, t state.TaskRow) []string {
	var heads []string
	for _, d := range t.Deps {
		heads = append(heads, m.tasks[d].HeadSHA)
	}
	return heads
}

// computeBase is t's base: the run base with its deps' verified heads
// merged in order. It is recomputed from memory every time it is needed.
func (r *Reconciler) computeBase(ctx context.Context, m *memState, t state.TaskRow) (string, error) {
	return r.base.ComputeBase(ctx, r.cfg.Project, m.run.BaseSHA, depHeads(m, t))
}

// reconcilePending plans a dispatch from memory.
func (r *Reconciler) reconcilePending(ctx context.Context, snap *memState, t state.TaskRow) (Result, error) {
	now := r.cfg.Now()
	if t.NotBefore != nil && t.NotBefore.After(now) {
		return Result{RequeueAfter: t.NotBefore.Sub(now)}, nil
	}
	if !depsPassed(snap, t) {
		return Result{}, nil // a dep's step Adds this key
	}
	if countActive(snap) >= r.cfg.MaxParallel {
		return Result{RequeueAfter: pollInterval}, nil
	}
	if r.afterPlan != nil {
		r.afterPlan(taskKey(t.ID))
	}
	return r.dispatch(ctx, snap, t)
}

// dispatch recomputes the base, applies the plan in one IMMEDIATE tx with a
// re-check, then prepares the worktree and launches the worker.
func (r *Reconciler) dispatch(ctx context.Context, snap *memState, t state.TaskRow) (Result, error) {
	base, err := r.computeBase(ctx, snap, t)
	if errors.Is(err, core.ErrMergeConflict) {
		reason := truncate("merge conflict preparing worktree: " + err.Error())
		err := r.apply(ctx, func(x *txn) error {
			if err := x.transition(t.ID, core.StatusPending, core.StatusBlocked, reason); err != nil {
				return err
			}
			return x.update(t.ID, state.TaskUpdate{BlockedReason: &reason})
		})
		if err != nil {
			return Result{}, err
		}
		r.addPending(snap, t.ID)
		return Result{}, r.afterStep(ctx)
	}
	if err != nil {
		return Result{}, err
	}
	if t.BaseSHA != "" && t.BaseSHA != base {
		r.halt(ctx, fmt.Sprintf("%s: task %s: recomputed base %s differs from the base its earlier attempt used (%s)",
			ReasonInconsistent, t.ID, base, t.BaseSHA))
		return Result{}, errHalted
	}
	r.cfg.bst.mu.Lock()
	budgetHalted := r.cfg.bst.halt.Kind != ""
	r.cfg.bst.mu.Unlock()
	if budgetHalted {
		return Result{RequeueAfter: pollInterval}, nil // no new attempt while a budget halt is set
	}
	sessionID, err := newUUID()
	if err != nil {
		return Result{}, err
	}
	nonce, err := r.newNonce()
	if err != nil {
		return Result{}, err
	}
	var ref core.AttemptRef
	var model, streamPath string
	infraBlocked := false
	var adm AdmitResult
	refused := false
	err = r.apply(ctx, func(x *txn) error {
		m := x.m.tasks[t.ID]
		if m.Status != core.StatusPending || !depsPassed(x.m, *m) || (m.NotBefore != nil && m.NotBefore.After(x.now)) {
			return core.ErrConflict
		}
		if countActive(x.m) >= r.cfg.MaxParallel {
			return core.ErrConflict
		}
		// Refunded attempts are bounded too, across restarts: a worker
		// that kills gosplan run (its attempt then ends lost on resume) or
		// sabotages its own launch must not get a fresh quota every time.
		lost, err := state.CountInfraAttempts(x.tx, r.cfg.Run, t.ID)
		if err != nil {
			return err
		}
		lost = max(lost, r.infraTask[t.ID]) // rows deleted in-run do not lower it
		if lost >= r.cfg.MaxAttempts {
			infraBlocked = true
			reason := fmt.Sprintf("%s: %d lost/start_failed attempts", ReasonInfrastructure, lost)
			if err := x.transition(t.ID, core.StatusPending, core.StatusBlocked, reason); err != nil {
				return err
			}
			return x.update(t.ID, state.TaskUpdate{BlockedReason: &reason})
		}
		n, err := state.NextAttempt(x.tx, r.cfg.Run, t.ID)
		if err != nil {
			return err
		}
		model = r.cfg.Model(m.AttemptsUsed + 1)
		if r.cfg.Budget != nil {
			adm, err = r.cfg.Budget.Admit(x.tx, AdmitRequest{Run: r.cfg.Run, Task: t.ID, Attempt: n, Model: model, Now: x.now})
			if err != nil {
				return err
			}
			if !adm.Admit {
				// Refused: the gate wrote its park row and events; the task
				// stays pending and not_before goes through txn.
				if !adm.NotBefore.After(x.now) {
					adm.NotBefore = x.now.Add(time.Second)
				}
				refused = true
				nb := adm.NotBefore
				p := &nb
				return x.update(t.ID, state.TaskUpdate{NotBefore: &p})
			}
			if adm.Model != "" {
				model = adm.Model
			}
		}
		streamPath = filepath.Join(r.cfg.RunDir, "attempts", fmt.Sprintf("%s.%d.stream.jsonl", t.ID, n))
		if err := x.transition(t.ID, core.StatusPending, core.StatusRunning, fmt.Sprintf("attempt %d", n)); err != nil {
			return err
		}
		ref, err = state.StartAttempt(x.tx, x.now, state.AttemptStart{
			Run: r.cfg.Run, Task: t.ID, SessionID: sessionID, Model: model, Owner: r.owner, StreamPath: streamPath,
			BaseSHA: base, Branch: m.Branch, Nonce: nonce,
		})
		if err != nil {
			return err
		}
		if r.cfg.Budget != nil {
			if err := r.cfg.Budget.Reserve(x.tx, ref, adm); err != nil {
				return err
			}
		}
		m.AttemptsUsed++
		x.m.attempt[t.ID] = ref
		delete(x.m.verdicts, t.ID)
		return x.update(t.ID, state.TaskUpdate{BaseSHA: &base})
	})
	if errors.Is(err, ErrBudgetTampered) {
		r.halt(ctx, ReasonTampered+": budget: "+err.Error())
		return Result{}, errHalted
	}
	if err != nil {
		return Result{}, err
	}
	if refused {
		if err := r.afterStep(ctx); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: adm.NotBefore.Sub(r.cfg.Now())}, nil
	}
	if infraBlocked {
		r.addPending(snap, t.ID)
		return Result{}, r.afterStep(ctx)
	}
	r.markMine(ref)
	t.Status, t.AttemptsUsed, t.BaseSHA = core.StatusRunning, t.AttemptsUsed+1, base
	if err := r.afterStep(ctx); err != nil {
		// The journal could not be written: do not start a worker the log
		// does not know about.
		return r.startFailed(ctx, snap, t, ref, fmt.Errorf("write %s: %w", r.cfg.EventLog, err), false)
	}

	wt, err := r.cfg.Workspace.Prepare(ctx, core.PrepareRequest{
		Project: r.cfg.Project, Run: r.cfg.Run, Task: t.ID, BaseSHA: snap.run.BaseSHA, DepHeads: depHeads(snap, t), Root: r.cfg.WorktreeRoot,
	})
	switch {
	case err != nil:
		return r.startFailed(ctx, snap, t, ref, err, errors.Is(err, core.ErrMergeConflict))
	case wt.BaseSHA != base:
		return r.startFailed(ctx, snap, t, ref, fmt.Errorf("workspace prepared base %s, recomputed %s", wt.BaseSHA, base), false)
	case t.Worktree != "" && wt.Dir != t.Worktree:
		return r.startFailed(ctx, snap, t, ref, fmt.Errorf("workspace moved the worktree from %s to %s", t.Worktree, wt.Dir), false)
	}
	if wt.Branch != t.Branch || wt.Dir != t.Worktree {
		err = r.apply(ctx, func(x *txn) error {
			return x.update(t.ID, state.TaskUpdate{Branch: &wt.Branch, Worktree: &wt.Dir})
		})
		if err != nil {
			if errors.Is(err, errHalted) {
				return Result{}, err
			}
			return r.startFailed(ctx, snap, t, ref, err, false)
		}
		t.Branch, t.Worktree = wt.Branch, wt.Dir
	}
	if r.cfg.ResetWorktree != nil {
		if err := r.cfg.ResetWorktree(ctx, wt.Dir, base); err != nil {
			return r.startFailed(ctx, snap, t, ref, err, false)
		}
	}

	prompt, err := r.cfg.BuildPrompt(t, ref.Attempt, t.LastFailure)
	if err != nil {
		return r.startFailed(ctx, snap, t, ref, err, false)
	}
	req := r.cfg.BuildLaunch(t, ref, prompt, sessionID, model, wt)
	req.Argv = append(req.Argv, adm.ExtraArgv...) // budget belts
	req.Env = withNonce(req.Env, nonce)
	req.Ref = ref
	req.StreamPath = streamPath
	if req.Dir == "" {
		req.Dir = wt.Dir
	}
	if req.StderrPath == "" {
		req.StderrPath = strings.TrimSuffix(streamPath, ".stream.jsonl") + ".stderr"
	}
	h, err := r.cfg.Launcher.Launch(ctx, req)
	if err != nil {
		return r.startFailed(ctx, snap, t, ref, err, false)
	}
	r.stateMu.Lock()
	r.infra = 0
	r.stateMu.Unlock()
	launched := r.cfg.Now()
	lt := &liveTask{ref: ref, h: h, streamPath: streamPath, lastLine: launched, quietUntil: launched.Add(r.cfg.StallTimeout)}
	r.mu.Lock()
	r.live[t.ID] = lt
	r.mu.Unlock()
	r.cfg.bst.mu.Lock()
	if r.cfg.bst.stopped {
		// A budget stop swept the live table while this launch was in
		// flight: stop this worker the same way.
		r.mu.Lock()
		lt.budgetStop = true
		r.mu.Unlock()
		_ = h.Kill()
	}
	r.cfg.bst.mu.Unlock()
	r.bg.Add(1)
	go r.supervise(ctx, t.ID, lt)
	pid, pgid := h.PID(), h.PGID()
	var started uint64
	if pid > 0 {
		started, _ = driver.ProcStartTime(pid)
		if !driver.LeadsGroup(pid, pgid) {
			// The launcher starts each worker as its own group leader
			// (pgid == pid). Anything else is a group this process did not
			// make: record none, so no restart ever kills it by group, and
			// stop this worker (it takes the normal exit path).
			pgid = 0
			_ = h.Kill()
		}
	}
	if err := r.cfg.Store.SetLeaseProcess(ctx, ref, pid, pgid, started); err != nil {
		return Result{}, err
	}
	return Result{RequeueAfter: r.cfg.LeaseCheck}, nil
}

// supervise runs the driver Supervisor (heartbeat + stall kill), stores the
// exit in the live table and Adds the task key. It changes no state.
func (r *Reconciler) supervise(ctx context.Context, id string, lt *liveTask) {
	defer r.bg.Done()
	// The api_retry quiet allowance is always on: the gate's
	// when one is set, else retry_delay_ms capped at StallQuietMax.
	quiet := func(line []byte) time.Duration { return driver.APIRetryQuiet(line, r.cfg.StallQuietMax) }
	if r.cfg.Budget != nil {
		quiet = r.cfg.Budget.Quiet
	}
	sup := driver.Supervisor{
		Store:        r.cfg.Store,
		StallTimeout: r.cfg.StallTimeout,
		Now:          r.cfg.Now,
		Quiet:        quiet,
		QuietMax:     r.cfg.StallQuietMax,
		OnLine: func(line []byte) {
			allow := min(max(quiet(line), 0), r.cfg.StallQuietMax)
			r.mu.Lock()
			lt.lastLine = r.cfg.Now()
			lt.quietUntil = lt.lastLine.Add(r.cfg.StallTimeout + allow)
			r.mu.Unlock()
			if r.cfg.OnLine != nil {
				r.cfg.OnLine(lt.ref, line)
			}
		},
	}
	exit := sup.Run(ctx, lt.ref, lt.h)
	r.mu.Lock()
	lt.exit, lt.exited = exit, true
	r.mu.Unlock()
	r.queue.Add(taskKey(id))
}

// startFailed finishes the attempt as start_failed, then blocks the task
// (merge conflict) or takes the retry path. A worker that never started
// cannot have failed: the attempt is given back (attempts_used is not
// consumed) and counts as an infrastructure failure instead.
func (r *Reconciler) startFailed(ctx context.Context, snap *memState, t state.TaskRow, ref core.AttemptRef, cause error, block bool) (Result, error) {
	err := r.apply(ctx, func(x *txn) error {
		end := state.AttemptEnd{Outcome: "start_failed"}
		if err := state.FinishAttempt(x.tx, x.now, ref, end); err != nil {
			return err
		}
		if r.cfg.Budget != nil {
			if err := r.cfg.Budget.Settle(x.tx, ref, end); err != nil {
				return err
			}
		}
		if err := x.refund(t.ID); err != nil {
			return err
		}
		if block {
			reason := truncate("merge conflict preparing worktree: " + cause.Error())
			if err := x.transition(t.ID, core.StatusRunning, core.StatusBlocked, reason); err != nil {
				return err
			}
			return x.update(t.ID, state.TaskUpdate{BlockedReason: &reason})
		}
		return r.retryPath(x, t.ID, core.StatusRunning, fmt.Sprintf("attempt %d failed to start: %v", ref.Attempt, cause))
	})
	if errors.Is(err, ErrBudgetTampered) {
		r.halt(ctx, ReasonTampered+": budget: "+err.Error())
		return Result{}, errHalted
	}
	if err != nil {
		return Result{}, err
	}
	if !block && r.noteInfra(ctx, t.ID, fmt.Sprintf("%s attempt %d failed to start: %v", t.ID, ref.Attempt, cause)) {
		return Result{}, errHalted
	}
	r.addPending(snap, t.ID)
	return Result{}, r.afterStep(ctx)
}

// noteInfra counts an infrastructure failure (a worker that never started,
// or one lost with an orchestrator) and halts the run `infrastructure` once
// MaxInfraFailures happen with no launch in between. It also counts the
// failure against the task (see dispatch). Reports whether it halted.
func (r *Reconciler) noteInfra(ctx context.Context, task, what string) bool {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.infra++
	r.infraTask[task]++
	if r.infra < r.cfg.MaxInfraFailures {
		return false
	}
	r.haltLocked(ctx, fmt.Sprintf("%s: %d infrastructure failures in a row, no worker launched; last: %s",
		ReasonInfrastructure, r.infra, firstLine(what)))
	return true
}

// usageLimitPark (inside a tx): the worker was refused by the plan's usage
// limit. The attempt is refunded (it says nothing about the task), the task
// goes back to pending until the reset, and every other pending task that
// would be ready sooner waits for the same reset, so the run does not spend
// one refused launch per task. No retryPath: no backoff growth, no
// same-failure breaker, last_failure unchanged. --deadline bounds the wait.
func (r *Reconciler) usageLimitPark(x *txn, id string, until time.Time, why string) error {
	if err := x.refund(id); err != nil {
		return err
	}
	reason := truncate(fmt.Sprintf("usage limit: waiting for reset at %s (%s)", until.Format(time.RFC3339), why))
	if err := x.transition(id, core.StatusRunning, core.StatusPending, reason); err != nil {
		return err
	}
	for _, t := range x.m.rows() {
		if t.ID != id && (t.Status != core.StatusPending || (t.NotBefore != nil && !t.NotBefore.Before(until))) {
			continue
		}
		nb := until
		p := &nb
		if err := x.update(t.ID, state.TaskUpdate{NotBefore: &p}); err != nil {
			return err
		}
	}
	return nil
}

// retryPath (inside a tx): back to pending with backoff while attempts
// remain, else failed. Memory's attempts_used counts the attempt just ended.
func (r *Reconciler) retryPath(x *txn, id, from, failure string) error {
	failure = truncate(failure)
	zero := 0
	if r.cfg.Budget != nil {
		block, err := r.cfg.Budget.Retry(x.tx, *x.m.tasks[id])
		if err != nil {
			return err
		}
		if block != "" { // SAME_FAILURE: a human moment, not another retry
			block = truncate(block)
			if err := x.transition(id, from, core.StatusBlocked, block); err != nil {
				return err
			}
			return x.update(id, state.TaskUpdate{BlockedReason: &block, LastFailure: &failure, VerifyErrors: &zero})
		}
	}
	used := x.m.tasks[id].AttemptsUsed
	if used < r.cfg.MaxAttempts {
		if err := x.transition(id, from, core.StatusPending, "retry: "+firstLine(failure)); err != nil {
			return err
		}
		nb := x.now.Add(r.cfg.RetryDelay(used))
		p := &nb
		return x.update(id, state.TaskUpdate{NotBefore: &p, LastFailure: &failure, VerifyErrors: &zero})
	}
	reason := fmt.Sprintf("attempts exhausted (%d/%d): %s", used, r.cfg.MaxAttempts, firstLine(failure))
	if err := x.transition(id, from, core.StatusFailed, reason); err != nil {
		return err
	}
	return x.update(id, state.TaskUpdate{LastFailure: &failure, VerifyErrors: &zero})
}

func (r *Reconciler) reconcileRunning(ctx context.Context, snap *memState, t state.TaskRow) (Result, error) {
	r.mu.Lock()
	lt := r.live[t.ID]
	var exited, stallKill bool
	var exit core.Exit
	var quietUntil time.Time
	if lt != nil {
		exited, exit, stallKill, quietUntil = lt.exited, lt.exit, lt.stallKill, lt.quietUntil
		if quietUntil.IsZero() {
			quietUntil = lt.lastLine.Add(r.cfg.StallTimeout)
		}
	}
	r.mu.Unlock()
	switch {
	case lt == nil:
		return r.recoverLost(ctx, snap, t)
	case exited:
		return r.handleExit(ctx, snap, t, lt, exit, stallKill)
	case stallKill && time.Since(lt.killedAt) > stallExitWait:
		// Killed for silence, and still no exit: record it stalled rather
		// than wait on a handle that may never return.
		return r.handleExit(ctx, snap, t, lt, core.Exit{Code: -1, Stalled: true}, true)
	case r.cfg.Now().After(quietUntil):
		// Backstop for the Supervisor's own stall timer; the exit path
		// records the outcome as stalled and Adds the key. Look again
		// shortly in case it never does.
		r.mu.Lock()
		if !lt.stallKill {
			lt.stallKill, lt.killedAt = true, time.Now()
		}
		r.mu.Unlock()
		_ = lt.h.Kill()
		return Result{RequeueAfter: stallRecheck}, nil
	default:
		return Result{RequeueAfter: r.cfg.LeaseCheck}, nil
	}
}

// handleExit records a finished worker and routes the task. The head is
// resolved here, once, and kept in memory: verify and integration use this
// SHA, never the branch ref or the DB.
func (r *Reconciler) handleExit(ctx context.Context, snap *memState, t state.TaskRow, lt *liveTask, exit core.Exit, stallKill bool) (Result, error) {
	outcome := "exited"
	switch {
	case exit.Stalled || stallKill:
		outcome = "stalled"
	case exit.Killed:
		outcome = "killed"
	}
	var head string
	headErr := errors.New("task has no branch")
	if t.Branch != "" {
		head, headErr = r.cfg.Workspace.Head(ctx, r.cfg.Project, t.Branch)
	}
	res, found, perr := streamjson.LastResult(lt.streamPath)
	if perr != nil {
		found = false
	}
	blockedReason, blocked := "", false
	if found {
		blockedReason, blocked = res.BlockedRequest()
	}
	code := exit.Code
	// A usage-limit refusal is the plan's window, not the worker: the attempt
	// is given back and the task waits for the reset (usageLimitPark).
	usageLimited := found && res.UsageLimited() && code != 0
	envWhy := ""
	if !found && code != 0 && !exit.Stalled && !stallKill && !exit.Killed {
		envWhy = envWorkerFailure(code, strings.TrimSuffix(lt.streamPath, ".stream.jsonl")+".stderr")
	}
	var usageUntil time.Time
	if usageLimited {
		usageUntil = r.cfg.Now().Add(r.cfg.UsageLimitBackoff)
		if at, ok, _ := streamjson.LastRejectedReset(lt.streamPath); ok {
			if t := time.Unix(at, 0).Add(r.cfg.UsageResetSlack); t.After(r.cfg.Now()) {
				usageUntil = t
			}
		}
	}
	end := state.AttemptEnd{
		Outcome: outcome, ExitCode: &code, BlockedRequest: blockedReason, HeadSHA: head,
	}
	if found {
		end.ResultSubtype, end.IsError, end.NumTurns = res.Subtype, res.IsError, res.NumTurns
		end.DurationMS, end.CostUSDEstimate, end.Usage = res.DurationMS, res.CostUSDEstimate, res.Usage
	}
	r.mu.Lock()
	budgetStop := lt.budgetStop
	r.mu.Unlock()
	stopReason := ""
	if budgetStop {
		end.Outcome = "killed"
		r.cfg.bst.mu.Lock()
		stopReason = r.cfg.bst.halt.Reason
		r.cfg.bst.mu.Unlock()
	}
	toVerify := false
	err := r.apply(ctx, func(x *txn) error {
		if err := state.FinishAttempt(x.tx, x.now, lt.ref, end); err != nil {
			return err
		}
		if r.cfg.Budget != nil {
			if err := r.cfg.Budget.Settle(x.tx, lt.ref, end); err != nil {
				return err
			}
		}
		if budgetStop {
			// Budget stop: refund the attempt and park the task.
			// Never retryPath: no attempt consumed, no backoff, not failed;
			// not_before and last_failure stay as they are.
			if err := x.refund(t.ID); err != nil {
				return err
			}
			return x.transition(t.ID, core.StatusRunning, core.StatusPending, "budget stop: "+stopReason)
		}
		if headErr == nil {
			if err := x.update(t.ID, state.TaskUpdate{HeadSHA: &head}); err != nil {
				return err
			}
		}
		switch {
		case usageLimited:
			return r.usageLimitPark(x, t.ID, usageUntil, firstLine(res.Result))
		case envWhy != "":
			return r.envPark(x, t.ID, core.StatusRunning, envWhy)
		case blocked:
			reason := truncate("GOSPLAN_BLOCKED: " + blockedReason)
			if err := x.transition(t.ID, core.StatusRunning, core.StatusBlocked, reason); err != nil {
				return err
			}
			if err := x.update(t.ID, state.TaskUpdate{BlockedReason: &reason}); err != nil {
				return err
			}
			return state.AppendEvent(x.tx, x.now, r.cfg.Run, t.ID, events.Blocker, events.BlockerData{Reason: blockedReason})
		case outcome == "exited" && code == 0 && headErr == nil:
			toVerify = true
			zero := 0
			if err := x.transition(t.ID, core.StatusRunning, core.StatusVerifying, ""); err != nil {
				return err
			}
			return x.update(t.ID, state.TaskUpdate{VerifyErrors: &zero})
		default:
			return r.retryPath(x, t.ID, core.StatusRunning, describeExit(lt.ref.Attempt, outcome, code, res, found, headErr, r.cfg.StallTimeout))
		}
	})
	if errors.Is(err, ErrBudgetTampered) {
		r.halt(ctx, ReasonTampered+": budget: "+err.Error())
		return Result{}, errHalted
	}
	if err != nil {
		return Result{}, err
	}
	r.mu.Lock()
	delete(r.live, t.ID)
	r.mu.Unlock()
	if envWhy != "" {
		r.halt(ctx, ReasonEnvironment+": task "+t.ID+": "+envWhy)
		return Result{}, errHalted
	}
	if toVerify {
		r.queue.Add(taskKey(t.ID)) // start the verify at once
	} else {
		r.addPending(snap, t.ID)
	}
	return Result{}, r.afterStep(ctx)
}

func describeExit(n int, outcome string, code int, res streamjson.Result, found bool, headErr error, stall time.Duration) string {
	var s string
	switch {
	case outcome == "stalled":
		s = fmt.Sprintf("attempt %d stalled: no output for %s, process group killed", n, stall)
	case outcome == "killed":
		s = fmt.Sprintf("attempt %d was killed", n)
	case code != 0:
		s = fmt.Sprintf("attempt %d exited with code %d", n, code)
	default:
		s = fmt.Sprintf("attempt %d: could not resolve the branch head: %v", n, headErr)
	}
	if found && strings.TrimSpace(res.Result) != "" {
		s += "\nfinal message (tail):\n" + tailBytes(res.Result, 1000)
	}
	return s
}

// newNonce is "<run>.<16 hex>": unique per attempt, and its run prefix lets
// a restart find every worker of the run by environment.
func (r *Reconciler) newNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return r.cfg.Run + "." + hex.EncodeToString(b[:]), nil
}

// withNonce is env with GOSPLAN_ATTEMPT_NONCE set to nonce (any other value
// removed).
func withNonce(env []string, nonce string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, driver.EnvNonce+"=") {
			out = append(out, kv)
		}
	}
	return append(out, driver.EnvNonce+"="+nonce)
}

// killLeaseGroups (resume, before sweepOrphans) kills the process group of
// every lease another orchestrator left, under KillVerified's identity
// rules. It runs first because the sweep kills the nonce holders, and a
// group whose last nonce holder is gone can no longer be told from a
// reused one.
func (r *Reconciler) killLeaseGroups(ctx context.Context) error {
	leases, err := r.cfg.Store.Leases(ctx, r.cfg.Run)
	if err != nil {
		return err
	}
	for _, l := range leases {
		if l.Owner != r.owner {
			driver.KillVerified(l.PID, l.PGID, l.StartTime, l.Nonce, r.cfg.Run)
		}
	}
	return nil
}

// sweepOrphans kills live processes of this run that carry an attempt
// nonce while this process has no workers yet (resume): workers of a
// crashed orchestrator, including any started between Launch and
// SetLeaseProcess that no lease row names a pid for, and its `gosplan verify`
// runs (nonce "<run>.verify.<task>.<n>"). A verify gets SIGTERM first:
// gosplan verify turns it into a cancel and kills its gate commands' groups,
// which run with a scrubbed environment (no nonce) and would survive a
// SIGKILL of gosplan verify alone. Whatever is left after stragglerGrace is
// SIGKILLed. It returns once every holder is gone or killed.
func (r *Reconciler) sweepOrphans() {
	var verifies []int
	for pid, nonce := range driver.NonceHolders(r.cfg.Run + ".") {
		if strings.HasPrefix(nonce, r.cfg.Run+".verify.") {
			if syscall.Kill(pid, syscall.SIGTERM) == nil {
				verifies = append(verifies, pid)
			}
			continue
		}
		driver.KillHolder(pid)
	}
	deadline := time.Now().Add(stragglerGrace)
	for len(verifies) > 0 {
		left := verifies[:0]
		for _, pid := range verifies {
			if driver.Alive(pid) {
				left = append(left, pid)
			}
		}
		verifies = left
		if len(verifies) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, pid := range verifies {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stragglerGrace is how long a crashed run's gosplan verify gets after SIGTERM
// to cancel its gates (verifyexec.KillGrace, the same bound gosplan run itself
// gives a verify it stops).
const stragglerGrace = 10 * time.Second

// recoverLost handles a running task with no live handle in this process.
// The attempt comes from memory; the lease row only supplies the process to
// kill, and only when it names that same attempt. The kill happens only if
// the leader's /proc start time matches the lease or its environment
// carries the lease's nonce; otherwise (a reused pid, another user's
// process, EPERM) nothing is killed and the attempt still ends lost.
func (r *Reconciler) recoverLost(ctx context.Context, snap *memState, t state.TaskRow) (Result, error) {
	ref, haveRef := snap.attempt[t.ID]
	leases, err := r.cfg.Store.Leases(ctx, r.cfg.Run)
	if err != nil {
		return Result{}, err
	}
	var lease *state.LeaseRow
	for i := range leases {
		if leases[i].Task == t.ID && haveRef && leases[i].AttemptID == ref.AttemptID {
			lease = &leases[i]
		}
	}
	if lease != nil && lease.Owner != r.owner {
		driver.KillVerified(lease.PID, lease.PGID, lease.StartTime, lease.Nonce, r.cfg.Run)
	}
	err = r.apply(ctx, func(x *txn) error {
		if !haveRef {
			return r.retryPath(x, t.ID, core.StatusRunning,
				fmt.Sprintf("attempt %d ended without a recorded exit", t.AttemptsUsed))
		}
		end := state.AttemptEnd{Outcome: "lost"}
		if err := state.FinishAttempt(x.tx, x.now, ref, end); err != nil {
			return err
		}
		if r.cfg.Budget != nil {
			if err := r.cfg.Budget.Settle(x.tx, ref, end); err != nil {
				return err
			}
		}
		if err := x.refund(t.ID); err != nil {
			return err
		}
		owner := "unknown"
		if lease != nil {
			owner = lease.Owner
		}
		return r.retryPath(x, t.ID, core.StatusRunning,
			fmt.Sprintf("attempt %d lost: lease owner %s has no live worker in this orchestrator", ref.Attempt, owner))
	})
	if errors.Is(err, ErrBudgetTampered) {
		r.halt(ctx, ReasonTampered+": budget: "+err.Error())
		return Result{}, errHalted
	}
	if err != nil {
		return Result{}, err
	}
	if haveRef && r.noteInfra(ctx, t.ID, fmt.Sprintf("%s attempt %d lost", t.ID, ref.Attempt)) {
		return Result{}, errHalted
	}
	r.addPending(snap, t.ID)
	return Result{}, r.afterStep(ctx)
}

type verdictKind int

const (
	verdictError verdictKind = iota
	verdictPass
	verdictFail
)

// classify maps a verifier outcome. Anything but a consistent pass or fail
// is could-not-verify, never a pass.
func classify(out core.VerifyOutcome, err error) verdictKind {
	switch {
	case err != nil:
		return verdictError
	case out.Exit == 0 && out.Result == "pass" && len(out.FailedGates) == 0:
		return verdictPass
	case out.Exit == 1 && out.Result == "fail":
		return verdictFail
	default:
		return verdictError
	}
}

// verdictNames reports whether the verdict JSON is about exactly base and
// head. A pass for any other pair is not this attempt's pass.
func verdictNames(raw []byte, base, head string) error {
	var v struct {
		BaseSHA string `json:"base_sha"`
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("verdict JSON: %w", err)
	}
	if v.BaseSHA != base || v.HeadSHA != head {
		return fmt.Errorf("verdict is for %s..%s, the orchestrator asked about %s..%s", v.BaseSHA, v.HeadSHA, base, head)
	}
	return nil
}

func (r *Reconciler) reconcileVerifying(ctx context.Context, snap *memState, t state.TaskRow) (Result, error) {
	now := r.cfg.Now()
	r.mu.Lock()
	vt := r.verifies[t.ID]
	at := r.verifyAt[t.ID]
	var done bool
	var out core.VerifyOutcome
	var verr error
	if vt != nil {
		done, out, verr = vt.done, vt.out, vt.err
	}
	r.mu.Unlock()

	if vt == nil {
		if at.After(now) {
			return Result{RequeueAfter: at.Sub(now)}, nil
		}
		ref, ok := snap.attempt[t.ID]
		if !ok {
			return Result{}, fmt.Errorf("reconcile: task %s is verifying with no attempt", t.ID)
		}
		base, err := r.computeBase(ctx, snap, t)
		if err != nil {
			return Result{}, err
		}
		if base != t.BaseSHA {
			r.halt(ctx, fmt.Sprintf("%s: task %s: recomputed base %s differs from the base its attempt ran on (%s)",
				ReasonInconsistent, t.ID, base, t.BaseSHA))
			return Result{}, errHalted
		}
		vt = &verifyTask{
			ref: ref,
			req: core.VerifyRequest{
				Project: r.cfg.Project, Run: r.cfg.Run, Task: t.ID, Attempt: ref.Attempt,
				Base: base, Head: t.HeadSHA, Scope: t.Scope, AllowNewProtected: t.AllowNewProtected,
				EventLog: r.cfg.EventLog,
				LogsDir:  filepath.Join(r.cfg.RunDir, "verify-logs", t.ID, fmt.Sprint(ref.Attempt)),
			},
		}
		r.mu.Lock()
		r.verifies[t.ID] = vt
		delete(r.verifyAt, t.ID)
		r.mu.Unlock()
		r.bg.Add(1)
		go func() {
			defer r.bg.Done()
			o, err := r.cfg.Verifier.Verify(ctx, vt.req)
			r.mu.Lock()
			vt.out, vt.err, vt.done = o, err, true
			r.mu.Unlock()
			r.queue.Add(taskKey(t.ID))
		}()
		return Result{}, nil
	}
	if !done {
		return Result{}, nil // the verify goroutine Adds the key
	}

	kind := classify(out, verr)
	if verr != nil {
		out = core.VerifyOutcome{Exit: 2, Detail: verr.Error()}
	}
	if kind == verdictPass {
		if err := verdictNames(out.VerdictJSON, vt.req.Base, vt.req.Head); err != nil {
			kind, out.Detail = verdictError, err.Error()
		}
	}
	detail := out.Detail
	if kind == verdictError && detail == "" {
		detail = fmt.Sprintf("verifier exit %d, result %q", out.Exit, out.Result)
	}
	retryVerify := false
	envWhy := ""
	if kind == verdictFail {
		envWhy = envGateFailure(out.VerdictJSON)
	}
	verrs, prevVerrs := 0, 0
	err := r.apply(ctx, func(x *txn) error {
		rec := state.VerdictRecord{VerdictJSON: out.VerdictJSON, FailedGates: out.FailedGates, HeadSHA: vt.req.Head}
		switch kind {
		case verdictPass:
			rec.Result = "pass"
		case verdictFail:
			rec.Result = "fail"
		default:
			rec.Result = "error"
		}
		if err := state.SetVerdict(x.tx, vt.ref, rec); err != nil {
			return err
		}
		x.m.verdicts[t.ID] = verdictMem{attempt: vt.ref.Attempt, result: rec.Result, base: vt.req.Base, head: vt.req.Head, tree: out.TreeSHA}
		// Run-wide consecutive could-not-verify count: +1 on error, reset by a pass or a fail.
		prevVerrs = r.cfg.bst.verifyErrs
		if rec.Result == "error" {
			verrs = prevVerrs + 1
		}
		r.cfg.bst.verifyErrs = verrs
		if r.cfg.Budget != nil {
			if err := r.cfg.Budget.Verdict(x.tx, vt.ref, rec.Result, verrs, verrs >= r.cfg.MaxVerifyErrors); err != nil {
				return err
			}
		}
		switch kind {
		case verdictPass:
			zero := 0
			if err := x.transition(t.ID, core.StatusVerifying, core.StatusPassed, ""); err != nil {
				return err
			}
			return x.update(t.ID, state.TaskUpdate{VerifyErrors: &zero})
		case verdictFail:
			if envWhy != "" {
				return r.envPark(x, t.ID, core.StatusVerifying, envWhy)
			}
			failure := fmt.Sprintf("attempt %d failed verification.\nfailed gates: %s\n%s",
				vt.ref.Attempt, strings.Join(out.FailedGates, ", "), detail)
			return r.retryPath(x, t.ID, core.StatusVerifying, failure)
		default:
			n := x.m.tasks[t.ID].VerifyErrors + 1
			if err := x.update(t.ID, state.TaskUpdate{VerifyErrors: &n}); err != nil {
				return err
			}
			if n < verifyErrorLimit {
				retryVerify = true
				return nil
			}
			reason := truncate("could not verify: " + detail)
			if err := x.transition(t.ID, core.StatusVerifying, core.StatusBlocked, reason); err != nil {
				return err
			}
			return x.update(t.ID, state.TaskUpdate{BlockedReason: &reason})
		}
	})
	if err != nil {
		// Rolled back: the count did not happen.
		r.stateMu.Lock()
		if r.cfg.bst.verifyErrs == verrs {
			r.cfg.bst.verifyErrs = prevVerrs
		}
		r.stateMu.Unlock()
	}
	if errors.Is(err, ErrBudgetTampered) {
		r.halt(ctx, ReasonTampered+": budget: "+err.Error())
		return Result{}, errHalted
	}
	if errors.Is(err, core.ErrConflict) {
		r.mu.Lock()
		delete(r.verifies, t.ID)
		r.mu.Unlock()
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	r.mu.Lock()
	delete(r.verifies, t.ID)
	if retryVerify {
		r.verifyAt[t.ID] = now.Add(r.cfg.VerifyRetryDelay)
	}
	r.mu.Unlock()
	if r.cfg.OnVerdict != nil {
		r.cfg.OnVerdict(ctx, vt.ref, vt.req, out)
	}
	if envWhy != "" {
		r.halt(ctx, ReasonEnvironment+": task "+t.ID+": "+envWhy)
		return Result{}, errHalted
	}
	if verrs >= r.cfg.MaxVerifyErrors {
		// Nothing is trustworthy when the verifier cannot run.
		r.halt(ctx, fmt.Sprintf("%s: %d consecutive could-not-verify results; last: %s", ReasonVerifierError, verrs, firstLine(detail)))
		return Result{}, errHalted
	}
	if retryVerify {
		return Result{RequeueAfter: r.cfg.VerifyRetryDelay}, r.afterStep(ctx)
	}
	r.addPending(snap, t.ID)
	return Result{}, r.afterStep(ctx)
}

var badStatus = map[string]bool{core.StatusFailed: true, core.StatusBlocked: true, core.StatusAbandoned: true}

// reconcileRun is run completion, plus the tamper check, the
// lock and liveness watchdogs, and integration.
func (r *Reconciler) reconcileRun(ctx context.Context) (Result, error) {
	if r.isHalted() || r.snapshot().run.Status != core.RunRunning {
		r.signalDone()
		return Result{}, nil
	}
	r.busyExpired(ctx)
	if err := r.check(ctx); err != nil {
		if errors.Is(err, errHalted) {
			return Result{}, nil
		}
		return Result{RequeueAfter: pollInterval}, nil
	}
	if r.cfg.Budget != nil {
		// Budget run key: Tick outside any tx, then one
		// apply re-arming this run's parked tasks through txn.
		h, err := r.cfg.Budget.Tick(ctx, r.cfg.Now())
		if errors.Is(err, ErrBudgetTampered) {
			r.halt(ctx, ReasonTampered+": budget: "+err.Error())
			return Result{}, nil
		}
		var stop []*liveTask
		if err == nil && h.Kind != "" {
			r.cfg.bst.mu.Lock()
			if r.cfg.bst.halt.Kind == "" {
				r.cfg.bst.halt = h // the first halt wins
			}
			if r.cfg.bst.halt.Kind == "stop" && !r.cfg.bst.stopped {
				r.cfg.bst.stopped = true
				r.mu.Lock()
				for _, lt := range r.live {
					if !lt.exited {
						lt.budgetStop = true
						stop = append(stop, lt)
					}
				}
				r.mu.Unlock()
			}
			r.cfg.bst.mu.Unlock()
		}
		for _, lt := range stop {
			_ = lt.h.Kill() // handleExit refunds and parks it
		}
		var rearmed []string
		err = r.apply(ctx, func(x *txn) error {
			rearmed = nil
			nbs, err := r.cfg.Budget.Rearm(x.tx, x.now)
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(nbs))
			for id := range nbs {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			for _, id := range ids {
				if m := x.m.tasks[id]; m == nil || m.Status != core.StatusPending {
					continue
				}
				nb := nbs[id]
				p := &nb
				if err := x.update(id, state.TaskUpdate{NotBefore: &p}); err != nil {
					return err
				}
				rearmed = append(rearmed, id)
			}
			return nil
		})
		switch {
		case errors.Is(err, ErrBudgetTampered):
			r.halt(ctx, ReasonTampered+": budget: "+err.Error())
			return Result{}, nil
		case errors.Is(err, errHalted):
			return Result{}, nil
		case err == nil:
			for _, id := range rearmed {
				r.queue.Add(taskKey(id))
			}
		}
	}
	r.cfg.bst.mu.Lock()
	bhalt := r.cfg.bst.halt
	r.cfg.bst.mu.Unlock()
	snap := r.snapshot()
	if reason := r.liveness(snap); reason != "" {
		r.halt(ctx, reason)
		return Result{}, nil
	}
	if reason := r.diskLow(); reason != "" {
		r.halt(ctx, reason)
		return Result{}, nil
	}
	now := r.cfg.Now()
	tasks := snap.rows()
	for _, t := range tasks {
		switch {
		case t.Status == core.StatusRunning, t.Status == core.StatusVerifying:
			return Result{RequeueAfter: pollInterval}, nil
		case t.Status != core.StatusPending:
		case bhalt.Kind != "":
			// A budget halt is set: a pending task is not ready.
		case t.NotBefore != nil && t.NotBefore.After(now):
			return Result{RequeueAfter: pollInterval}, nil
		case depsPassed(snap, t):
			return Result{RequeueAfter: pollInterval}, nil
		}
	}

	// Pending tasks with a failed/blocked/abandoned ancestor are abandoned;
	// the reason names the first such ancestor in (tier, id) order.
	sp := &spec.Spec{}
	for _, t := range tasks {
		sp.Tasks = append(sp.Tasks, spec.Task{ID: t.ID, Tier: t.Tier, Deps: t.Deps})
	}
	type abandon struct{ id, reason string }
	var abandons []abandon
	seen := map[string]bool{}
	for _, t := range tasks {
		if !badStatus[t.Status] {
			continue
		}
		for _, d := range sp.Descendants(t.ID) {
			if snap.tasks[d].Status == core.StatusPending && !seen[d] {
				seen[d] = true
				abandons = append(abandons, abandon{d, fmt.Sprintf("dependency %s is %s", t.ID, t.Status)})
			}
		}
	}
	var notPassed []string
	for _, t := range tasks {
		st := t.Status
		if seen[t.ID] {
			st = core.StatusAbandoned
		}
		if st != core.StatusPassed {
			notPassed = append(notPassed, fmt.Sprintf("%s (%s)", t.ID, st))
		}
	}
	final, reason := core.RunComplete, ""
	if len(notPassed) > 0 {
		final, reason = core.RunHalted, "not passed: "+strings.Join(notPassed, ", ")
	}
	var parked []string
	if bhalt.Kind != "" {
		// Budget halt: pending tasks that are not abandoned stay pending
		// (parked) so a later `gosplan run --run ID` can resume them.
		for _, t := range tasks {
			if t.Status == core.StatusPending && !seen[t.ID] {
				parked = append(parked, t.ID)
			}
		}
		slices.Sort(parked)
		ids := "none"
		if len(parked) > 0 {
			ids = strings.Join(parked, ", ")
		}
		final, reason = core.RunHalted, bhalt.Reason+": parked "+ids
	}

	tip, conflict, err := r.integrate(ctx, snap)
	if errors.Is(err, errHalted) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	if conflict != nil {
		// Exit 0 means every task passed and was integrated: a conflict
		// blocks the task and halts the run.
		final, reason = core.RunHalted, conflict.reason()
		if len(notPassed) > 0 {
			reason += "; not passed: " + strings.Join(notPassed, ", ")
		}
	}

	err = r.apply(ctx, func(x *txn) error {
		if countActive(x.m) > 0 {
			return core.ErrConflict
		}
		for _, t := range x.m.rows() {
			if t.Status == core.StatusPending && bhalt.Kind == "" && depsPassed(x.m, t) && (t.NotBefore == nil || !t.NotBefore.After(x.now)) {
				return core.ErrConflict
			}
		}
		for _, a := range abandons {
			if err := x.transition(a.id, core.StatusPending, core.StatusAbandoned, a.reason); err != nil {
				return err
			}
			reason := a.reason
			if err := x.update(a.id, state.TaskUpdate{BlockedReason: &reason}); err != nil {
				return err
			}
		}
		if bhalt.Kind != "" {
			if _, err := state.AppendBudgetEvent(x.tx, x.now, r.cfg.Run, "", budgetlog.TypeHalt,
				budget.Halt{Kind: bhalt.Kind, Reason: bhalt.Reason, Parked: append([]string{}, parked...)}); err != nil {
				return err
			}
		}
		if conflict != nil {
			why := truncate(conflict.reason())
			if err := x.transition(conflict.task, core.StatusPassed, core.StatusBlocked, why); err != nil {
				return err
			}
			if err := x.update(conflict.task, state.TaskUpdate{BlockedReason: &why}); err != nil {
				return err
			}
			if err := state.AppendEvent(x.tx, x.now, r.cfg.Run, conflict.task, events.Blocker, events.BlockerData{Reason: why}); err != nil {
				return err
			}
		}
		if tip != "" {
			if err := state.SetIntegrationTx(x.tx, r.cfg.Run, r.cfg.IntegrationBranch, tip); err != nil {
				return err
			}
			x.m.run.IntegrationBranch, x.m.run.IntegrationSHA = r.cfg.IntegrationBranch, tip
		}
		if err := x.finishRun(final, withIntegration(reason, tip)); err != nil {
			return err
		}
		r.finalReason = reason // stateMu is held by apply
		return nil
	})
	if errors.Is(err, errHalted) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	if err := r.flush(ctx); err != nil {
		return Result{}, err
	}
	r.signalDone()
	return Result{}, nil
}

// liveness names tasks that have been stuck for ProgressTimeout: running
// with no worker in this process (or a worker whose exit was never
// recorded), verifying with no verify (or a finished verify whose verdict
// was never recorded), or ready with a free slot but never dispatched.
// It is the backstop behind noteWrite, which halts a failing write sooner. A stuck run halts no_progress instead of
// idling all night.
func (r *Reconciler) liveness(snap *memState) string {
	now := time.Now()
	active := countActive(snap)
	r.cfg.bst.mu.Lock()
	budgetHalted := r.cfg.bst.halt.Kind != "" // a pending task is not ready
	r.cfg.bst.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	var stuck []string
	for _, t := range snap.rows() {
		why := ""
		switch t.Status {
		case core.StatusRunning:
			if lt := r.live[t.ID]; lt == nil {
				why = "running with no worker"
			} else if lt.exited {
				why = "exit could not be recorded"
			}
		case core.StatusVerifying:
			if vt := r.verifies[t.ID]; vt == nil && !r.verifyAt[t.ID].After(r.cfg.Now()) {
				why = "verifying with no verify running"
			} else if vt != nil && vt.done {
				why = "verdict could not be recorded"
			}
		case core.StatusPending:
			if !budgetHalted && (t.NotBefore == nil || !t.NotBefore.After(r.cfg.Now())) && depsPassed(snap, t) && active < r.cfg.MaxParallel {
				why = "ready but not dispatched"
			}
		}
		if why == "" {
			delete(r.stuckSince, t.ID)
			continue
		}
		since, ok := r.stuckSince[t.ID]
		if !ok {
			r.stuckSince[t.ID] = now
			continue
		}
		if now.Sub(since) > r.cfg.ProgressTimeout {
			stuck = append(stuck, fmt.Sprintf("%s (%s for %s)", t.ID, why, now.Sub(since).Round(time.Second)))
		}
	}
	if len(stuck) == 0 {
		return ""
	}
	return ReasonNoProgress + ": " + strings.Join(stuck, ", ")
}

// withIntegration appends the integration tip to a run_finished reason, so
// the event log carries it for the resume cross-check.
func withIntegration(reason, tip string) string {
	if tip == "" {
		return reason
	}
	if reason == "" {
		return "integration_sha=" + tip
	}
	return reason + "; integration_sha=" + tip
}

// integrationConflict is a passed task whose head could not be merged.
type integrationConflict struct {
	task, head    string
	paths         []string
	notIntegrated []string // this task and every passed task after it
}

func (c *integrationConflict) reason() string {
	return fmt.Sprintf("%s: task %s (head %s) conflicts with the integration branch on: %s; not integrated: %s",
		ReasonIntegrationConflict, c.task, short(c.head), strings.Join(c.paths, ", "), strings.Join(c.notIntegrated, ", "))
}

// integrate merges the passed tasks' verified heads, in (tier, id) order,
// into the integration branch from the run base. Only a task whose
// in-memory verdict is a pass for exactly its recomputed base and its
// resolved head is merged, and it is merged by that SHA. Afterwards the tip
// is recomputed from the same SHAs and the branch re-resolved; both must
// equal what Integrate returned. On a merge conflict the branch stays at
// the last good merge and the conflict is returned: the caller blocks that
// task and halts the run.
func (r *Reconciler) integrate(ctx context.Context, snap *memState) (string, *integrationConflict, error) {
	var heads, ids []string
	for _, t := range snap.rows() {
		if t.Status != core.StatusPassed {
			continue
		}
		v, ok := snap.verdicts[t.ID]
		if !ok || v.result != "pass" || v.head != t.HeadSHA || v.base != t.BaseSHA {
			r.halt(ctx, fmt.Sprintf("%s: task %s is passed without a pass verdict from this orchestrator for %s..%s",
				ReasonTampered, t.ID, t.BaseSHA, t.HeadSHA))
			return "", nil, errHalted
		}
		base, err := r.computeBase(ctx, snap, t)
		if err != nil {
			return "", nil, err
		}
		if base != v.base {
			r.halt(ctx, fmt.Sprintf("%s: task %s: recomputed base %s differs from its verdict's base %s", ReasonInconsistent, t.ID, base, v.base))
			return "", nil, errHalted
		}
		heads, ids = append(heads, v.head), append(ids, t.ID)
	}
	branch := r.cfg.IntegrationBranch
	tip, err := r.cfg.Workspace.Integrate(ctx, r.cfg.Project, branch, snap.run.BaseSHA, heads)
	merged := heads
	var conflict *integrationConflict
	if errors.Is(err, core.ErrMergeConflict) {
		var ci conflictInfo
		i := -1
		if errors.As(err, &ci) {
			i = slices.Index(heads, ci.ConflictHead())
		}
		if i < 0 {
			r.halt(ctx, fmt.Sprintf("%s: %v", ReasonIntegrationConflict, err))
			return "", nil, errHalted
		}
		conflict = &integrationConflict{task: ids[i], head: heads[i], paths: ci.ConflictPaths(), notIntegrated: ids[i:]}
		merged = heads[:i]
	} else if err != nil {
		r.halt(ctx, fmt.Sprintf("%s: integrating %s: %v", ReasonIntegrationFailed, branch, err))
		return "", nil, errHalted
	}
	want, err := r.base.ComputeBase(ctx, r.cfg.Project, snap.run.BaseSHA, merged)
	if err != nil {
		return "", nil, err
	}
	if conflict != nil {
		tip = want
	}
	got, err := r.cfg.Workspace.Head(ctx, r.cfg.Project, branch)
	if err != nil {
		return "", nil, err
	}
	if tip != want || got != tip {
		r.halt(ctx, fmt.Sprintf("%s: integration: Integrate returned %s, the verified heads give %s, branch %s is at %s",
			ReasonInconsistent, tip, want, branch, got))
		return "", nil, errHalted
	}
	return tip, conflict, nil
}

func (r *Reconciler) signalDone() { r.finishOnce.Do(func() { close(r.finished) }) }

// addPending Adds self and every task that was pending in the snapshot: a
// slot freed or a dependency may have passed, and self may now be pending
// with a backoff to wait out.
func (r *Reconciler) addPending(snap *memState, self string) {
	r.queue.Add(taskKey(self))
	for _, t := range snap.tasks {
		if t.Status == core.StatusPending {
			r.queue.Add(taskKey(t.ID))
		}
	}
}

// afterStep runs after every committed task step: flush the outbox, write
// state.json, and Add the run key.
func (r *Reconciler) afterStep(ctx context.Context) error {
	err := r.flush(ctx)
	r.stateMu.Lock()
	halted := r.noteWrite(ctx, "journal", err)
	r.stateMu.Unlock()
	r.queue.Add(runKey)
	if halted {
		return errHalted
	}
	return err
}

func (r *Reconciler) flush(ctx context.Context) error {
	if _, err := r.cfg.Store.FlushEvents(ctx, r.cfg.Run, r.cfg.EventLog); err != nil {
		return err
	}
	return r.cfg.Store.WriteStateJSON(ctx, r.cfg.Run, filepath.Join(r.cfg.RunDir, "state.json"))
}

func truncate(s string) string { return headBytes(s, maxFailureBytes) }

// headBytes is at most n bytes of s from the start, and tailBytes from the
// end, never splitting a UTF-8 sequence.
func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return headBytes(s, 200)
}
