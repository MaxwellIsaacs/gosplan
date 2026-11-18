package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/driver"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// The lease row already holds the attempt's nonce when the worker is
// launched (it is written in the dispatch transaction), and the worker's
// environment carries the same nonce. A crash between Launch and
// SetLeaseProcess therefore still leaves enough to find the worker.
func TestReconcileLeaseNonceBeforeLaunch(t *testing.T) {
	h := newHarness(t, task("1.a", 1))
	var checked bool
	h.l.onLaunch = func(req core.LaunchRequest) {
		ls, err := h.store.Leases(context.Background(), testRun)
		if err != nil || len(ls) != 1 {
			t.Errorf("leases at launch: %+v, %v", ls, err)
			return
		}
		l := ls[0]
		if !strings.HasPrefix(l.Nonce, testRun+".") || l.PID != 0 {
			t.Errorf("lease at launch = %+v, want a nonce and no pid yet", l)
		}
		want := driver.EnvNonce + "=" + l.Nonce
		n := 0
		for _, kv := range req.Env {
			if strings.HasPrefix(kv, driver.EnvNonce+"=") {
				n++
				if kv != want {
					t.Errorf("worker env has %s, lease has %s", kv, want)
				}
			}
		}
		if n != 1 {
			t.Errorf("worker env has %d %s entries, want 1", n, driver.EnvNonce)
		}
		checked = true
	}
	h.cfg.BuildLaunch = func(tr state.TaskRow, ref core.AttemptRef, prompt, sessionID, model string, wt core.Prepared) core.LaunchRequest {
		// An inherited nonce must be replaced, not duplicated.
		return core.LaunchRequest{Argv: []string{"fakeclaude"}, Env: []string{driver.EnvNonce + "=inherited"}}
	}
	if got := h.run(); got != core.RunComplete {
		t.Fatalf("run = %s", got)
	}
	if !checked {
		t.Fatal("onLaunch never ran")
	}
}
