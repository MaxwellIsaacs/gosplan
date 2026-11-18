package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// cmdBudget implements `gosplan budget`: no
// subcommand (or only flags) shows the budget, `explain TASK` says why a
// task would or would not be admitted now, `calibrate` checks whether the
// budget model is working. Token and USD numbers are estimates; every
// budget.* default is policy.
func cmdBudget(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return cmdBudgetShow(args) // cmd_budget_show.go
	}
	switch args[0] {
	case "explain":
		return cmdBudgetExplain(args[1:]) // cmd_budget_explain.go
	case "calibrate":
		return cmdBudgetCalibrate(args[1:]) // cmd_budget_calibrate.go
	}
	fmt.Fprintf(os.Stderr, "gosplan budget: unknown subcommand %q\n", args[0])
	budgetUsage()
	return exitError
}

func budgetUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  gosplan budget [--project DIR | --state-dir DIR] [--json]
  gosplan budget explain TASK [--run ID] [--project DIR | --state-dir DIR] [--json]
  gosplan budget calibrate [--since DUR] [--project DIR | --state-dir DIR] [--json]`)
}

// budgetCommon holds the flags every `gosplan budget` subcommand shares.
type budgetCommon struct {
	project, stateDir *string
	json              *bool
}

// newBudgetFlags returns a flag set named "budget <sub>" with --project,
// --state-dir and --json registered.
func newBudgetFlags(sub string) (*flag.FlagSet, budgetCommon) {
	name := "budget"
	if sub != "" {
		name += " " + sub
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs, budgetCommon{
		project:  fs.String("project", "", "project directory (default: cwd); locates the default state dir"),
		stateDir: fs.String("state-dir", "", "state directory (overrides --project)"),
		json:     fs.Bool("json", false, "print JSON"),
	}
}

// parseBudgetArgs parses args with fs, allowing positional arguments before,
// between and after the flags (`explain 2.a --json`). It returns the
// positionals, or ok=false after printing the error.
func parseBudgetArgs(fs *flag.FlagSet, args []string) (pos []string, ok bool) {
	for {
		if err := fs.Parse(args); err != nil {
			return nil, false
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, true
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// openBudgetStore resolves the state dir (--state-dir, else the project's
// default) and opens gosplan.db read-only. done=true means the caller returns
// code: exit 2 when there is no state database, exit 0 after printing
// "no budget tables (schema version 1)" for a schema v1 file.
func openBudgetStore(cmd string, c budgetCommon) (s *state.Store, dir string, code int, done bool) {
	dir = *c.stateDir
	if dir == "" {
		p := *c.project
		if p == "" {
			p = "."
		}
		d, err := state.DefaultDir(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
			return nil, "", exitError, true
		}
		dir = d
	}
	db := filepath.Join(dir, "gosplan.db")
	if _, err := os.Stat(db); err != nil {
		fmt.Fprintf(os.Stderr, "%s: no state database at %s (pass --state-dir or --project)\n", cmd, db)
		return nil, "", exitError, true
	}
	s, err := state.OpenReadOnly(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return nil, "", exitError, true
	}
	if v := s.SchemaVersion(); v < 2 {
		s.Close()
		fmt.Printf("%s: no budget tables (schema version %d)\n", cmd, v)
		return nil, "", exitPass, true
	}
	return s, dir, exitPass, false
}

// notImplemented is the body of a budget surface whose owning task has not
// landed yet. Exit 2: the command could not do its job.
func notImplemented(cmd string) int {
	fmt.Fprintf(os.Stderr, "%s: not implemented yet\n", cmd)
	return exitError
}
