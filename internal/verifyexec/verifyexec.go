// Package verifyexec is the real core.Verifier: it runs the M1 verifier
// `gosplan verify` as a subprocess and maps its result without trusting it more
// than it has to. Exit 0 means the process exited 0 AND its JSON says "pass";
// anything that cannot be confirmed is Exit 2 (could not verify).
package verifyexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
)

// Exec is the real core.Verifier: it runs `<GosplanBin> verify ...`.
type Exec struct {
	GosplanBin string        // path to gosplan (required; runner resolves os.Executable)
	Timeout    time.Duration // required
	OutDir     string        // each invocation gets <OutDir>/<task>.<n>.<seq>/verdict.{json,stderr,exit}
}

var _ core.Verifier = Exec{}

// KillGrace is how long `gosplan verify` gets after SIGTERM (on timeout or
// cancel) before its process group is SIGKILLed. gosplan verify turns SIGTERM
// into a context cancel, and the verifier then kills each gate command's
// own process group, which a SIGKILL of gosplan verify alone would orphan.
var KillGrace = 10 * time.Second

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// verdict is the subset of verify.Verdict that is read back.
type verdict struct {
	Result  string `json:"result"`
	TreeSHA string `json:"tree_sha"`
	Gates   []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"gates"`
	FailedGates []string `json:"failed_gates"`
}

// Args builds the argv for `gosplan verify`. It never includes --cache,
// --task-base or --attempt.
func Args(req core.VerifyRequest) []string {
	a := []string{"verify", "--project", req.Project, "--task", req.Task, "--base", req.Base, "--head", req.Head}
	for _, s := range req.Scope {
		a = append(a, "--scope", s)
	}
	if req.AllowNewProtected {
		a = append(a, "--allow-new-protected")
	}
	a = append(a, "--json")
	if req.EventLog != "" {
		a = append(a, "--log", req.EventLog)
		if req.Run != "" {
			a = append(a, "--run", req.Run)
		}
	}
	if req.LogsDir != "" {
		a = append(a, "--logs", req.LogsDir)
	}
	return a
}

// claimDir creates base.<seq> for the lowest seq >= 1 that does not exist
// yet. Mkdir is atomic, so every invocation, in this process or another
// (a straggler `gosplan verify` of a crashed gosplan run, or a later resume), gets
// its own directory and none writes over another's verdict.
func claimDir(base string) (string, int, error) {
	for seq := 1; ; seq++ {
		dir := base + "." + strconv.Itoa(seq)
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, seq, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", 0, err
		}
	}
}

// VerifyNonce is the GOSPLAN_ATTEMPT_NONCE `gosplan verify` runs with:
// "<run>.verify.<task>.<n>". Its run prefix lets a resumed gosplan run find and
// kill the verifies (and their gate commands) of a crashed one, as it does
// workers, before it starts verifies of its own.
func VerifyNonce(req core.VerifyRequest) string {
	return req.Run + ".verify." + req.Task + "." + strconv.Itoa(req.Attempt)
}

// verifyEnv is base with GOSPLAN_ATTEMPT_NONCE set to VerifyNonce(req) (when
// the request names a run) and no other value of it.
func verifyEnv(base []string, req core.VerifyRequest) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if !strings.HasPrefix(kv, envNonce+"=") {
			out = append(out, kv)
		}
	}
	if req.Run != "" {
		out = append(out, envNonce+"="+VerifyNonce(req))
	}
	return out
}

// envNonce is driver.EnvNonce (not imported: verifyexec does not depend on
// the worker driver).
const envNonce = "GOSPLAN_ATTEMPT_NONCE"

func couldNot(format string, args ...any) core.VerifyOutcome {
	return core.VerifyOutcome{Exit: 2, Detail: fmt.Sprintf(format, args...)}
}

// Verify runs gosplan verify. The error is only for programming errors; every
// verifier problem is an Exit 2 outcome.
func (e Exec) Verify(ctx context.Context, req core.VerifyRequest) (core.VerifyOutcome, error) {
	bin := e.GosplanBin
	if bin == "" {
		return core.VerifyOutcome{}, errors.New("verifyexec: GosplanBin is empty")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		return core.VerifyOutcome{}, errors.New("verifyexec: Timeout is zero")
	}
	if e.OutDir == "" {
		return core.VerifyOutcome{}, errors.New("verifyexec: OutDir is empty")
	}
	if !fullSHA.MatchString(req.Base) || !fullSHA.MatchString(req.Head) {
		return couldNot("base %q and head %q must be full 40-char SHAs", req.Base, req.Head), nil
	}
	if err := os.MkdirAll(e.OutDir, 0o700); err != nil {
		return couldNot("create out dir: %v", err), nil
	}
	dir, seq, err := claimDir(filepath.Join(e.OutDir, req.Task+"."+strconv.Itoa(req.Attempt)))
	if err != nil {
		return couldNot("create verify dir: %v", err), nil
	}
	if req.LogsDir != "" {
		req.LogsDir = filepath.Join(req.LogsDir, strconv.Itoa(seq))
	}
	prefix := filepath.Join(dir, "verdict")
	stdout, err := os.Create(prefix + ".json")
	if err != nil {
		return couldNot("create %s.json: %v", prefix, err), nil
	}
	defer stdout.Close()
	stderr, err := os.Create(prefix + ".stderr")
	if err != nil {
		return couldNot("create %s.stderr: %v", prefix, err), nil
	}
	defer stderr.Close()

	cmd := exec.Command(bin, Args(req)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = verifyEnv(os.Environ(), req)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return couldNot("start %s: %v", bin, err), nil
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var cause string
	killed := make(chan string, 1)
	stop := func() {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(KillGrace):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	go func() {
		select {
		case <-done:
		case <-timer.C:
			killed <- fmt.Sprintf("timed out after %s", timeout)
			stop()
		case <-ctx.Done():
			killed <- "cancelled: " + ctx.Err().Error()
			stop()
		}
	}()
	waitErr := cmd.Wait()
	close(done)
	// Reap stragglers the verifier left in its group.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case cause = <-killed:
	default:
	}

	code := -1
	var sigDesc string
	if waitErr == nil {
		code = 0
	} else {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sigDesc = "killed by signal " + ws.Signal().String()
			} else {
				code = ee.ExitCode()
			}
		} else {
			sigDesc = waitErr.Error()
		}
	}
	_ = os.WriteFile(prefix+".exit", []byte(strconv.Itoa(code)+"\n"), 0o600)

	fail := func(reason string) (core.VerifyOutcome, error) {
		return couldNot("%s%s", reason, tailStderr(prefix+".stderr")), nil
	}
	switch {
	case cause != "":
		return fail("verifier " + cause)
	case sigDesc != "":
		return fail("verifier " + sigDesc)
	case code != 0 && code != 1:
		return fail(fmt.Sprintf("verifier exited %d", code))
	}

	raw, err := os.ReadFile(prefix + ".json")
	if err != nil {
		return fail("read verdict: " + err.Error())
	}
	var v verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return fail(fmt.Sprintf("verifier exited %d with unparseable JSON: %v", code, err))
	}
	out := core.VerifyOutcome{Result: v.Result, TreeSHA: v.TreeSHA, VerdictJSON: raw}
	switch {
	case code == 0 && v.Result == "pass" && len(v.FailedGates) == 0:
		out.Exit = 0
		return out, nil
	case code == 1 && v.Result == "fail" && len(v.FailedGates) > 0:
		out.Exit = 1
		out.FailedGates = v.FailedGates
		out.Detail = failDetail(v)
		return out, nil
	}
	return fail(fmt.Sprintf("verifier exited %d but verdict is inconsistent (result %q, %d failed gates)", code, v.Result, len(v.FailedGates)))
}

func failDetail(v verdict) string {
	var parts []string
	for _, g := range v.Gates {
		if g.Status == "fail" {
			parts = append(parts, g.Name+": "+g.Detail)
		}
	}
	if len(parts) == 0 {
		for _, n := range v.FailedGates {
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, "; ")
}

// tailStderr returns ": <last 20 lines>" of the file, or "" if it is empty.
func tailStderr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	b = bytes.TrimRight(b, "\n")
	if len(b) == 0 {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return "\n" + strings.Join(lines, "\n")
}
