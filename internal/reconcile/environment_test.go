package reconcile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

func envFailOutcome(code int) core.VerifyOutcome {
	b, _ := json.Marshal(map[string]any{
		"result": "fail", "failed_gates": []string{"cmd:test"},
		"commands": []events.CommandResult{
			{Name: "lint", Argv: []string{"sh", "-c", "go vet ./..."}, ExitCode: 0},
			{Name: "test", Argv: []string{"sh", "-c", "nix shell nixpkgs#go -c go test ./..."}, ExitCode: code},
		},
	})
	return core.VerifyOutcome{Exit: 1, Result: "fail", FailedGates: []string{"cmd:test"}, VerdictJSON: b, Detail: "exit 127"}
}

// TestReconcileEnvGateHaltsAndRefunds: a gate that exits 127 (nix not on
// PATH) halts the run environment after one attempt, with the attempt given
// back and the task pending, instead of spending every attempt.
func TestReconcileEnvGateHaltsAndRefunds(t *testing.T) {
	h := newHarness(t, task("1.a", 1), task("2.a", 2, "1.a"))
	h.v.outcome = func(string, int) core.VerifyOutcome { return envFailOutcome(127) }
	o := h.runOutcome()
	if o.Status != core.RunHalted {
		t.Fatalf("run status = %s, want halted", o.Status)
	}
	if n := len(h.v.callsFor("1.a")); n != 1 {
		t.Errorf("verify calls = %d, want 1 (no retry into the same missing command)", n)
	}
	tr := h.task("1.a")
	if tr.Status != core.StatusPending || tr.AttemptsUsed != 0 {
		t.Errorf("1.a = %s, attempts_used %d; want pending, 0", tr.Status, tr.AttemptsUsed)
	}
	if st := h.task("2.a").Status; st != core.StatusPending {
		t.Errorf("2.a = %s, want pending (not abandoned)", st)
	}
	if !strings.HasPrefix(o.Reason, ReasonEnvironment+": task 1.a: verify gate test exited 127") || !strings.Contains(o.Reason, "nix") {
		t.Errorf("halt reason = %q", o.Reason)
	}
}

// TestReconcileRealGateFailureStillRetries: exit 1 is the project's tests
// failing, not the environment; it takes the retry path.
func TestReconcileRealGateFailureStillRetries(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MaxAttempts = 2
	h.v.outcome = func(string, int) core.VerifyOutcome { return envFailOutcome(1) }
	h.run()
	if tr := h.task("1.a"); tr.Status != core.StatusFailed || tr.AttemptsUsed != 2 {
		t.Errorf("1.a = %s after %d attempts, want failed after 2", tr.Status, tr.AttemptsUsed)
	}
}

// TestReconcileEnvWorkerExit127Halts: a worker that exits 127 with no result
// line (claude missing) halts environment and is refunded.
func TestReconcileEnvWorkerExit127Halts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.l.script = func(string, int) step { return step{code: 127} }
	o := h.runOutcome()
	if o.Status != core.RunHalted {
		t.Fatalf("run status = %s", o.Status)
	}
	if tr := h.task("1.a"); tr.Status != core.StatusPending || tr.AttemptsUsed != 0 {
		t.Errorf("1.a = %s, attempts_used %d; want pending, 0", tr.Status, tr.AttemptsUsed)
	}
	if launches, _ := h.l.snapshot(); len(launches) != 1 {
		t.Errorf("launches = %d, want 1", len(launches))
	}
	if r := o.Reason; !strings.Contains(r, ReasonEnvironment) || !strings.Contains(r, "exited 127") {
		t.Errorf("halt reason = %q", r)
	}
}

func TestEnvWorkerFailureFlagError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "1.a.1.stderr")
	os.WriteFile(p, []byte("some banner\nerror: unknown option '--permission-mode'\n"), 0o644)
	if got := envWorkerFailure(1, p); !strings.Contains(got, "unknown option '--permission-mode'") {
		t.Errorf("flag error: %q", got)
	}
	os.WriteFile(p, []byte("Error: tests failed\n"), 0o644)
	if got := envWorkerFailure(1, p); got != "" {
		t.Errorf("ordinary failure classified as environment: %q", got)
	}
	if got := envWorkerFailure(1, p+".missing"); got != "" {
		t.Errorf("missing stderr: %q", got)
	}
}

// TestReconcileDiskLowHalts: free space under the floor halts disk_low
// before anything else goes wrong; nothing is spent.
func TestReconcileDiskLowHalts(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	h.cfg.MinFreeDisk = 3 << 30
	h.cfg.DiskFree = func() (int64, string, error) { return 1 << 30, "/state", nil }
	h.l.script = func(string, int) step { return step{hang: true, heartbeat: 20 * time.Millisecond} }
	o := h.runOutcome()
	if o.Status != core.RunHalted || !strings.HasPrefix(o.Reason, ReasonDiskLow+": 1.0 GB free on /state") {
		t.Fatalf("outcome = %s %q, want halted disk_low", o.Status, o.Reason)
	}
}
