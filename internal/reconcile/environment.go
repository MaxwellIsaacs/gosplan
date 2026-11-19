package reconcile

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

// ReasonEnvironment halts a run whose environment cannot do the work: a
// verify gate's command is not installed or not executable, or the worker
// binary rejects the flags gosplan passes. Retrying cannot fix either, so
// the attempt is refunded, the task goes back to pending, and the run halts
// for a human to fix PATH or the claude install and `gosplan run --continue`.
const ReasonEnvironment = "environment"

// envGateFailure names the first verify command that exited 126 (found, not
// executable) or 127 (not found), or "" if none did. Those codes come from
// the shell, not from the project's tests.
func envGateFailure(verdictJSON []byte) string {
	var v struct {
		Commands []events.CommandResult `json:"commands"`
	}
	if json.Unmarshal(verdictJSON, &v) != nil {
		return ""
	}
	for _, c := range v.Commands {
		if c.ExitCode != 126 && c.ExitCode != 127 {
			continue
		}
		what := "command not found"
		if c.ExitCode == 126 {
			what = "command not executable"
		}
		return fmt.Sprintf("verify gate %s exited %d (%s): %s", c.Name, c.ExitCode, what, firstWord(c.Argv))
	}
	return ""
}

// firstWord is the program a gate runs: the first word of its shell string
// when it runs through `sh -c` / `bash -c`, else argv[0].
func firstWord(argv []string) string {
	if len(argv) >= 3 && (strings.HasSuffix(argv[0], "sh") || strings.HasSuffix(argv[0], "bash")) && argv[1] == "-c" {
		if f := strings.Fields(argv[2]); len(f) > 0 {
			return f[0]
		}
	}
	if len(argv) > 0 {
		return argv[0]
	}
	return "?"
}

// workerFlagError matches what the claude CLI prints when it does not know a
// flag gosplan passes (an old claude on PATH).
var workerFlagError = regexp.MustCompile(`(?i)(unknown option|unknown flag|unrecognized option|invalid option|error: option '--)`)

// envWorkerFailure reports why a worker that exited non-zero without a
// result line was the environment's fault, or "". Exit 126/127 means the
// worker binary (or what it execs) is missing; a flag error on stderr means
// the claude on PATH is too old for gosplan's argv.
func envWorkerFailure(code int, stderrPath string) string {
	if code == 126 || code == 127 {
		return fmt.Sprintf("worker exited %d: the claude binary or something it runs is missing or not executable", code)
	}
	b, err := os.ReadFile(stderrPath)
	if err != nil {
		return ""
	}
	if len(b) > 64<<10 {
		b = b[len(b)-64<<10:]
	}
	for _, line := range strings.Split(string(b), "\n") {
		if workerFlagError.MatchString(line) {
			return "worker rejected its flags (is the claude on PATH too old?): " + truncate(strings.TrimSpace(line))
		}
	}
	return ""
}

// envPark (inside a tx): give the attempt back and return the task to
// pending; the caller halts the run with ReasonEnvironment after the commit.
func (r *Reconciler) envPark(x *txn, id, from, why string) error {
	if err := x.refund(id); err != nil {
		return err
	}
	return x.transition(id, from, core.StatusPending, truncate(ReasonEnvironment+": "+why))
}

// ReasonDiskLow halts a run whose disk fell under Config.MinFreeDisk. It
// stops while gosplan.db and the logs can still be written, so the run ends
// with an intact record and `gosplan run --continue` can finish it.
const ReasonDiskLow = "disk_low"

// diskLow returns a halt reason when free space is under the floor, else "".
// A failed measurement is not a reason to halt.
func (r *Reconciler) diskLow() string {
	if r.cfg.DiskFree == nil || r.cfg.MinFreeDisk <= 0 {
		return ""
	}
	free, at, err := r.cfg.DiskFree()
	if err != nil || free >= r.cfg.MinFreeDisk {
		return ""
	}
	return fmt.Sprintf("%s: %.1f GB free on %s, under the %.1f GB floor (--min-free-disk)",
		ReasonDiskLow, float64(free)/(1<<30), at, float64(r.cfg.MinFreeDisk)/(1<<30))
}
