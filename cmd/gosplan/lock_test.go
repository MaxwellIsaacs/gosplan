package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// While one gosplan run holds a run, a second `gosplan run --run ID` exits 2 at once
// with a clear message and changes nothing.
func TestRunSecondOrchestratorRefused(t *testing.T) {
	f := newAdvFix(t, "1.a", "1.a")
	if res := f.run("ADV_MODE=happy"); res.code != 0 {
		t.Fatalf("first run: exit %d\n%s", res.code, res.stderr)
	}
	run, err := f.store().LatestRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(f.state, "runs", run, "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	res := f.runArgs([]string{"--run", run}, "ADV_MODE=happy")
	if res.code != 2 || !strings.Contains(res.stderr, "run "+run+" is in use by another gosplan run") {
		t.Fatalf("second orchestrator: want exit 2 'in use', got %d\n%s", res.code, res.stderr)
	}
}
