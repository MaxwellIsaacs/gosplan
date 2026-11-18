package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// settleRec is one Settle call, with what the call's own tx showed.
type settleRec struct {
	ref        core.AttemptRef
	outcome    string
	taskStatus string // tasks.status read through the Settle tx
	ended      bool   // attempts.ended_at IS NOT NULL through the Settle tx
}

type verdictRec struct {
	result      string
	consecutive int
	halting     bool
}

// admitSeen is one Admit call, with the task row as the Admit tx showed it.
type admitSeen struct {
	req       AdmitRequest
	status    string
	notBefore *time.Time
	attempts  int // attempt rows of the task
}

// fakeBudget is a scripted reconcile.Budget. Every hook is recorded; the
// script funcs are optional (nil = admit / no block / no halt / nothing).
type fakeBudget struct {
	admit      func(call int, req AdmitRequest) (AdmitResult, error)
	reserveErr func(call int) error
	retry      func(t state.TaskRow) string
	tick       func(now time.Time) budget.Halt
	rearm      func(now time.Time) map[string]time.Time
	selfCheck  string

	mu       sync.Mutex
	admits   []admitSeen
	reserves []core.AttemptRef
	settles  []settleRec
	retries  []state.TaskRow
	verdicts []verdictRec
	ticks    int
}

func (f *fakeBudget) Admit(tx *sql.Tx, req AdmitRequest) (AdmitResult, error) {
	seen := admitSeen{req: req}
	var nb sql.NullString
	if err := tx.QueryRow(`SELECT status, not_before FROM tasks WHERE run_id = ? AND id = ?`, req.Run, req.Task).Scan(&seen.status, &nb); err != nil {
		return AdmitResult{}, err
	}
	if nb.Valid {
		t, _ := time.Parse(time.RFC3339Nano, nb.String)
		seen.notBefore = &t
	}
	if err := tx.QueryRow(`SELECT count(*) FROM attempts WHERE run_id = ? AND task_id = ?`, req.Run, req.Task).Scan(&seen.attempts); err != nil {
		return AdmitResult{}, err
	}
	f.mu.Lock()
	f.admits = append(f.admits, seen)
	call := len(f.admits)
	f.mu.Unlock()
	if f.admit != nil {
		return f.admit(call, req)
	}
	return AdmitResult{Admit: true, Model: req.Model, Rule: budget.RuleAdmit}, nil
}

// Reserve writes a real reservation row in the dispatch tx.
func (f *fakeBudget) Reserve(tx *sql.Tx, ref core.AttemptRef, res AdmitResult) error {
	f.mu.Lock()
	f.reserves = append(f.reserves, ref)
	call := len(f.reserves)
	f.mu.Unlock()
	if err := state.InsertReservation(tx, ref, budget.Decision{Model: res.Model}, true, 0, time.Now()); err != nil {
		return err
	}
	if f.reserveErr != nil {
		return f.reserveErr(call)
	}
	return nil
}

func (f *fakeBudget) Settle(tx *sql.Tx, ref core.AttemptRef, end state.AttemptEnd) error {
	rec := settleRec{ref: ref, outcome: end.Outcome}
	if err := tx.QueryRow(`SELECT status FROM tasks WHERE run_id = ? AND id = ?`, ref.Run, ref.Task).Scan(&rec.taskStatus); err != nil {
		return err
	}
	var ended sql.NullString
	if err := tx.QueryRow(`SELECT ended_at FROM attempts WHERE id = ?`, ref.AttemptID).Scan(&ended); err != nil {
		return err
	}
	rec.ended = ended.Valid
	f.mu.Lock()
	f.settles = append(f.settles, rec)
	f.mu.Unlock()
	return nil
}

func (f *fakeBudget) Retry(tx *sql.Tx, t state.TaskRow) (string, error) {
	f.mu.Lock()
	f.retries = append(f.retries, t)
	f.mu.Unlock()
	if f.retry != nil {
		return f.retry(t), nil
	}
	return "", nil
}

func (f *fakeBudget) Verdict(tx *sql.Tx, ref core.AttemptRef, result string, consecutive int, halting bool) error {
	f.mu.Lock()
	f.verdicts = append(f.verdicts, verdictRec{result, consecutive, halting})
	f.mu.Unlock()
	return nil
}

func (f *fakeBudget) Tick(ctx context.Context, now time.Time) (budget.Halt, error) {
	f.mu.Lock()
	f.ticks++
	f.mu.Unlock()
	if f.tick != nil {
		return f.tick(now), nil
	}
	return budget.Halt{}, nil
}

func (f *fakeBudget) Rearm(tx *sql.Tx, now time.Time) (map[string]time.Time, error) {
	if f.rearm != nil {
		return f.rearm(now), nil
	}
	return nil, nil
}

func (f *fakeBudget) Quiet(line []byte) time.Duration { return 0 }
func (f *fakeBudget) SelfCheck(ctx context.Context) string {
	return f.selfCheck
}

func (f *fakeBudget) settlesCopy() []settleRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]settleRec(nil), f.settles...)
}

func (f *fakeBudget) admitsCopy() []admitSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]admitSeen(nil), f.admits...)
}

func (f *fakeBudget) reservesCopy() []core.AttemptRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.AttemptRef(nil), f.reserves...)
}

// taskStateTos is the "to" of every task_state event of task in the log.
func taskStateTos(t *testing.T, h *harness, task string) []string {
	t.Helper()
	evs, err := events.ReadFile(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Type != events.TaskState || ev.Task != task {
			continue
		}
		var d events.TaskStateData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d.To)
	}
	return out
}

func countRows(t *testing.T, h *harness, q string, args ...any) int {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Refusal: the task stays pending with no attempt row, not_before is set
// through txn (no state_tampered on the next step), it is requeued at
// not_before, and no task_state event is written for it.
func TestBudgetHookRefusalParksThroughTxn(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	var refusedNB time.Time
	fb := &fakeBudget{admit: func(call int, req AdmitRequest) (AdmitResult, error) {
		if call == 1 {
			refusedNB = req.Now.Add(300 * time.Millisecond)
			return AdmitResult{Admit: false, NotBefore: refusedNB, Rule: budget.RuleHeadroom, Reason: "no headroom"}, nil
		}
		return AdmitResult{Admit: true, Model: req.Model}, nil
	}}
	h.cfg.Budget = fb
	o := h.runOutcome()
	if o.Status != core.RunComplete {
		t.Fatalf("run = %s (%s), want complete", o.Status, o.Reason)
	}
	as := fb.admitsCopy()
	if len(as) != 2 {
		t.Fatalf("Admit calls = %d, want 2 (refused, then admitted)", len(as))
	}
	if as[0].req.Attempt != 1 || as[0].req.Model != "sonnet" || as[0].req.Run != testRun || as[0].req.Task != "1.a" {
		t.Fatalf("first AdmitRequest = %+v", as[0].req)
	}
	second := as[1]
	if second.status != core.StatusPending || second.attempts != 0 {
		t.Fatalf("after the refusal: status %s, %d attempt rows; want pending and none", second.status, second.attempts)
	}
	if second.notBefore == nil || !second.notBefore.Equal(refusedNB) {
		t.Fatalf("not_before after the refusal = %v, want %v", second.notBefore, refusedNB)
	}
	if second.req.Now.Before(refusedNB) {
		t.Fatalf("re-admitted at %v, before not_before %v", second.req.Now, refusedNB)
	}
	if got, want := taskStateTos(t, h, "1.a"), []string{"pending", "running", "verifying", "passed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("task_state events = %v, want %v (a refusal writes none)", got, want)
	}
	if n := len(h.attempts("1.a")); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

// Admit: the model and belts of the AdmitResult are what is launched, and
// Reserve runs once, in the dispatch tx, for the started attempt.
func TestBudgetHookAdmitModelAndReserve(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	fb := &fakeBudget{admit: func(call int, req AdmitRequest) (AdmitResult, error) {
		return AdmitResult{Admit: true, Model: "routed-haiku", ExtraArgv: []string{"--max-budget-usd", "1.00"}}, nil
	}}
	h.cfg.Budget = fb
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("run = %s (%s)", o.Status, o.Reason)
	}
	ls, _ := h.l.snapshot()
	if len(ls) != 1 {
		t.Fatalf("launches = %d", len(ls))
	}
	argv := strings.Join(ls[0].req.Argv, " ")
	if !strings.Contains(argv, "--model routed-haiku") || !strings.HasSuffix(argv, "--max-budget-usd 1.00") {
		t.Fatalf("argv = %q, want the routed model and the belt appended", argv)
	}
	as := h.attempts("1.a")
	if len(as) != 1 || as[0].Model != "routed-haiku" {
		t.Fatalf("attempt rows = %+v, want model routed-haiku", as)
	}
	rs := fb.reservesCopy()
	if len(rs) != 1 || rs[0].AttemptID != as[0].ID {
		t.Fatalf("Reserve calls = %+v, want one for attempt %d", rs, as[0].ID)
	}
	if n := countRows(t, h, `SELECT count(*) FROM budget_reservations WHERE attempt_id = ? AND model = 'routed-haiku'`, as[0].ID); n != 1 {
		t.Fatalf("reservation rows = %d", n)
	}
}

// Reserve runs inside the dispatch tx: when the tx rolls back (Reserve
// fails the first time), its row and the attempt row vanish together.
func TestBudgetHookReserveInDispatchTx(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	fb := &fakeBudget{reserveErr: func(call int) error {
		if call == 1 {
			return core.ErrConflict
		}
		return nil
	}}
	h.cfg.Budget = fb
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("run = %s (%s)", o.Status, o.Reason)
	}
	rs := fb.reservesCopy()
	if len(rs) != 2 {
		t.Fatalf("Reserve calls = %d, want 2 (rolled back, then committed)", len(rs))
	}
	as := h.attempts("1.a")
	if len(as) != 1 || as[0].N != 1 {
		t.Fatalf("attempts = %+v, want the rolled-back attempt gone (one attempt, n=1)", as)
	}
	if n := countRows(t, h, `SELECT count(*) FROM budget_reservations`); n != 1 {
		t.Fatalf("reservation rows = %d, want 1: the rolled-back Reserve's row must vanish with its tx", n)
	}
	if n := countRows(t, h, `SELECT count(*) FROM budget_reservations WHERE attempt_id = ?`, as[0].ID); n != 1 {
		t.Fatal("the surviving reservation is not the committed attempt's")
	}
}

// wantSettledInTx: Settle ran exactly once per finished attempt, in the
// tx that finished it (attempt ended, task not yet moved on).
func wantSettledInTx(t *testing.T, h *harness, fb *fakeBudget, task string, wantOutcomes []string, wantStatus string) {
	t.Helper()
	ss := fb.settlesCopy()
	var got []string
	seen := map[int64]int{}
	for _, s := range ss {
		if s.ref.Task != task {
			continue
		}
		got = append(got, s.outcome)
		seen[s.ref.AttemptID]++
		if !s.ended {
			t.Errorf("Settle for attempt %d ran before FinishAttempt", s.ref.AttemptID)
		}
		if s.taskStatus != wantStatus {
			t.Errorf("Settle for attempt %d (%s) saw task %s; want %s (same tx as FinishAttempt)", s.ref.AttemptID, s.outcome, s.taskStatus, wantStatus)
		}
	}
	if !reflect.DeepEqual(got, wantOutcomes) {
		t.Fatalf("Settle outcomes = %v, want %v", got, wantOutcomes)
	}
	for _, a := range h.attempts(task) {
		if a.EndedAt.IsZero() {
			continue
		}
		if seen[a.ID] != 1 {
			t.Errorf("attempt %d (%s) settled %d times, want 1", a.ID, a.Outcome, seen[a.ID])
		}
	}
}

func TestBudgetHookSettleOnExit(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.script = func(task string, n int) step {
		if n == 1 {
			return step{lines: []string{resultLine("broken")}, code: 3}
		}
		return okStep
	}
	fb := &fakeBudget{}
	h.cfg.Budget = fb
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("run = %s (%s)", o.Status, o.Reason)
	}
	wantSettledInTx(t, h, fb, "1.a", []string{"exited", "exited"}, core.StatusRunning)
}

func TestBudgetHookSettleOnStartFailure(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.w.prepareErr = map[string]error{"1.a": errors.New("disk full")}
	fb := &fakeBudget{}
	h.cfg.Budget = fb
	h.runOutcome()
	wantSettledInTx(t, h, fb, "1.a", []string{"start_failed", "start_failed", "start_failed"}, core.StatusRunning)
}

func TestBudgetHookSettleOnLost(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	seedLost(t, h, 0, 0, "")
	fb := &fakeBudget{}
	h.cfg.Budget = fb
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("run = %s (%s)", o.Status, o.Reason)
	}
	wantSettledInTx(t, h, fb, "1.a", []string{"lost", "exited"}, core.StatusRunning)
}

func TestBudgetHookSettleOnAbort(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.script = func(string, int) step { return step{hang: true, heartbeat: 50 * time.Millisecond} }
	fb := &fakeBudget{}
	h.cfg.Budget = fb
	r, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if l, _ := h.l.snapshot(); len(l) > 0 {
				time.Sleep(100 * time.Millisecond)
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	status, err := r.Run(ctx)
	if err != nil || status != core.RunAborted {
		t.Fatalf("Run = %q, %v; want aborted", status, err)
	}
	wantSettledInTx(t, h, fb, "1.a", []string{"killed"}, core.StatusRunning)
}

// Retry's block reason sends the task to blocked, attempts left unused.
func TestBudgetHookRetryBlocks(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.script = func(string, int) step { return step{lines: []string{resultLine("nothing")}, code: 3} }
	const why = "SAME_FAILURE: 2 attempts with no-commit"
	fb := &fakeBudget{retry: func(t state.TaskRow) string {
		if t.AttemptsUsed >= 2 {
			return why
		}
		return ""
	}}
	h.cfg.Budget = fb
	h.runOutcome()
	tk := h.task("1.a")
	if tk.Status != core.StatusBlocked || tk.BlockedReason != why {
		t.Fatalf("1.a = %s %q, want blocked %q", tk.Status, tk.BlockedReason, why)
	}
	if tk.AttemptsUsed != 2 || len(h.attempts("1.a")) != 2 || tk.AttemptsUsed >= h.cfg.MaxAttempts {
		t.Fatalf("attempts used %d (rows %d), want 2 of %d", tk.AttemptsUsed, len(h.attempts("1.a")), h.cfg.MaxAttempts)
	}
}

// Rearm: a task parked for an hour is re-armed through txn and runs now.
func TestBudgetHookRearmThroughTxn(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	var mu sync.Mutex
	parked, rearmed := false, false
	var rearmNB time.Time
	fb := &fakeBudget{
		admit: func(call int, req AdmitRequest) (AdmitResult, error) {
			if call == 1 {
				mu.Lock()
				parked = true
				mu.Unlock()
				return AdmitResult{NotBefore: req.Now.Add(time.Hour), Rule: budget.RuleHeadroom}, nil
			}
			return AdmitResult{Admit: true, Model: req.Model}, nil
		},
		rearm: func(now time.Time) map[string]time.Time {
			mu.Lock()
			defer mu.Unlock()
			if !parked || rearmed {
				return nil
			}
			rearmed, rearmNB = true, now.Add(50*time.Millisecond)
			return map[string]time.Time{"1.a": rearmNB, "no-such-task": now}
		},
	}
	h.cfg.Budget = fb
	start := time.Now()
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("run = %s (%s), want complete (no state_tampered)", o.Status, o.Reason)
	}
	as := fb.admitsCopy()
	if len(as) != 2 || as[1].notBefore == nil || !as[1].notBefore.Equal(rearmNB) {
		t.Fatalf("admits = %+v, want the second to see the re-armed not_before %v", as, rearmNB)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("re-armed task took %s", time.Since(start))
	}
}

// heartbeatStep keeps a worker busy for about d, then succeeds.
func heartbeatStep(d time.Duration) step {
	n := int(d / (100 * time.Millisecond))
	lines := make([]string, 0, n+1)
	for i := 0; i < n; i++ {
		lines = append(lines, `{"type":"system","subtype":"heartbeat"}`)
	}
	return step{lines: append(lines, resultLine("done")), lineGap: 100 * time.Millisecond}
}

// Drain: the running attempt finishes and verifies, nothing new starts,
// the run halts with the parked ids, and a free slot held for longer than
// ProgressTimeout is not no_progress.
func TestBudgetHookDrainHalt(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("1.b", 1), task("2.a", 2, "1.a"))
	h.cfg.ProgressTimeout = 200 * time.Millisecond
	h.l.script = func(task string, n int) step { return heartbeatStep(4500 * time.Millisecond) }
	fb := &fakeBudget{}
	fb.admit = func(call int, req AdmitRequest) (AdmitResult, error) {
		if req.Task == "1.b" {
			return AdmitResult{NotBefore: req.Now.Add(100 * time.Millisecond), Rule: budget.RuleFairness}, nil
		}
		return AdmitResult{Admit: true, Model: req.Model}, nil
	}
	fb.tick = func(now time.Time) budget.Halt {
		if len(fb.reservesCopy()) > 0 {
			return budget.Halt{Kind: "drain", Reason: "cap:usd_estimate"}
		}
		return budget.Halt{}
	}
	h.cfg.Budget = fb
	o := h.runOutcome()
	if o.Status != core.RunHalted || o.Reason != "cap:usd_estimate: parked 1.b, 2.a" {
		t.Fatalf("run = %s %q, want halted \"cap:usd_estimate: parked 1.b, 2.a\"", o.Status, o.Reason)
	}
	if tk := h.task("1.a"); tk.Status != core.StatusPassed {
		t.Fatalf("1.a = %s, want passed (a drain lets the running attempt finish and verify)", tk.Status)
	}
	for _, id := range []string{"1.b", "2.a"} {
		if tk := h.task(id); tk.Status != core.StatusPending {
			t.Fatalf("%s = %s, want pending (parked)", id, tk.Status)
		}
	}
	if ls, _ := h.l.snapshot(); len(ls) != 1 || ls[0].task != "1.a" {
		t.Fatalf("launches = %+v, want only 1.a", ls)
	}
	var data string
	if err := h.store.DB().QueryRow(`SELECT data_json FROM budget_events WHERE type = 'budget_halt'`).Scan(&data); err != nil {
		t.Fatalf("budget_halt event: %v", err)
	}
	var hd budget.Halt
	if err := json.Unmarshal([]byte(data), &hd); err != nil || hd.Kind != "drain" || !reflect.DeepEqual(hd.Parked, []string{"1.b", "2.a"}) {
		t.Fatalf("budget_halt = %s (%v)", data, err)
	}
}

// Stop: the live worker is killed promptly; its attempt ends killed and is
// refunded, and the task goes back to pending, even on its last attempt.
func TestBudgetHookStopHalt(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MaxAttempts = 1
	h.l.script = func(string, int) step { return step{hang: true, heartbeat: 50 * time.Millisecond} }
	var stopAt time.Time
	var mu sync.Mutex
	fb := &fakeBudget{}
	fb.tick = func(now time.Time) budget.Halt {
		if len(fb.reservesCopy()) == 0 {
			return budget.Halt{}
		}
		mu.Lock()
		if stopAt.IsZero() {
			stopAt = time.Now()
		}
		mu.Unlock()
		return budget.Halt{Kind: "stop", Reason: ReasonDeadline}
	}
	h.cfg.Budget = fb
	o := h.runOutcome()
	if o.Status != core.RunHalted || o.Reason != "deadline: parked 1.a" {
		t.Fatalf("run = %s %q, want halted \"deadline: parked 1.a\"", o.Status, o.Reason)
	}
	if h.l.killCount("1.a") != 1 {
		t.Fatalf("kills = %d, want 1", h.l.killCount("1.a"))
	}
	tk := h.task("1.a")
	if tk.Status != core.StatusPending || tk.AttemptsUsed != 0 || tk.NotBefore != nil || tk.LastFailure != "" {
		t.Fatalf("1.a = %s used=%d not_before=%v last_failure=%q; want pending, refunded, untouched",
			tk.Status, tk.AttemptsUsed, tk.NotBefore, tk.LastFailure)
	}
	if got := outcomes(h.attempts("1.a")); !reflect.DeepEqual(got, []string{"killed"}) {
		t.Fatalf("outcomes = %v, want [killed]", got)
	}
	if fb.retries != nil {
		t.Fatalf("Retry called %d times: a budget stop never takes retryPath", len(fb.retries))
	}
	wantSettledInTx(t, h, fb, "1.a", []string{"killed"}, core.StatusRunning)
	if tos := taskStateTos(t, h, "1.a"); !slices.Equal(tos, []string{"pending", "running", "pending"}) {
		t.Fatalf("task_state = %v", tos)
	}
}

func errOutcome(task string, call int) core.VerifyOutcome {
	return core.VerifyOutcome{Exit: 2, Result: "error", Detail: "verifier crashed on " + task}
}

// verifier_error is always on: two consecutive could-not-verify verdicts
// across the run halt it with a nil Budget.
func TestBudgetHookVerifierErrorHaltNoGate(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("1.b", 1))
	h.cfg.MaxVerifyErrors = 2
	h.v.outcome = errOutcome
	o := h.runOutcome()
	if o.Status != core.RunHalted || !strings.HasPrefix(o.Reason, "verifier_error: 2 consecutive could-not-verify results; last: verifier crashed on 1.") {
		t.Fatalf("run = %s %q, want halted verifier_error", o.Status, o.Reason)
	}
}

// With a gate, Verdict gets the count and whether the reconciler halts.
func TestBudgetHookVerifierErrorVerdictCount(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("1.b", 1))
	h.cfg.MaxVerifyErrors = 2
	h.v.outcome = errOutcome
	fb := &fakeBudget{}
	h.cfg.Budget = fb
	o := h.runOutcome()
	if o.Status != core.RunHalted || !strings.HasPrefix(o.Reason, ReasonVerifierError+": 2 consecutive") {
		t.Fatalf("run = %s %q", o.Status, o.Reason)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	want := []verdictRec{{"error", 1, false}, {"error", 2, true}}
	if !reflect.DeepEqual(fb.verdicts, want) {
		t.Fatalf("Verdict calls = %+v, want %+v", fb.verdicts, want)
	}
}

// A pass resets the run-wide count: error, pass, error, pass completes.
func TestBudgetHookVerifierErrorResetByPass(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("2.a", 2, "1.a"))
	h.cfg.MaxVerifyErrors = 2
	h.v.outcome = func(task string, call int) core.VerifyOutcome {
		if call == 1 {
			return errOutcome(task, call)
		}
		return passOutcome
	}
	fb := &fakeBudget{}
	h.cfg.Budget = fb
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("run = %s %q, want complete (a pass resets the count)", o.Status, o.Reason)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	want := []verdictRec{{"error", 1, false}, {"pass", 0, false}, {"error", 1, false}, {"pass", 0, false}}
	if !reflect.DeepEqual(fb.verdicts, want) {
		t.Fatalf("Verdict calls = %+v, want %+v", fb.verdicts, want)
	}
}

func TestBudgetHookTamperedAdmitHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.Budget = &fakeBudget{admit: func(int, AdmitRequest) (AdmitResult, error) {
		return AdmitResult{}, fmt.Errorf("%w: budget_runs mode changed", ErrBudgetTampered)
	}}
	o := h.runOutcome()
	if o.Status != core.RunHalted || !strings.HasPrefix(o.Reason, ReasonTampered+": budget: ") || !strings.Contains(o.Reason, "budget_runs mode changed") {
		t.Fatalf("run = %s %q, want halted state_tampered: budget: …", o.Status, o.Reason)
	}
	if n := len(h.attempts("1.a")); n != 0 {
		t.Fatalf("attempts = %d, want none", n)
	}
}

func TestBudgetHookSelfCheckHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.Budget = &fakeBudget{selfCheck: ReasonInconsistent + ": budget.jsonl decision eid 3 differs"}
	o := h.runOutcome()
	if o.Status != core.RunHalted || o.Reason != ReasonInconsistent+": budget.jsonl decision eid 3 differs" {
		t.Fatalf("run = %s %q", o.Status, o.Reason)
	}
}

// nil Budget: master's diamond completes and nothing touches the budget tables.
func TestBudgetHookNilBudgetIsMaster(t *testing.T) {
	h := newHarness(t, diamond()...)
	if o := h.runOutcome(); o.Status != core.RunComplete || o.Reason != "" {
		t.Fatalf("run = %s %q", o.Status, o.Reason)
	}
	for _, tbl := range []string{"budget_events", "budget_reservations", "budget_parks", "budget_runs", "budget_breakers"} {
		if n := countRows(t, h, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Fatalf("%s has %d rows with a nil Budget", tbl, n)
		}
	}
	for _, id := range []string{"1.a", "2.a", "2.b", "3.a"} {
		if n := len(h.attempts(id)); n != 1 {
			t.Fatalf("%s attempts = %d", id, n)
		}
	}
}

// gapHandle prints each line after its own gap, then exits 0.
type gapHandle struct {
	lines chan []byte
	kill  chan struct{}
	once  sync.Once
	done  chan struct{}
	exit  core.Exit
}

func (g *gapHandle) PID() int             { return 0 }
func (g *gapHandle) PGID() int            { return 0 }
func (g *gapHandle) Lines() <-chan []byte { return g.lines }
func (g *gapHandle) Wait() core.Exit      { <-g.done; return g.exit }
func (g *gapHandle) Kill() error          { g.once.Do(func() { close(g.kill) }); return nil }

type gapLine struct {
	gap  time.Duration
	line string
}

type gapLauncher struct {
	script []gapLine
	mu     sync.Mutex
	n      int
}

func (l *gapLauncher) Launch(ctx context.Context, req core.LaunchRequest) (core.Handle, error) {
	f, err := os.Create(req.StreamPath)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.n++
	l.mu.Unlock()
	g := &gapHandle{lines: make(chan []byte, len(l.script)), kill: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(g.done)
		g.exit = core.Exit{Code: 0}
		for _, gl := range l.script {
			select {
			case <-time.After(gl.gap):
			case <-g.kill:
				g.exit = core.Exit{Code: -1, Killed: true}
				f.Close()
				close(g.lines)
				return
			}
			fmt.Fprintln(f, gl.line)
			g.lines <- []byte(gl.line)
		}
		f.Close()
		close(g.lines)
	}()
	return g, nil
}

// The reconciler's own stall backstop honours the api_retry allowance
// (quietUntil), with no gate: 700 ms of silence after an api_retry line
// with retry_delay_ms 1000 is not a stall at StallTimeout 300 ms.
func TestBudgetHookAPIRetryQuietBackstop(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	gl := &gapLauncher{script: []gapLine{
		{0, `{"type":"system","subtype":"init"}`},
		{100 * time.Millisecond, `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":1000}`},
		{700 * time.Millisecond, resultLine("done")},
	}}
	h.cfg.Launcher = gl
	if o := h.runOutcome(); o.Status != core.RunComplete {
		t.Fatalf("run = %s %q", o.Status, o.Reason)
	}
	if got := outcomes(h.attempts("1.a")); !reflect.DeepEqual(got, []string{"exited"}) {
		t.Fatalf("outcomes = %v, want [exited]: the api_retry wait was killed as a stall", got)
	}
}
