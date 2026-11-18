package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// Budget is the reconciler's view of the budget gate.
// A nil Config.Budget keeps master's behaviour, except the two always-on
// rules (api_retry quiet allowance, verifier_error halt), which
// do not depend on it. The gate never writes the tasks table: every task
// change goes through the reconciler's txn.
type Budget interface {
	// Admit runs inside dispatch's apply, after the memory re-checks and
	// state.NextAttempt, before the pending->running transition. It settles
	// orphan reservations, decides, writes its events and, on an enforced
	// refusal, upserts budget_parks. It never writes tasks; the reconciler
	// sets not_before with x.update.
	Admit(tx *sql.Tx, req AdmitRequest) (AdmitResult, error)
	// Reserve runs in the same tx right after StartAttempt when Admit admitted.
	Reserve(tx *sql.Tx, ref core.AttemptRef, res AdmitResult) error
	// Settle runs in the tx of every hooked state.FinishAttempt call
	// (handleExit, startFailed, recoverLost, abort). haltLocked is not hooked.
	Settle(tx *sql.Tx, ref core.AttemptRef, end state.AttemptEnd) error
	// Retry runs in retryPath's tx before the task goes back to pending.
	// A non-empty blockReason sends it to blocked instead (SAME_FAILURE).
	Retry(tx *sql.Tx, t state.TaskRow) (blockReason string, err error)
	// Verdict runs in the verdict tx after state.SetVerdict. consecutive is
	// the reconciler's run-wide count of verdict errors; halting is true when
	// the reconciler will halt verifier_error after this tx. It only logs.
	Verdict(tx *sql.Tx, ref core.AttemptRef, result string, consecutive int, halting bool) error
	// Tick runs on each run-key reconcile, outside any tx.
	Tick(ctx context.Context, now time.Time) (budget.Halt, error)
	// Rearm runs in one apply on the run key, after Tick: this run's parked
	// tasks whose budget_parks row is gone, with their new not_before.
	Rearm(tx *sql.Tx, now time.Time) (map[string]time.Time, error)
	// Quiet is the extra stall allowance for one stdout line (api_retry),
	// already capped at quiet_max.
	Quiet(line []byte) time.Duration
	// SelfCheck runs once at the end of Run, after master's selfCheck: it
	// compares budget.jsonl with the decisions this process wrote. Returns a
	// halt reason or "".
	SelfCheck(ctx context.Context) string
}

// ErrBudgetTampered is wrapped by a hook that finds rows this process wrote
// changed; the reconciler halts state_tampered.
var ErrBudgetTampered = errors.New("budget: own rows changed")

type AdmitRequest struct {
	Run, Task string
	Attempt   int    // n from state.NextAttempt: the attempt number it would get
	Model     string // cfg.Model(m.AttemptsUsed+1) (consumed count): used unless routing picks another
	Now       time.Time
}

type AdmitResult struct {
	Admit     bool
	Model     string    // model to launch
	NotBefore time.Time // when refused; always >= Now + 1s
	Rule      string
	Reason    string
	ExtraArgv []string // belts: --max-budget-usd (enforce mode, usd_factor set); --max-turns override
	Decision  []byte   // canonical JSON of budget.Decision, for Reserve
}
