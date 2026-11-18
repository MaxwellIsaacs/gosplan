package runner

import (
	"fmt"
	"io"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/spec"
)

// DryRun loads and validates the spec in dir and prints each task (tier,
// deps, deps source), then the waves in which tasks become ready if every
// task passes. It touches no database, worktree or branch.
func DryRun(dir string, w io.Writer) error {
	sp, err := spec.Load(dir)
	if err != nil {
		return err
	}
	for _, x := range sp.Warnings {
		fmt.Fprintf(w, "warning: %s\n", x)
	}
	fmt.Fprintf(w, "spec %s (sha256 %s)\n", sp.Dir, short(sp.ContentSHA256))
	fmt.Fprintln(w, "tasks:")
	for _, t := range sp.Tasks {
		fmt.Fprintf(w, "  %-8s tier %d  deps [%s] (%s)\n", t.ID, t.Tier, strings.Join(t.Deps, ", "), t.DepsSource)
	}
	fmt.Fprintln(w, "ready order if every task passes:")
	for i, wave := range readyWaves(sp) {
		fmt.Fprintf(w, "  %d: %s\n", i+1, strings.Join(wave, " "))
	}
	return nil
}

// readyWaves simulates a run in which every task passes: each wave is the set
// of tasks ready once every earlier wave has passed, in (tier, id) order.
func readyWaves(sp *spec.Spec) [][]string {
	status := map[string]string{}
	for _, t := range sp.Tasks {
		status[t.ID] = "pending"
	}
	var waves [][]string
	for {
		ready := sp.Ready(status)
		if len(ready) == 0 {
			return waves
		}
		for _, id := range ready {
			status[id] = "passed"
		}
		waves = append(waves, ready)
	}
}
