package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
)

// replayBudget is `gosplan replay`'s budget step, called after the M1
// replay succeeded. logPath is --log, budgetLog is --budget-log ("" = the
// budget.jsonl next to --log, when it exists), check is --check. It returns
// the exit code.
//
// Without --check nothing is added to the M1 output. With a budget log it
// recomputes every recorded budget decision, breaker change and fit from the
// inputs recorded earlier in the same file. That shows each result follows
// from its recorded inputs; it does not show the inputs were true.
func replayBudget(logPath, budgetLog string, check bool) int {
	if !check {
		return exitPass
	}
	path := budgetLog
	if path == "" {
		path = filepath.Join(filepath.Dir(logPath), "budget.jsonl")
		if _, err := os.Stat(path); err != nil {
			return exitPass // no budget log: M1 behaviour, nothing printed
		}
	}
	evs, err := budgetlog.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan replay: read budget log:", err)
		return exitError
	}
	n, drift, err := budgetlog.Replay(evs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan replay:", err)
		return exitError
	}
	if drift != "" {
		fmt.Println(drift)
		return exitFail
	}
	fmt.Printf("ok: %d budget decisions recomputed\n", n)
	return exitPass
}
