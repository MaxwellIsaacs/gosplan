//go:build nrcore

// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit it to make a test pass.
//
// Command coreprobe pins the exported contract of internal/core
// at compile time, then prints its values as
// JSON for conformance/core_test.go. It is built only with -tags nrcore, so
// `go vet ./conformance/...` works before internal/core exists.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
)

// Every name, field and type of the core contract, written as typed assignments: any drift
// from the verbatim contract fails to build.
var (
	_ string = core.StatusPending
	_ string = core.StatusRunning
	_ string = core.StatusVerifying
	_ string = core.StatusPassed
	_ string = core.StatusFailed
	_ string = core.StatusBlocked
	_ string = core.StatusAbandoned
	_ string = core.RunRunning
	_ string = core.RunComplete
	_ string = core.RunHalted
	_ string = core.RunAborted

	_ time.Duration                                     = core.BaseRetryDelay
	_ time.Duration                                     = core.MaxRetryDelay
	_ time.Duration                                     = core.StallTimeout
	_ func(int) time.Duration                           = core.RetryDelay
	_ error                                             = core.ErrConflict
	_ error                                             = core.ErrMergeConflict
	_ core.LineHook                                     = func(ref core.AttemptRef, line []byte) {}
	_ core.VerdictHook                                  = func(ctx context.Context, ref core.AttemptRef, req core.VerifyRequest, out core.VerifyOutcome) {}
	_ func(core.LineHook) func(core.AttemptRef, []byte) = func(h core.LineHook) func(core.AttemptRef, []byte) { return h }

	_ = core.AttemptRef{Run: "", Task: "", Attempt: int(1), AttemptID: int64(1)}
	_ = core.LaunchRequest{Ref: core.AttemptRef{}, Argv: []string{}, Dir: "", Env: []string{}, StreamPath: "", StderrPath: ""}
	_ = core.Exit{Code: int(0), Signal: "", Stalled: false, Killed: false, Err: error(nil)}
	_ = core.VerifyRequest{Project: "", Run: "", Task: "", Attempt: int(1), Base: "", Head: "", Scope: []string(nil),
		AllowNewProtected: false, EventLog: "", LogsDir: ""}
	_ = core.VerifyOutcome{Exit: int(0), Result: "", FailedGates: []string(nil), TreeSHA: "", VerdictJSON: []byte(nil), Detail: ""}
	_ = core.PrepareRequest{Project: "", Run: "", Task: "", BaseSHA: "", DepHeads: []string(nil), Root: ""}
	_ = core.Prepared{Dir: "", Branch: "", BaseSHA: ""}
)

// The interfaces have exactly the contract's method sets (both directions).
type handle interface {
	PID() int
	PGID() int
	Lines() <-chan []byte
	Wait() core.Exit
	Kill() error
}
type launcher interface {
	Launch(ctx context.Context, req core.LaunchRequest) (core.Handle, error)
}
type verifier interface {
	Verify(ctx context.Context, req core.VerifyRequest) (core.VerifyOutcome, error)
}
type workspace interface {
	Prepare(ctx context.Context, req core.PrepareRequest) (core.Prepared, error)
	Head(ctx context.Context, project, branch string) (string, error)
	Remove(ctx context.Context, project, dir string) error
	Integrate(ctx context.Context, project, branch, baseSHA string, heads []string) (string, error)
}

var (
	_ handle         = core.Handle(nil)
	_ core.Handle    = handle(nil)
	_ launcher       = core.Launcher(nil)
	_ core.Launcher  = launcher(nil)
	_ verifier       = core.Verifier(nil)
	_ core.Verifier  = verifier(nil)
	_ workspace      = core.Workspace(nil)
	_ core.Workspace = workspace(nil)
)

func main() {
	delays := map[string]string{}
	for _, n := range []int{-3, 0, 1, 2, 3, 4, 5, 6, 7, 100} {
		delays[itoa(n)] = core.RetryDelay(n).String()
	}
	out := map[string]any{
		"statuses": []string{core.StatusPending, core.StatusRunning, core.StatusVerifying, core.StatusPassed,
			core.StatusFailed, core.StatusBlocked, core.StatusAbandoned},
		"run_statuses":      []string{core.RunRunning, core.RunComplete, core.RunHalted, core.RunAborted},
		"base_retry_delay":  core.BaseRetryDelay.String(),
		"max_retry_delay":   core.MaxRetryDelay.String(),
		"stall_timeout":     core.StallTimeout.String(),
		"retry_delay":       delays,
		"conflict_is":       errors.Is(wrap(core.ErrConflict), core.ErrConflict),
		"conflict_distinct": !errors.Is(core.ErrConflict, core.ErrMergeConflict),
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

type wrapped struct{ err error }

func (w wrapped) Error() string { return "wrapped: " + w.err.Error() }
func (w wrapped) Unwrap() error { return w.err }
func wrap(err error) error      { return wrapped{err} }

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
