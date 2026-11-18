// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// core_test.go: RED BY DESIGN. The public internal/core contract,
// checked through conformance/coreprobe,
// which is built only with -tags nrcore so this package compiles before
// internal/core exists. A probe build failure is an assertion failure here.
package conformance

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

type probeOut struct {
	Statuses         []string          `json:"statuses"`
	RunStatuses      []string          `json:"run_statuses"`
	BaseRetryDelay   string            `json:"base_retry_delay"`
	MaxRetryDelay    string            `json:"max_retry_delay"`
	StallTimeout     string            `json:"stall_timeout"`
	RetryDelay       map[string]string `json:"retry_delay"`
	ConflictIs       bool              `json:"conflict_is"`
	ConflictDistinct bool              `json:"conflict_distinct"`
}

func coreProbe(t *testing.T) probeOut {
	t.Helper()
	cmd := exec.Command("go", "run", "-tags", "nrcore", "./conformance/coreprobe")
	cmd.Dir = moduleRoot
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("internal/core contract probe does not build or run: %v\n%s", err, clip(se.String()))
	}
	var p probeOut
	if err := json.Unmarshal([]byte(so.String()), &p); err != nil {
		t.Fatalf("coreprobe output: %v\n%s", err, so.String())
	}
	return p
}

// TestCoreContract: internal/core exports the contract's names with the contract's values.
// Clause: orchestrator#CORE-CONTRACT
func TestCoreContract(t *testing.T) {
	p := coreProbe(t)
	if got := strings.Join(p.Statuses, ","); got != "pending,running,verifying,passed,failed,blocked,abandoned" {
		t.Fatalf("task statuses: got %s", got)
	}
	if got := strings.Join(p.RunStatuses, ","); got != "running,complete,halted,aborted" {
		t.Fatalf("run statuses: got %s", got)
	}
	if p.BaseRetryDelay != "10s" || p.MaxRetryDelay != "5m0s" || p.StallTimeout != "5m0s" {
		t.Fatalf("defaults: base %s max %s stall %s", p.BaseRetryDelay, p.MaxRetryDelay, p.StallTimeout)
	}
	if !p.ConflictIs || !p.ConflictDistinct {
		t.Fatalf("ErrConflict must be a distinct sentinel usable with errors.Is")
	}
}

// TestCoreRetryDelay: RetryDelay(n) = min(10s·2^(n-1), 5m), n < 1 as 1.
// Clause: orchestrator#CORE-RETRYDELAY
// Clause: orchestrator#DY-BACKOFF
func TestCoreRetryDelay(t *testing.T) {
	p := coreProbe(t)
	want := map[string]string{"-3": "10s", "0": "10s", "1": "10s", "2": "20s", "3": "40s", "4": "1m20s",
		"5": "2m40s", "6": "5m0s", "7": "5m0s", "100": "5m0s"}
	for n, w := range want {
		if p.RetryDelay[n] != w {
			t.Fatalf("RetryDelay(%s): want %s, got %q", n, w, p.RetryDelay[n])
		}
	}
}
