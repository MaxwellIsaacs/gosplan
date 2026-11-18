package driver

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcStartTimeAndOwns(t *testing.T) {
	c := exec.Command("sleep", "30")
	c.Env = append(os.Environ(), EnvNonce+"=run.abc")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	pid := c.Process.Pid
	st, err := ProcStartTime(pid)
	if err != nil || st == 0 {
		t.Fatalf("ProcStartTime = %d, %v", st, err)
	}
	if self, _ := ProcStartTime(os.Getpid()); self > st {
		t.Errorf("child started at %d, before this test process (%d)", st, self)
	}
	for _, c := range []struct {
		start uint64
		nonce string
		want  bool
	}{
		{st, "", true},            // start time matches
		{st + 1, "run.abc", true}, // nonce matches
		{st + 1, "run.abd", false},
		{0, "", false},
	} {
		if got := Owns(pid, c.start, c.nonce); got != c.want {
			t.Errorf("Owns(%d, %d, %q) = %v, want %v", pid, c.start, c.nonce, got, c.want)
		}
	}
	if Owns(1, 0, "x") {
		t.Error("Owns(1) = true")
	}
	if h := NonceHolders("run."); h[pid] != "run.abc" {
		t.Errorf("NonceHolders = %v, want pid %d", h, pid)
	}
}

// orphanGroup starts a leader in its own group with GOSPLAN_RUN=r1 and a run
// nonce; the leader starts `sleep 60` under `env <unset...>` and exits.
// It returns the leader pid (== pgid) and start time, and the child pid.
func orphanGroup(t *testing.T, unset ...string) (leader int, start uint64, child int) {
	t.Helper()
	var u []string
	for _, v := range unset {
		u = append(u, "-u", v)
	}
	c := exec.Command("sh", "-c", `env "$@" sleep 60 </dev/null >/dev/null 2>&1 & echo $!; sleep 0.2`, "sh")
	c.Args = append(c.Args, u...)
	c.Env = append(os.Environ(), "GOSPLAN_RUN=r1", EnvNonce+"=r1.abc")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	leader = c.Process.Pid
	start, _ = ProcStartTime(leader)
	var b [32]byte
	n, _ := out.Read(b[:])
	child, err = strconv.Atoi(strings.TrimSpace(string(b[:n])))
	if err != nil {
		t.Fatalf("child pid %q: %v", b[:n], err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	_ = c.Wait()
	return leader, start, child
}

func alive(pid int) bool { return Alive(pid) }

func waitDead(pid int) bool {
	for i := 0; i < 50; i++ {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Re-review 5a: the leader has exited and the group's only member dropped
// the nonce but still carries GOSPLAN_RUN: it is the run's group, killed.
func TestKillVerifiedOrphanGroupWithoutLeader(t *testing.T) {
	leader, st, child := orphanGroup(t, EnvNonce)
	if alive(leader) {
		t.Fatal("leader still alive")
	}
	if pg, err := procPgrp(child); err != nil || pg != leader {
		t.Fatalf("child pgrp %d (%v), want %d", pg, err, leader)
	}
	KillVerified(leader, leader, st, "r1.abc", "r1")
	if !waitDead(child) {
		t.Fatal("same-group child of a dead leader survived")
	}
}

// A group with no member carrying the run (it may be a reused pgid, or a
// child that stripped every GOSPLAN_ variable) is left alone. The second case
// is the documented open part: such a child survives until D7.
func TestKillVerifiedOrphanGroupUnmarkedLeftAlone(t *testing.T) {
	leader, st, child := orphanGroup(t, EnvNonce, "GOSPLAN_RUN")
	KillVerified(leader, leader, st, "r1.abc", "r1")
	time.Sleep(100 * time.Millisecond)
	if !alive(child) {
		t.Fatal("killed a group with no member carrying the run")
	}
	leader2, st2, child2 := orphanGroup(t, EnvNonce)
	KillVerified(leader2, leader2, st2, "r2.abc", "r2") // another run's lease
	time.Sleep(100 * time.Millisecond)
	if !alive(child2) {
		t.Fatal("killed a group marked with another run")
	}
}

// Re-review 5c: the lease's pgid is what gets signalled, so it must be the
// leader's own group. A lease whose pid is ours (nonce) but whose pgid
// names another group, or whose pid is not a group leader, gets no group
// kill; the nonce holder itself is still killed.
func TestKillVerifiedRequiresLeaderGroup(t *testing.T) {
	start := func(setpgid bool, env ...string) *exec.Cmd {
		c := exec.Command("sleep", "60")
		c.Env = append(os.Environ(), env...)
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: setpgid}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = c.Wait() }()
		t.Cleanup(func() { _ = c.Process.Kill() })
		return c
	}
	other := start(true) // an unrelated group the forged lease points at
	ours := start(true, EnvNonce+"=r1.n1")
	killed := KillVerified(ours.Process.Pid, other.Process.Pid, 0, "r1.n1", "r1")
	for _, k := range killed {
		if k < 0 {
			t.Errorf("group kill %d with pgid != pid", k)
		}
	}
	if !waitDead(ours.Process.Pid) {
		t.Error("nonce holder not killed")
	}
	if !alive(other.Process.Pid) {
		t.Error("the other group was killed")
	}

	member := start(false, EnvNonce+"=r1.n2") // in this test's group, not a leader
	if LeadsGroup(member.Process.Pid, member.Process.Pid) {
		t.Fatal("LeadsGroup true for a non-leader")
	}
	for _, k := range KillVerified(member.Process.Pid, member.Process.Pid, 0, "r1.n2", "r1") {
		if k < 0 {
			t.Errorf("group kill %d of a pid that leads no group", k)
		}
	}
	if !LeadsGroup(other.Process.Pid, other.Process.Pid) || LeadsGroup(other.Process.Pid, other.Process.Pid+1) {
		t.Error("LeadsGroup wrong for a real leader")
	}
}
