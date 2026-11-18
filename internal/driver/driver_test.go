package driver

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// The race detector sleeps 1s at exit by default; launch() sets
// GORACE=atexit_sleep_ms=0 so short stall timeouts are not eaten by startup.
//
// TestMain re-executes the test binary as a tiny child program when
// DRIVER_HELPER is set. Modes: "lines N interval", "silent", "bgchild",
// "exit N", "longline N". Arguments come in DRIVER_ARGS (space separated).
func TestMain(m *testing.M) {
	if mode := os.Getenv("DRIVER_HELPER"); mode != "" {
		helperMain(mode, strings.Fields(os.Getenv("DRIVER_ARGS")))
		return
	}
	os.Exit(m.Run())
}

func helperMain(mode string, args []string) {
	switch mode {
	case "lines":
		n, _ := strconv.Atoi(args[0])
		d, _ := time.ParseDuration(args[1])
		for i := 0; i < n; i++ {
			fmt.Printf("line %d\n", i)
			time.Sleep(d)
		}
	case "silent":
		time.Sleep(30 * time.Second)
	case "bgchild":
		// Child inherits stdout and outlives the leader.
		c := exec.Command("sleep", "60")
		c.Stdout = os.Stdout
		if err := c.Start(); err != nil {
			os.Exit(99)
		}
		fmt.Println("started")
	case "setsidchild":
		// Child leaves the group (setsid), keeps stdout and outlives the leader.
		c := exec.Command("sleep", "60")
		c.Stdout = os.Stdout
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			os.Exit(99)
		}
		fmt.Printf("child %d\n", c.Process.Pid)
	case "exit":
		n, _ := strconv.Atoi(args[0])
		os.Exit(n)
	case "longline":
		n, _ := strconv.Atoi(args[0])
		fmt.Println(strings.Repeat("x", n))
	}
	os.Exit(0)
}

func launch(t *testing.T, ctx context.Context, mode string, args ...string) (*Proc, string) {
	t.Helper()
	dir := t.TempDir()
	stream := filepath.Join(dir, "stream.jsonl")
	h, err := Exec{}.Launch(ctx, core.LaunchRequest{
		Argv:       []string{os.Args[0]},
		Dir:        dir,
		Env:        []string{"DRIVER_HELPER=" + mode, "DRIVER_ARGS=" + strings.Join(args, " "), "PATH=" + os.Getenv("PATH"), "GORACE=atexit_sleep_ms=0"},
		StreamPath: stream,
		StderrPath: filepath.Join(dir, "stderr.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = KillGroup(h.PGID()) })
	return h.(*Proc), stream
}

func TestDriverCapturesLinesAndFile(t *testing.T) {
	p, stream := launch(t, context.Background(), "lines", "5", "1ms")
	var got []string
	for l := range p.Lines() {
		got = append(got, string(l))
	}
	if e := p.Wait(); e.Code != 0 {
		t.Fatalf("exit = %+v", e)
	}
	want := "line 0\nline 1\nline 2\nline 3\nline 4\n"
	if strings.Join(got, "\n")+"\n" != want {
		t.Fatalf("lines = %q", got)
	}
	b, err := os.ReadFile(stream)
	if err != nil || string(b) != want {
		t.Fatalf("stream file = %q, %v", b, err)
	}
	if fi, _ := os.Stat(stream); fi.Mode().Perm() != 0o600 {
		t.Fatalf("stream mode = %v", fi.Mode())
	}
}

func TestDriverLongLine(t *testing.T) {
	p, _ := launch(t, context.Background(), "longline", "200000")
	var got [][]byte
	for l := range p.Lines() {
		got = append(got, l)
	}
	p.Wait()
	if len(got) != 1 || !bytes.Equal(got[0], bytes.Repeat([]byte("x"), 200000)) {
		t.Fatalf("got %d lines, first len %d", len(got), len(got[0]))
	}
}

func TestDriverExitCode(t *testing.T) {
	p, _ := launch(t, context.Background(), "exit", "3")
	e := p.Wait()
	if e.Code != 3 || e.Stalled || e.Killed || e.Signal != "" {
		t.Fatalf("exit = %+v", e)
	}
	if e2 := p.Wait(); e2.Code != e.Code {
		t.Fatalf("second Wait differs: %+v", e2)
	}
}

func TestDriverKillsGroupAfterLeaderExits(t *testing.T) {
	p, _ := launch(t, context.Background(), "bgchild")
	done := make(chan core.Exit, 1)
	go func() { done <- p.Wait() }()
	select {
	case e := <-done:
		if e.Code != 0 {
			t.Fatalf("exit = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return: background child held stdout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for GroupAlive(p.PGID()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if GroupAlive(p.PGID()) {
		t.Fatal("process group still alive after Wait")
	}
}

func TestDriverOwnProcessGroup(t *testing.T) {
	p, _ := launch(t, context.Background(), "silent")
	if p.PGID() != p.PID() {
		t.Fatalf("pgid %d != pid %d", p.PGID(), p.PID())
	}
	if p.PGID() == syscall.Getpgrp() {
		t.Fatal("worker shares the test's process group")
	}
	got, err := syscall.Getpgid(p.PID())
	if err != nil || got != p.PID() {
		t.Fatalf("Getpgid = %d, %v", got, err)
	}
}

func TestDriverContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, _ := launch(t, ctx, "silent")
	cancel()
	done := make(chan core.Exit, 1)
	go func() { done <- p.Wait() }()
	select {
	case e := <-done:
		if e.Code != -1 || e.Signal != "killed" || !e.Killed {
			t.Fatalf("exit = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not kill the worker")
	}
}

func TestDriverEnvIsExact(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRIVER_LEAK", "leaked")
	h, err := Exec{}.Launch(context.Background(), core.LaunchRequest{
		Argv: []string{"/bin/sh", "-c", `echo "[$DRIVER_LEAK][$ONLY]"`},
		Dir:  dir, Env: []string{"ONLY=yes"},
		StreamPath: filepath.Join(dir, "s"), StderrPath: filepath.Join(dir, "e"),
	})
	if err != nil {
		t.Fatal(err)
	}
	l := <-h.Lines()
	h.Wait()
	if string(l) != "[][yes]" {
		t.Fatalf("line = %q", l)
	}
}

func TestDriverDropsWhenNobodyReads(t *testing.T) {
	p, stream := launch(t, context.Background(), "lines", strconv.Itoa(linesBuffer+50), "0s")
	p.Wait() // returns although nobody drained Lines()
	if p.Dropped() != 50 {
		t.Fatalf("dropped = %d, want 50", p.Dropped())
	}
	b, _ := os.ReadFile(stream)
	if n := bytes.Count(b, []byte("\n")); n != linesBuffer+50 {
		t.Fatalf("stream has %d lines", n)
	}
}

func TestKillGroupMissingIsNotError(t *testing.T) {
	c := exec.Command("true")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if err := KillGroup(c.Process.Pid); err != nil {
		t.Fatalf("KillGroup of dead group: %v", err)
	}
	if GroupAlive(c.Process.Pid) {
		t.Fatal("dead group reported alive")
	}
}

func TestLeaseOwnerFormat(t *testing.T) {
	a, b := LeaseOwner(), LeaseOwner()
	parts := strings.Split(a, ":")
	if len(parts) < 3 || parts[len(parts)-2] != strconv.Itoa(os.Getpid()) || a == b {
		t.Fatalf("owner %q / %q", a, b)
	}
}

// supervised launches mode and seeds a lease for it.
func supervised(t *testing.T, mode string, args ...string) (*state.Store, core.AttemptRef, *Proc) {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	db := s.DB()
	for _, q := range []string{
		`INSERT INTO specs (dir, content_sha256, imported_at) VALUES ('/s', 'x', '2026-01-01T00:00:00Z')`,
		`INSERT INTO runs (id, spec_id, project, base_sha, status, started_at) VALUES ('r', 1, '/p', '0', 'running', '2026-01-01T00:00:00Z')`,
		`INSERT INTO tasks (run_id, id, tier, prompt_path, status, deps_source) VALUES ('r', 't', 1, '/p.md', 'running', 'tiers')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var ref core.AttemptRef
	err = s.Immediate(context.Background(), func(tx *sql.Tx) error {
		var err error
		ref, err = state.StartAttempt(tx, time.Now().Add(-time.Hour), state.AttemptStart{Run: "r", Task: "t", SessionID: "s", Model: "m", Owner: LeaseOwner()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := launch(t, context.Background(), mode, args...)
	return s, ref, p
}

func TestSupervisorStallKills(t *testing.T) {
	s, ref, p := supervised(t, "silent")
	sup := &Supervisor{Store: s, StallTimeout: 300 * time.Millisecond}
	start := time.Now()
	e := sup.Run(context.Background(), ref, p)
	if !e.Stalled || e.Killed {
		t.Fatalf("exit = %+v", e)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	if GroupAlive(p.PGID()) {
		t.Fatal("group alive after stall kill")
	}
}

func TestSupervisorActiveWorkerNotStalled(t *testing.T) {
	s, ref, p := supervised(t, "lines", "10", "100ms")
	var seen atomic.Int32
	sup := &Supervisor{Store: s, StallTimeout: 300 * time.Millisecond, OnLine: func([]byte) { seen.Add(1) }}
	e := sup.Run(context.Background(), ref, p)
	if e.Stalled || e.Code != 0 {
		t.Fatalf("exit = %+v", e)
	}
	if seen.Load() != 10 {
		t.Fatalf("OnLine saw %d lines", seen.Load())
	}
}

func TestSupervisorHeartbeatThrottled(t *testing.T) {
	s, ref, p := supervised(t, "lines", "150", "10ms")
	var beats atomic.Int32
	sup := &Supervisor{Store: s, StallTimeout: 5 * time.Second}
	sup.heartbeat = func(ctx context.Context, r core.AttemptRef, at time.Time) error {
		beats.Add(1)
		return s.Heartbeat(ctx, r, at)
	}
	before, _ := s.Leases(context.Background(), "r")
	e := sup.Run(context.Background(), ref, p)
	if e.Code != 0 || e.Stalled {
		t.Fatalf("exit = %+v", e)
	}
	if n := beats.Load(); n < 1 || n > 3 {
		t.Fatalf("heartbeat writes = %d, want 1..3 for ~150 lines", n)
	}
	after, _ := s.Leases(context.Background(), "r")
	if !after[0].HeartbeatAt.After(before[0].HeartbeatAt) {
		t.Fatalf("heartbeat did not advance: %v -> %v", before[0].HeartbeatAt, after[0].HeartbeatAt)
	}
}

// A setsid child holding stdout used to keep Wait blocked forever: now Wait
// returns ExitGrace after the leader exits, with the leader's exit code and
// every line it wrote.
func TestDriverSetsidChildHoldingStdout(t *testing.T) {
	p, _ := launch(t, context.Background(), "setsidchild")
	var lines []string
	for l := range p.Lines() {
		lines = append(lines, string(l))
	}
	start := time.Now()
	e := p.Wait()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "child ") {
		t.Fatalf("lines = %q", lines)
	}
	pid, _ := strconv.Atoi(strings.TrimPrefix(lines[0], "child "))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if e.Code != 0 || e.Err != nil {
		t.Fatalf("exit = %+v", e)
	}
	if d := time.Since(start); d > ExitGrace+2*time.Second {
		t.Fatalf("Wait took %v after Lines closed", d)
	}
	// The escapee is still alive: the documented limit until D7.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Logf("setsid child already gone: %v", err)
	}
}

// A heartbeat write stuck on a locked DB must not slow reading: every line
// reaches OnLine and Run returns long before the blocked writes would let it.
func TestSupervisorHeartbeatOffReadPath(t *testing.T) {
	s, ref, p := supervised(t, "lines", "50", "10ms")
	var seen atomic.Int32
	release := make(chan struct{})
	defer close(release)
	sup := &Supervisor{Store: s, StallTimeout: 5 * time.Second, OnLine: func([]byte) { seen.Add(1) }}
	sup.heartbeat = func(ctx context.Context, r core.AttemptRef, at time.Time) error {
		<-release // as if gosplan.db stayed locked
		return nil
	}
	start := time.Now()
	e := sup.Run(context.Background(), ref, p)
	if e.Code != 0 || seen.Load() != 50 {
		t.Fatalf("exit %+v, %d lines", e, seen.Load())
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Run took %v with a blocked heartbeat", d)
	}
}
