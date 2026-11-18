package driver

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// EnvNonce is the per-attempt nonce in every worker's environment. The
// lease row holds the same value, so a restarted orchestrator can tell its
// own orphaned workers from unrelated processes that reused the pid.
const EnvNonce = "GOSPLAN_ATTEMPT_NONCE"

// procStat returns the fields of /proc/<pid>/stat after the comm field
// (index 0 is field 3, "state").
func procStat(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return nil, fmt.Errorf("driver: /proc/%d/stat: no comm field", pid)
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return nil, fmt.Errorf("driver: /proc/%d/stat: %d fields", pid, len(f))
	}
	return f, nil
}

// ProcStartTime returns field 22 of /proc/<pid>/stat: the process start
// time in clock ticks after boot. With the pid, it names one process for
// the life of the boot.
func ProcStartTime(pid int) (uint64, error) {
	f, err := procStat(pid)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// procEnvHas reports whether /proc/<pid>/environ has the entry kv. A process
// of another user (EPERM) or a gone one reads as false.
func procEnvHas(pid int, kv string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	for _, e := range bytes.Split(b, []byte{0}) {
		if string(e) == kv {
			return true
		}
	}
	return false
}

// Owns reports whether pid is the worker a lease describes: its start time
// equals startTime (when recorded), or its environment carries
// GOSPLAN_ATTEMPT_NONCE=nonce (when recorded). Anything unreadable is not ours.
func Owns(pid int, startTime uint64, nonce string) bool {
	if pid <= 1 {
		return false
	}
	if startTime != 0 {
		if st, err := ProcStartTime(pid); err == nil && st == startTime {
			return true
		}
	}
	return nonce != "" && procEnvHas(pid, EnvNonce+"="+nonce)
}

// NonceHolders returns the live processes of this user whose environment
// has an GOSPLAN_ATTEMPT_NONCE entry starting with prefix, mapped to the entry's
// value. Processes it cannot read are skipped.
func NonceHolders(prefix string) map[int]string {
	out := map[int]string{}
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	key := []byte(EnvNonce + "=")
	self := os.Getpid()
	for _, d := range dirs {
		pid, err := strconv.Atoi(filepath.Base(d))
		if err != nil || pid == self {
			continue
		}
		b, err := os.ReadFile(d + "/environ")
		if err != nil {
			continue
		}
		for _, e := range bytes.Split(b, []byte{0}) {
			if v, ok := bytes.CutPrefix(e, key); ok && strings.HasPrefix(string(v), prefix) {
				out[pid] = string(v)
			}
		}
	}
	return out
}

// procPgrp is field 5 of /proc/<pid>/stat: the process group.
func procPgrp(pid int) (int, error) {
	f, err := procStat(pid)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(f[2])
}

// carriesRun reports whether pid's environment marks it as one of run's:
// an GOSPLAN_ATTEMPT_NONCE with the run's prefix, or GOSPLAN_RUN=<run>.
func carriesRun(pid int, run string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	for _, e := range bytes.Split(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(e, []byte(EnvNonce+"=")); ok && strings.HasPrefix(string(v), run+".") {
			return true
		}
		if string(e) == "GOSPLAN_RUN="+run {
			return true
		}
	}
	return false
}

// orphanGroupIsRuns reports whether a group whose leader is gone still
// holds a live process that carries run (see carriesRun). Linux does not
// hand out a pid that is still some group's id, so a live member with the
// run's mark means the group is the one the lease's leader made; with no
// such member the group may be a reused one and is left alone.
func orphanGroupIsRuns(pgid int, run string) bool {
	if run == "" {
		return false
	}
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		pid, err := strconv.Atoi(filepath.Base(d))
		if err != nil {
			continue
		}
		if pg, err := procPgrp(pid); err == nil && pg == pgid && carriesRun(pid, run) {
			return true
		}
	}
	return false
}

// Alive reports whether pid is a live process (not gone, not a zombie).
func Alive(pid int) bool {
	f, err := procStat(pid)
	return err == nil && f[0] != "Z"
}

// LeadsGroup reports whether pid is the leader of process group pgid:
// pgid == pid (what Setpgid gives the worker) and /proc/<pid>/stat agrees,
// or the process has already exited (nothing to contradict it).
func LeadsGroup(pid, pgid int) bool {
	if pid <= 1 || pgid != pid {
		return false
	}
	pg, err := procPgrp(pid)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return pg == pgid
}

// KillHolder SIGKILLs pid, a process found carrying one of the run's
// attempt nonces. If it leads its own process group it is a worker leader
// (the launcher starts every worker with Setpgid) and its nonce proves the
// group is the run's, lease or not (a crash before the pid was recorded),
// so the whole group is killed first: children that dropped the nonce die
// with it.
func KillHolder(pid int) error {
	if Alive(pid) && LeadsGroup(pid, pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

// KillVerified SIGKILLs the group of a lease's worker of run, and then every
// process carrying the lease's nonce (children that outlived the leader,
// including setsid ones that kept the environment). The group is killed
// only if the lease's pgid is its pid, and the leader is still ours (Owns)
// and its /proc pgrp is that group, or, once the leader has exited,
// if some live member of the group still carries the run (an attempt nonce
// of the run, or GOSPLAN_RUN=<run>): a child that dropped only the nonce dies
// with its group. It never returns an error: EPERM or ESRCH mean the
// process is not ours or gone, and the caller ends the attempt `lost`
// either way. It reports what it killed.
func KillVerified(pid, pgid int, startTime uint64, nonce, run string) []int {
	var killed []int
	// Workers are started with Setpgid, so the lease's group is the
	// leader's pid; a lease naming any other group is not killed by group.
	if pgid > 1 && pgid == pid {
		pg, err := procPgrp(pid)
		own := err == nil && pg == pgid && Owns(pid, startTime, nonce)
		gone := err != nil && errors.Is(err, os.ErrNotExist)
		if own || (gone && orphanGroupIsRuns(pgid, run)) {
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
				killed = append(killed, -pgid)
			}
		}
	}
	if nonce != "" {
		for p, v := range NonceHolders(nonce) {
			if v != nonce {
				continue
			}
			if err := KillHolder(p); err == nil || !errors.Is(err, syscall.ESRCH) {
				killed = append(killed, p)
			}
		}
	}
	return killed
}
