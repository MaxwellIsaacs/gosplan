package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/report"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// cmdStatus implements `gosplan status`: it prints the run report from the SQLite
// state, opened read-only. Only --write-report writes, and only report.json.
func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	project := fs.String("project", "", "project directory (default: cwd); locates the default state dir")
	stateDir := fs.String("state-dir", "", "state directory (overrides --project)")
	runID := fs.String("run", "", "run id (default: newest run)")
	asJSON := fs.Bool("json", false, "print the run report as JSON")
	write := fs.Bool("write-report", false, "also write <state>/runs/<run>/report.json")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	dir := *stateDir
	if dir == "" {
		p := *project
		if p == "" {
			p = "."
		}
		d, err := state.DefaultDir(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gosplan status: %v\n", err)
			return exitError
		}
		dir = d
	}
	db := filepath.Join(dir, "gosplan.db")
	if _, err := os.Stat(db); err != nil {
		fmt.Fprintf(os.Stderr, "gosplan status: no state database at %s (pass --state-dir or --project)\n", db)
		return exitError
	}
	s, err := state.OpenReadOnly(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gosplan status: %v\n", err)
		return exitError
	}
	defer s.Close()
	ctx := context.Background()
	run := *runID
	if run == "" {
		if run, err = s.LatestRun(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "gosplan status: %v\n", err)
			return exitError
		}
		if run == "" {
			fmt.Fprintf(os.Stderr, "gosplan status: no runs in %s\n", db)
			return exitError
		}
	}
	now := time.Now()
	r, err := report.Build(ctx, s, run, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gosplan status: %v\n", err)
		return exitError
	}
	if *write {
		if err := report.Write(filepath.Join(dir, "runs", run, "report.json"), r); err != nil {
			fmt.Fprintf(os.Stderr, "gosplan status: write report: %v\n", err)
			return exitError
		}
	}
	if *asJSON {
		b, err := report.JSON(r)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gosplan status: %v\n", err)
			return exitError
		}
		os.Stdout.Write(b)
		return exitPass
	}
	fmt.Print(report.Text(r, now))
	return exitPass
}
