// Contract file: changes only by human-reviewed commits.

// Package core holds the types shared by the orchestrator packages. Other
// packages import it and never edit it.
package core

import (
	"context"
	"errors"
	"time"
)

// Task statuses. Exactly the states of the M1 event schema.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusVerifying = "verifying"
	StatusPassed    = "passed"
	StatusFailed    = "failed"
	StatusBlocked   = "blocked"
	StatusAbandoned = "abandoned"
)

// Run statuses.
const (
	RunRunning  = "running"
	RunComplete = "complete"
	RunHalted   = "halted"
	RunAborted  = "aborted"
)

// Retry backoff defaults.
const (
	BaseRetryDelay = 10 * time.Second
	MaxRetryDelay  = 5 * time.Minute
	StallTimeout   = 5 * time.Minute
)

// RetryDelay is the backoff before attempt n+1 after attempt n (n >= 1)
// failed: min(10s * 2^(n-1), 5m). n < 1 is treated as 1.
func RetryDelay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := BaseRetryDelay
	for i := 1; i < n; i++ {
		d *= 2
		if d >= MaxRetryDelay {
			return MaxRetryDelay
		}
	}
	return d
}

// ErrConflict means an apply-step re-check found the state changed since
// the plan was made. The caller requeues; it is not a failure.
var ErrConflict = errors.New("core: state changed since plan")

// ErrMergeConflict means a dependency head could not be merged.
var ErrMergeConflict = errors.New("core: merge conflict")

// AttemptRef names one attempt.
type AttemptRef struct {
	Run       string
	Task      string
	Attempt   int   // 1-based
	AttemptID int64 // attempts.id
}

// LaunchRequest is one worker process to start.
type LaunchRequest struct {
	Ref        AttemptRef
	Argv       []string // argv[0] is the binary
	Dir        string   // working directory (the task worktree)
	Env        []string // complete environment
	StreamPath string   // receives every stdout line
	StderrPath string
}

// Exit describes how a worker ended.
type Exit struct {
	Code    int    // process exit code; -1 if killed by a signal
	Signal  string // signal name if killed, else ""
	Stalled bool   // killed by the stall detector
	Killed  bool   // killed by Kill() for another reason (abort, shutdown)
	Err     error  // wait error other than a non-zero exit
}

// Handle is a running worker.
type Handle interface {
	PID() int
	PGID() int
	// Lines delivers each stdout line (without the newline). It is closed
	// at EOF. Lines are also written to StreamPath.
	Lines() <-chan []byte
	// Wait blocks until the process has exited and stdout reached EOF,
	// then returns the same Exit on every call.
	Wait() Exit
	// Kill SIGKILLs the whole process group. Safe to call more than once.
	Kill() error
}

// Launcher starts workers.
type Launcher interface {
	Launch(ctx context.Context, req LaunchRequest) (Handle, error)
}

// VerifyRequest is one `gosplan verify` invocation.
type VerifyRequest struct {
	Project           string
	Run               string
	Task              string
	Attempt           int
	Base              string   // full SHA, tasks.base_sha
	Head              string   // full SHA, resolved branch tip
	Scope             []string // nil = unrestricted
	AllowNewProtected bool
	EventLog          string // passed as --log
	LogsDir           string // passed as --logs
}

// VerifyOutcome is what the orchestrator learns from `gosplan verify`.
type VerifyOutcome struct {
	Exit        int      // 0 pass, 1 fail, 2 could not verify (any other failure maps to 2)
	Result      string   // "pass" | "fail" | "" when Exit == 2
	FailedGates []string // from the verdict JSON
	TreeSHA     string
	VerdictJSON []byte // raw stdout of `gosplan verify --json`; nil when Exit == 2
	Detail      string // short human reason (failed gate details or the exit-2 cause)
}

// Verifier runs the verifier. It must never report Exit 0 unless the
// verifier process exited 0 and its JSON says "pass".
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (VerifyOutcome, error)
}

// PrepareRequest asks for a task worktree.
type PrepareRequest struct {
	Project  string // project repo (absolute)
	Run      string
	Task     string
	BaseSHA  string   // run base, full SHA
	DepHeads []string // full SHAs of passed dependency heads, in dependency order
	Root     string   // parent dir for worktrees, outside the project
}

// Prepared is a ready worktree.
type Prepared struct {
	Dir     string
	Branch  string // gosplan/<run>/<task>
	BaseSHA string // HEAD after merging DepHeads: the --base for gosplan verify
}

// Workspace owns git worktrees and branches. It never touches the project's
// checked-out branch, main or master.
type Workspace interface {
	Prepare(ctx context.Context, req PrepareRequest) (Prepared, error) // ErrMergeConflict on conflict
	Head(ctx context.Context, project, branch string) (string, error)  // full SHA of refs/heads/<branch>
	Remove(ctx context.Context, project, dir string) error
	// Integrate creates or resets branch at baseSHA and merges heads in order.
	// Returns the new tip. ErrMergeConflict on conflict (branch left at the last good merge).
	Integrate(ctx context.Context, project, branch, baseSHA string, heads []string) (string, error)
}

// LineHook sees every stdout line of every attempt (window parsing).
type LineHook func(ref AttemptRef, line []byte)

// VerdictHook runs after a verdict is recorded (signing, mutation). It
// cannot change the task's status.
type VerdictHook func(ctx context.Context, ref AttemptRef, req VerifyRequest, out VerifyOutcome)
