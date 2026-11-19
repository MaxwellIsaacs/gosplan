// Command gosplan is the gosplan M1 tool: the verifier and the event-log
// replay check. Exit codes for `verify`: 0 pass, 1 gates failed, 2 could
// not verify (infrastructure).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/gitx"
	"github.com/MaxwellIsaacs/gosplan/internal/verify"
)

const version = "0.2.0"

const (
	exitPass  = 0
	exitFail  = 1
	exitError = 2
)

// commands maps each subcommand to its handler.
var commands = map[string]func(args []string) int{
	"verify":             cmdVerify,
	"replay":             cmdReplay,
	"version":            cmdVersion,
	"run":                cmdRun,               // cmd_run.go
	"status":             cmdStatus,            // cmd_status.go
	"keygen":             cmdKeygen,            // cmd_attest.go
	"attest":             cmdAttest,            // cmd_attest.go
	"verify-attestation": cmdVerifyAttestation, // cmd_attest.go
	"budget":             cmdBudget,            // cmd_budget.go
}

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

// dispatch runs the subcommand named by args[0] and returns the exit code.
// No or an unknown subcommand prints usage and returns exitError.
func dispatch(args []string) int {
	if len(args) < 1 {
		usage()
		return exitError
	}
	cmd, ok := commands[args[0]]
	if !ok {
		usage()
		return exitError
	}
	return cmd(args[1:])
}

func cmdVersion(args []string) int {
	fmt.Println(version)
	return exitPass
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  gosplan verify --project DIR --task ID --base SHA --head SHA [--scope GLOB ...] [--allow-new-protected] [--cache DIR] [--logs DIR] [--json]
            [--log FILE [--run ID] [--task-base SHA] [--attempt N]]
  gosplan replay --log FILE --state FILE --check [--budget-log FILE]
  gosplan run --spec DIR --project DIR [--state-dir DIR] [--max-parallel N] [--max-attempts N] [--attest-key FILE] ...
  gosplan status [--project DIR | --state-dir DIR] [--run ID] [--json]
  gosplan budget [--project DIR | --state-dir DIR] [--json]
  gosplan budget explain TASK [--run ID] [--project DIR | --state-dir DIR] [--json]
  gosplan budget calibrate [--since DUR] [--project DIR | --state-dir DIR] [--json]
  gosplan keygen [--out FILE]
  gosplan attest --verdict FILE --key FILE --out FILE --run ID --attempt N
  gosplan verify-attestation --envelope FILE --pubkey FILE [--tree SHA] [--json]
  gosplan version`)
}

// multiFlag collects repeated --scope flags.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	project := fs.String("project", ".", "project repo directory")
	task := fs.String("task", "", "task id")
	base := fs.String("base", "", "base commit, full 40-char SHA (the commit the task branched from)")
	head := fs.String("head", "", "head commit, full 40-char SHA (task branch tip)")
	cache := fs.String("cache", "", "verdict cache dir (empty disables; must not be writable by command code)")
	logs := fs.String("logs", "", "command log dir (empty discards output)")
	asJSON := fs.Bool("json", false, "print the verdict as JSON")
	allowNew := fs.Bool("allow-new-protected", false, "allow the task to add files matching protected globs (set from the task sidecar)")
	logPath := fs.String("log", "", "append gate, verdict (and launch) events to this events.jsonl (empty: no log)")
	runID := fs.String("run", "", "run id for the event envelope (default: generated); needs --log")
	taskBase := fs.String("task-base", "", "base SHA the attempt launched from; writes a launch event; needs --log")
	attempt := fs.Int("attempt", 0, "attempt number; writes a launch event; needs --log")
	var scope multiFlag
	fs.Var(&scope, "scope", "writable path glob for the task (repeatable; omit for unrestricted)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *task == "" || *base == "" || *head == "" {
		fmt.Fprintln(os.Stderr, "gosplan verify: --task, --base and --head are required")
		return exitError
	}
	if *logPath == "" && (*runID != "" || *taskBase != "" || *attempt != 0) {
		fmt.Fprintln(os.Stderr, "gosplan verify: --run, --task-base and --attempt need --log")
		return exitError
	}
	wantLaunch := *taskBase != "" || *attempt != 0
	if *taskBase != "" {
		if err := gitx.RequireFullSHA(*taskBase); err != nil {
			fmt.Fprintln(os.Stderr, "gosplan verify: --task-base:", err)
			return exitError
		}
	}
	// 0 is the "not given" default, so an explicit --attempt 0 is checked
	// by whether the flag was set.
	attemptSet := false
	fs.Visit(func(f *flag.Flag) { attemptSet = attemptSet || f.Name == "attempt" })
	if *attempt < 0 || (attemptSet && *attempt == 0) {
		fmt.Fprintln(os.Stderr, "gosplan verify: --attempt must be positive")
		return exitError
	}

	// Open the log before verifying, so a bad path fails fast (exit 2)
	// instead of after the commands have run.
	var elog *events.Log
	if *logPath != "" {
		if *runID == "" {
			*runID = newRunID(time.Now())
		}
		var err error
		if elog, err = events.Open(*logPath, *runID); err != nil {
			fmt.Fprintln(os.Stderr, "gosplan verify: open log:", err)
			return exitError
		}
		defer elog.Close()
		if wantLaunch {
			if err := appendLaunch(elog, *task, *taskBase, *attempt); err != nil {
				fmt.Fprintln(os.Stderr, "gosplan verify: write log:", err)
				return exitError
			}
		}
	}

	// nil scope (no --scope flag) means unrestricted.
	var scopeArg []string
	if len(scope) > 0 {
		scopeArg = scope
	}
	repo := gitx.Repo{Dir: *project}
	// SIGINT/SIGTERM cancel the run; deferred cleanup (export dir, HOME, and
	// command process groups) then still runs, instead of leaking.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	v, err := verify.Run(ctx, verify.Options{
		Repo: repo, Task: *task, Base: *base, Head: *head,
		Scope: scopeArg, AllowNewProtected: *allowNew,
		CacheDir: *cache, LogDir: *logs,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan verify: could not verify:", err)
		return exitError
	}
	// A verdict that could not be recorded is not reported: the orchestrator
	// reads the log, so exit 2 (retry) rather than a pass it cannot see.
	if elog != nil {
		if err := appendVerdict(elog, v); err != nil {
			fmt.Fprintln(os.Stderr, "gosplan verify: write log:", err)
			return exitError
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			fmt.Fprintln(os.Stderr, "gosplan verify: encode:", err)
			return exitError
		}
	} else {
		printHuman(v)
	}
	if v.Passed() {
		return exitPass
	}
	return exitFail
}

func printHuman(v verify.Verdict) {
	fmt.Printf("task %s  %s..%s  tree %s  cache_hit=%v\n", v.Task, short(v.BaseSHA), short(v.HeadSHA), short(v.TreeSHA), v.CacheHit)
	for _, g := range v.Gates {
		line := fmt.Sprintf("  [%-4s] %s", strings.ToUpper(string(g.Status)), g.Name)
		if g.Detail != "" {
			line += " — " + g.Detail
		}
		fmt.Println(line)
	}
	if v.Unrestricted {
		fmt.Println("  note: scope unrestricted (no --scope given)")
	}
	fmt.Printf("result: %s\n", strings.ToUpper(v.Result))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func cmdReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	logPath := fs.String("log", "", "events.jsonl path")
	statePath := fs.String("state", "", "state.json path")
	check := fs.Bool("check", false, "exit 1 if replayed statuses differ from state.json")
	budgetLog := fs.String("budget-log", "", "budget.jsonl to replay after the M1 check (default: budget.jsonl next to --log, when it exists)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *logPath == "" {
		fmt.Fprintln(os.Stderr, "gosplan replay: --log is required")
		return exitError
	}
	evs, err := events.ReadFile(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan replay: read log:", err)
		return exitError
	}
	replayed, err := events.TaskStatuses(evs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan replay:", err)
		return exitError
	}
	if !*check {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(replayed)
		return replayBudget(*logPath, *budgetLog, *check) // cmd_replay_budget.go
	}
	if *statePath == "" {
		fmt.Fprintln(os.Stderr, "gosplan replay: --state is required with --check")
		return exitError
	}
	b, err := os.ReadFile(filepath.Clean(*statePath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan replay: read state:", err)
		return exitError
	}
	recorded := map[string]string{}
	if err := json.Unmarshal(b, &recorded); err != nil {
		fmt.Fprintln(os.Stderr, "gosplan replay: parse state (expects flat task->status object):", err)
		return exitError
	}
	if d := events.Diff(replayed, recorded); d != "" {
		fmt.Println("mismatch:", d)
		return exitFail
	}
	fmt.Println("ok: replayed statuses match state")
	return replayBudget(*logPath, *budgetLog, *check) // cmd_replay_budget.go
}
