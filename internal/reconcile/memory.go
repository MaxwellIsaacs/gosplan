package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// Halt reasons. Each is the first word of the run_finished reason.
const (
	ReasonTampered     = "state_tampered"     // the DB differs from what this orchestrator wrote
	ReasonInconsistent = "state_inconsistent" // the final self-check (replay, integration tip) failed
	ReasonLocked       = "state_locked"       // the DB stayed locked past Config.LockTimeout
	ReasonNoProgress   = "no_progress"        // tasks stuck with nothing running, verifying or waiting
	ReasonWriteFailed  = "state_write_failed" // one step's write kept failing (not busy) past WriteFailTimeout / MaxWriteFailures

	ReasonIntegrationConflict = "integration_conflict" // a passed head does not merge into the integration branch
	ReasonIntegrationFailed   = "integration_failed"   // git failed while integrating
	ReasonInfrastructure      = "infrastructure"       // workers repeatedly never started or were lost
)

// errHalted is returned once the reconciler has halted the run itself.
var errHalted = errors.New("reconcile: run halted")

// errTampered aborts a transaction whose pre-check found a divergence.
var errTampered = errors.New("reconcile: state tampered")

// verdictMem is the verdict this process got for one attempt, with the
// base and head it asked about.
type verdictMem struct {
	attempt          int
	result           string // pass | fail | error
	base, head, tree string
}

// memState is the orchestrator's own copy of the run. Workers run as the
// same uid and can write gosplan.db, so the DB is the journal and the
// crash-recovery source; decisions read this copy. Every transaction first
// compares the DB rows with it, and any difference halts the run.
type memState struct {
	run      state.RunRow
	tasks    map[string]*state.TaskRow
	order    []string                   // (tier, id)
	verdicts map[string]verdictMem      // task -> verdict of its current attempt
	attempt  map[string]core.AttemptRef // task -> its open (or latest) attempt
}

func newMemState(run state.RunRow, tasks []state.TaskRow) *memState {
	m := &memState{run: run, tasks: map[string]*state.TaskRow{}, verdicts: map[string]verdictMem{}, attempt: map[string]core.AttemptRef{}}
	for i := range tasks {
		t := tasks[i]
		m.tasks[t.ID] = &t
		m.order = append(m.order, t.ID)
	}
	return m
}

func (m *memState) clone() *memState {
	c := &memState{run: m.run, tasks: make(map[string]*state.TaskRow, len(m.tasks)), order: m.order,
		verdicts: make(map[string]verdictMem, len(m.verdicts)), attempt: make(map[string]core.AttemptRef, len(m.attempt))}
	for id, t := range m.tasks {
		x := *t
		c.tasks[id] = &x
	}
	for k, v := range m.verdicts {
		c.verdicts[k] = v
	}
	for k, v := range m.attempt {
		c.attempt[k] = v
	}
	return c
}

// rows returns copies of the task rows in (tier, id) order.
func (m *memState) rows() []state.TaskRow {
	out := make([]state.TaskRow, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, *m.tasks[id])
	}
	return out
}

// statuses is the {task: status} map replay compares against.
func (m *memState) statuses() map[string]string {
	out := map[string]string{}
	for id, t := range m.tasks {
		out[id] = t.Status
	}
	return out
}

// diff compares DB rows with memory and names every differing table, row
// and field. "" means they agree.
func (m *memState) diff(run state.RunRow, tasks []state.TaskRow) string {
	var out []string
	var rf []string
	field := func(fs *[]string, name string, db, mem any) {
		if !reflect.DeepEqual(db, mem) {
			*fs = append(*fs, fmt.Sprintf("%s db=%s mem=%s", name, show(db), show(mem)))
		}
	}
	field(&rf, "status", run.Status, m.run.Status)
	field(&rf, "base_sha", run.BaseSHA, m.run.BaseSHA)
	field(&rf, "project", run.Project, m.run.Project)
	field(&rf, "integration_branch", run.IntegrationBranch, m.run.IntegrationBranch)
	field(&rf, "integration_sha", run.IntegrationSHA, m.run.IntegrationSHA)
	if len(rf) > 0 {
		out = append(out, fmt.Sprintf("runs row %s: %s", m.run.ID, strings.Join(rf, ", ")))
	}
	seen := map[string]bool{}
	for _, d := range tasks {
		seen[d.ID] = true
		t := m.tasks[d.ID]
		if t == nil {
			out = append(out, fmt.Sprintf("tasks row %s: not part of this run", d.ID))
			continue
		}
		var fs []string
		field(&fs, "status", d.Status, t.Status)
		field(&fs, "tier", d.Tier, t.Tier)
		field(&fs, "prompt_path", d.PromptPath, t.PromptPath)
		field(&fs, "deps_source", d.DepsSource, t.DepsSource)
		field(&fs, "deps", d.Deps, t.Deps)
		field(&fs, "scope_json", d.Scope, t.Scope)
		field(&fs, "allow_new_protected", d.AllowNewProtected, t.AllowNewProtected)
		if !sameTime(d.NotBefore, t.NotBefore) {
			fs = append(fs, fmt.Sprintf("not_before db=%s mem=%s", showTime(d.NotBefore), showTime(t.NotBefore)))
		}
		field(&fs, "base_sha", d.BaseSHA, t.BaseSHA)
		field(&fs, "branch", d.Branch, t.Branch)
		field(&fs, "worktree", d.Worktree, t.Worktree)
		field(&fs, "head_sha", d.HeadSHA, t.HeadSHA)
		field(&fs, "attempts_used", d.AttemptsUsed, t.AttemptsUsed)
		field(&fs, "verify_errors", d.VerifyErrors, t.VerifyErrors)
		if d.LastFailure != t.LastFailure {
			fs = append(fs, "last_failure differs")
		}
		field(&fs, "blocked_reason", d.BlockedReason, t.BlockedReason)
		if len(fs) > 0 {
			out = append(out, fmt.Sprintf("tasks row %s: %s", d.ID, strings.Join(fs, ", ")))
		}
	}
	for _, id := range m.order {
		if !seen[id] {
			out = append(out, fmt.Sprintf("tasks row %s: deleted", id))
		}
	}
	return strings.Join(out, "; ")
}

func show(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return fmt.Sprintf("%q", s)
}

func showTime(t *time.Time) string {
	if t == nil {
		return "null"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// applyUpdate mirrors state.UpdateTask on a row.
func applyUpdate(t *state.TaskRow, u state.TaskUpdate) {
	if u.NotBefore != nil {
		if *u.NotBefore == nil {
			t.NotBefore = nil
		} else {
			// The DB keeps RFC 3339 text; keep what reading it back gives.
			nb, _ := time.Parse(time.RFC3339Nano, (**u.NotBefore).UTC().Format(time.RFC3339Nano))
			t.NotBefore = &nb
		}
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&t.BaseSHA, u.BaseSHA)
	set(&t.Branch, u.Branch)
	set(&t.Worktree, u.Worktree)
	set(&t.HeadSHA, u.HeadSHA)
	set(&t.LastFailure, u.LastFailure)
	set(&t.BlockedReason, u.BlockedReason)
	if u.AttemptsUsed != nil {
		t.AttemptsUsed = *u.AttemptsUsed
	}
	if u.VerifyErrors != nil {
		t.VerifyErrors = *u.VerifyErrors
	}
}

// txn is one transaction plus the memory draft it updates. The draft
// replaces memory only after the commit.
type txn struct {
	tx  *sql.Tx
	m   *memState
	run string
	now time.Time
}

// transition is state.Transition plus the draft. A from that memory does
// not hold is a conflict, whatever the DB says.
func (x *txn) transition(id, from, to, reason string) error {
	t := x.m.tasks[id]
	if t == nil || t.Status != from {
		return core.ErrConflict
	}
	if err := state.Transition(x.tx, x.now, x.run, id, from, to, reason); err != nil {
		return err
	}
	t.Status = to
	return nil
}

func (x *txn) update(id string, u state.TaskUpdate) error {
	if err := state.UpdateTask(x.tx, x.run, id, u); err != nil {
		return err
	}
	applyUpdate(x.m.tasks[id], u)
	return nil
}

// refund gives back the attempt that just ended: it never ran the worker
// (start_failed) or lost it to an orchestrator fault (lost).
func (x *txn) refund(id string) error {
	used := x.m.tasks[id].AttemptsUsed - 1
	if used < 0 {
		used = 0
	}
	return x.update(id, state.TaskUpdate{AttemptsUsed: &used})
}

func (x *txn) reopenRun() error {
	if err := state.ReopenRun(x.tx, x.run); err != nil {
		return err
	}
	x.m.run.Status = core.RunRunning
	return nil
}

func (x *txn) finishRun(status, reason string) error {
	if err := state.FinishRun(x.tx, x.now, x.run, status, reason); err != nil {
		return err
	}
	x.m.run.Status = status
	return nil
}

// apply runs fn in one IMMEDIATE transaction. The transaction first reads
// every task row and the run row and compares them with memory; on any
// difference it rolls back and the run halts state_tampered. fn changes the
// DB and the draft together; the draft becomes memory after the commit.
// stateMu is held for the whole call, so no other step sees memory and DB
// out of step.
func (r *Reconciler) apply(ctx context.Context, fn func(x *txn) error) error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.halted() {
		return errHalted
	}
	var next *memState
	var tamper string
	start := time.Now()
	err := r.cfg.Store.Immediate(ctx, func(tx *sql.Tx) error {
		run, err := state.GetRunTx(tx, r.cfg.Run)
		if err != nil {
			return err
		}
		tasks, err := state.ListTasksTx(tx, r.cfg.Run)
		if err != nil {
			return err
		}
		if tamper = r.mem.diff(run, tasks); tamper != "" {
			return errTampered
		}
		next = r.mem.clone()
		return fn(&txn{tx: tx, m: next, run: r.cfg.Run, now: r.cfg.Now()})
	})
	r.noteDB(ctx, start, err, true)
	switch {
	case tamper != "":
		r.haltLocked(ctx, ReasonTampered+": "+tamper)
		return errHalted
	case err != nil:
		if r.noteWrite(ctx, stepOf(ctx), err) {
			return errHalted
		}
		return err
	}
	r.noteWrite(ctx, stepOf(ctx), nil)
	r.mem = next
	return nil
}

// stepKey is the context key under which Reconcile stores its work-queue
// key, so a failing write can be attributed to its step.
type stepKey struct{}

func stepOf(ctx context.Context) string {
	if k, ok := ctx.Value(stepKey{}).(string); ok && k != "" {
		return k
	}
	return "setup"
}

// writeFail is one step's run of consecutive failed writes.
type writeFail struct {
	n     int
	first time.Time
}

// noteWrite tracks consecutive failed writes per step (a work-queue key, or
// "journal" for the events.jsonl/state.json flush). A busy DB is
// state_locked's business and a conflict is a normal re-plan; any other
// error counts. Past MaxWriteFailures in a row or WriteFailTimeout since the
// first, the run halts state_write_failed instead of retrying the same
// step all night (a worker that edits the attempt row FinishAttempt needs,
// or a state dir that turned read-only). A success clears the step.
// Reports whether it halted. stateMu must be held.
func (r *Reconciler) noteWrite(ctx context.Context, step string, err error) bool {
	if err == nil {
		delete(r.writeFails, step)
		return false
	}
	if state.IsBusy(err) || errors.Is(err, core.ErrConflict) || errors.Is(err, errHalted) || r.halted() {
		return false
	}
	now := time.Now()
	wf := r.writeFails[step]
	if wf == nil {
		wf = &writeFail{first: now}
		r.writeFails[step] = wf
	}
	wf.n++
	if wf.n < r.cfg.MaxWriteFailures && now.Sub(wf.first) < r.cfg.WriteFailTimeout {
		return false
	}
	r.haltLocked(ctx, fmt.Sprintf("%s: %s: %s (%d failures in %s)", ReasonWriteFailed, step, firstLine(err.Error()),
		wf.n, now.Sub(wf.first).Round(time.Second)))
	return true
}

// check is the read-only form of apply's pre-check, for steps that write
// nothing (the run key's poll).
func (r *Reconciler) check(ctx context.Context) error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.halted() {
		return errHalted
	}
	start := time.Now()
	run, err := r.cfg.Store.GetRun(ctx, r.cfg.Run)
	var tasks []state.TaskRow
	if err == nil {
		tasks, err = r.cfg.Store.ListTasks(ctx, r.cfg.Run)
	}
	if err != nil {
		r.noteDB(ctx, start, err, false)
		return err
	}
	if d := r.mem.diff(run, tasks); d != "" {
		r.haltLocked(ctx, ReasonTampered+": "+d)
		return errHalted
	}
	return nil
}

// noteDB tracks how long the DB has been busy, from the start of the first
// operation that came back busy (it already waited busy_timeout). Only a
// successful write (a step's, or busyExpired's probe) clears the clock: in
// WAL mode reads still work while another connection holds the write lock. Past LockTimeout the run halts
// state_locked. stateMu must be held.
func (r *Reconciler) noteDB(ctx context.Context, start time.Time, err error, write bool) {
	now := time.Now()
	switch {
	case state.IsBusy(err):
		if r.busySince.IsZero() {
			r.busySince = start
		}
		if now.Sub(r.busySince) > r.cfg.LockTimeout {
			r.haltLocked(ctx, fmt.Sprintf("%s: gosplan.db stayed locked for %s (another process holds the SQLite lock)",
				ReasonLocked, now.Sub(r.busySince).Round(time.Second)))
		}
	case err == nil && write:
		r.busySince = time.Time{}
	}
}

// busyExpired is noteDB's timer checked from the run key's poll. While the
// clock runs it probes the write lock for real (an empty BEGIN IMMEDIATE;
// COMMIT, which waits at most the store's busy_timeout, 5s): success means
// the lock is free and clears the clock, so one busy read followed by a
// quiet spell is not a lock; only a probe that is busy again advances it.
func (r *Reconciler) busyExpired(ctx context.Context) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.busySince.IsZero() || r.halted() {
		return
	}
	start := time.Now()
	err := r.cfg.Store.Immediate(ctx, func(*sql.Tx) error { return nil })
	if err == nil || state.IsBusy(err) {
		r.noteDB(ctx, start, err, true)
	}
}

func (r *Reconciler) halted() bool { return r.haltReason != "" }

// halt stops the run with reason (see Reason*). Safe to call more than once;
// the first reason wins.
func (r *Reconciler) halt(ctx context.Context, reason string) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.haltLocked(ctx, reason)
}

// haltLocked (stateMu held) cancels every worker and verify this process
// started, records what it can (attempts killed, run halted) without the
// tamper pre-check, and ends Run. Each write is best effort and separate, so
// a tampered or locked row cannot stop the halt itself.
func (r *Reconciler) haltLocked(ctx context.Context, reason string) {
	if r.haltReason != "" {
		return
	}
	r.haltReason = reason
	ctx = context.WithoutCancel(ctx)
	if r.cancelChildren != nil {
		r.cancelChildren()
	}
	r.mu.Lock()
	lives := make(map[string]*liveTask, len(r.live))
	for id, lt := range r.live {
		lives[id] = lt
	}
	r.mu.Unlock()
	for _, lt := range lives {
		_ = lt.h.Kill()
	}
	now := r.cfg.Now()
	if !strings.HasPrefix(reason, ReasonLocked) {
		_ = r.cfg.Store.Immediate(ctx, func(tx *sql.Tx) error {
			return state.FinishRun(tx, now, r.cfg.Run, core.RunHalted, reason)
		})
		for _, lt := range lives {
			ref := lt.ref
			_ = r.cfg.Store.Immediate(ctx, func(tx *sql.Tx) error {
				return state.FinishAttempt(tx, now, ref, state.AttemptEnd{Outcome: "killed"})
			})
		}
		_ = r.flush(ctx)
	}
	r.mem.run.Status = core.RunHalted
	r.signalDone()
}
