package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// cmdOutcome is the result of one verification command.
type cmdOutcome struct {
	ExitCode    int
	Duration    time.Duration
	TimedOut    bool
	Interrupted bool  // the verifier's own context was cancelled
	Err         error // non-nil only if the command could not be started
}

// runCommand runs shell via bash -c in dir with the given environment.
//
// Output always goes to outPath (a file, never a pipe), so a child that
// keeps running cannot block Wait. The command gets its own process group.
// The group is SIGKILLed on timeout, on cancellation, and after the shell
// exits, so background children do not outlive the verdict.
//
// Limitation: a child that calls setsid() leaves the group. Stopping those
// needs a sandbox (D7), not this function.
func runCommand(ctx context.Context, dir string, env []string, shell string, timeout time.Duration, outPath string) cmdOutcome {
	out, err := os.Create(outPath)
	if err != nil {
		return cmdOutcome{ExitCode: -1, Err: err}
	}
	defer out.Close()

	cmd := exec.Command("bash", "-c", shell)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return cmdOutcome{ExitCode: -1, Err: err}
	}
	pgid := cmd.Process.Pid

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	res := cmdOutcome{}
	select {
	case err := <-done:
		res = finish(cmd, err, start)
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		err := <-done
		res = finish(cmd, err, start)
		res.TimedOut = true
		res.ExitCode = -1
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		err := <-done
		res = finish(cmd, err, start)
		res.Interrupted = true
		res.ExitCode = -1
	}
	// Reap anything the shell left behind in its group.
	//
	// Known limit (not fixed in M1): the shell has been reaped by now, so if
	// every member of the group has also exited, the pgid number is free and
	// the kernel could in theory hand it to an unrelated new process group
	// before this call, which would then be killed. The window is tiny
	// (microseconds, and PIDs are allocated sequentially). A pidfd or a cgroup
	// per command (D7, systemd-run scope) would close it.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	res.Duration = time.Since(start)
	return res
}

func finish(cmd *exec.Cmd, err error, _ time.Time) cmdOutcome {
	out := cmdOutcome{ExitCode: -1}
	if cmd.ProcessState != nil {
		out.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) && out.ExitCode == -1 {
			out.Err = err
		}
	}
	return out
}

// commandEnv is the environment verification commands run with. It is built
// from scratch: nothing from the verifier's own environment leaks in except
// PATH (needed to find toolchains) and LANG. HOME is a fresh directory, so
// per-user config, caches and git config are not shared with the verifier.
func commandEnv(home string) []string {
	env := []string{
		"PATH=" + absolutePath(os.Getenv("PATH")),
		"HOME=" + home,
		"LANG=" + getenvDefault("LANG", "C.UTF-8"),
		"TERM=dumb",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
	}
	return env
}

// absolutePath keeps only absolute PATH entries. A relative or empty entry
// resolves against the command's working directory, which is the export.
func absolutePath(p string) string {
	var keep []string
	for _, d := range filepath.SplitList(p) {
		if filepath.IsAbs(d) {
			keep = append(keep, d)
		}
	}
	return strings.Join(keep, string(filepath.ListSeparator))
}

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// logPathFor returns the per-command output path. Output is always written
// to a file, so it lands in logDir if given, or in workDir otherwise.
func logPathFor(dir, task, name string) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%s.log", sanitize(task), sanitize(name)))
}

func sanitize(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b = append(b, r)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}
