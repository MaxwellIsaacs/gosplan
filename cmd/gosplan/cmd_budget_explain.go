package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/report"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

const budgetExplainSchema = "gosplan.budget-explain/v1"

// budgetExplain is the `gosplan budget explain --json` output.
type budgetExplain struct {
	Schema       string                 `json:"schema"`
	Run          string                 `json:"run"`
	Task         string                 `json:"task"`
	Status       string                 `json:"status"`
	Admit        bool                   `json:"admit"`
	Rule         string                 `json:"rule"`
	Reason       string                 `json:"reason"`
	AdmitAt      *time.Time             `json:"admit_at"`
	NotBefore    *time.Time             `json:"not_before"`
	Pools        []budget.PoolResult    `json:"pools"`
	Estimate     *report.BudgetEstimate `json:"estimate"`
	LastDecision any                    `json:"last_decision"`
	Notes        []string               `json:"notes"`
	// unenforced: an observe-mode refusal by a rule that mode does not apply.
	unenforced bool
}

// RuleNotPending is the explain-only rule of a task that is not pending.
const ruleNotPending = "not_pending"

// cmdBudgetExplain implements `gosplan budget explain TASK [--run ID]`.
func cmdBudgetExplain(args []string) int {
	fs, c := newBudgetFlags("explain")
	runID := fs.String("run", "", "run id (default: newest run)")
	model := fs.String("model", "", "model to decide for (default: the task's, else the run's, newest attempt)")
	cfgFile := fs.String("budget-config", "", "budget config file (default: <state>/budget.toml; absent = defaults, mode observe)")
	pos, ok := parseBudgetArgs(fs, args)
	if !ok {
		return exitError
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "gosplan budget explain: exactly one TASK is required")
		return exitError
	}
	s, dir, code, done := openBudgetStore("gosplan budget explain", c)
	if done {
		return code
	}
	defer s.Close()
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "gosplan budget explain: %v\n", err)
		return exitError
	}
	ctx := context.Background()
	run := *runID
	if run == "" {
		var err error
		if run, err = s.LatestRun(ctx); err != nil {
			return fail(err)
		}
		if run == "" {
			return fail(errors.New("no runs in the state database"))
		}
	}
	task, err := s.GetTask(ctx, run, pos[0])
	if err != nil {
		return fail(fmt.Errorf("task %s in run %s: %w", pos[0], run, err))
	}
	cfg, err := loadBudgetConfig(*cfgFile, dir)
	if err != nil {
		return fail(err)
	}
	mode := cfg.Mode
	var cfgNote string
	br, brErr := state.GetBudgetRun(s.DB(), run)
	if brErr == nil && *cfgFile == "" {
		// Decide as the run's own gate does: with its recorded mode and
		// effective config (budget_runs), not whatever <state>/budget.toml
		// says now. An explicit --budget-config overrides both.
		if rc, ok := runConfig(br); ok {
			cfg, mode = rc, br.Mode
		} else {
			cfgNote = "Run " + run + "'s recorded config could not be read: the decision uses " + filepath.Join(dir, "budget.toml") + " (or defaults)."
		}
	}
	now := budgetNow()
	out := budgetExplain{Schema: budgetExplainSchema, Run: run, Task: task.ID, Status: task.Status,
		Pools: []budget.PoolResult{}, Notes: append([]string{}, report.BudgetNotes...)}
	if out.LastDecision, err = lastDecision(s.DB(), run, task.ID); err != nil {
		return fail(err)
	}
	if task.Status != core.StatusPending {
		out.Rule = ruleNotPending
		out.Reason = fmt.Sprintf("task status is %s, not pending: nothing to admit", task.Status)
	} else {
		m := *model
		if m == "" {
			m = newestModel(s.DB(), run, task.ID)
		}
		if m == "" {
			m = "unknown"
			out.Notes = append(out.Notes, "No attempt has run yet and no --model was given: the decision uses model \"unknown\", so model-specific pools are skipped.")
		}
		snap, err := state.LoadSnapshot(s.DB(), state.SnapshotRequest{
			Run: run, Task: task.ID, Attempt: task.AttemptsUsed + 1, Model: m, Mode: mode, Config: cfg, Now: now})
		if err != nil {
			return fail(err)
		}
		if cfgNote != "" {
			out.Notes = append(out.Notes, cfgNote)
		}
		if brErr != nil {
			out.Notes = append(out.Notes, "Run "+run+" has no budget_runs row (no gate ever ran for it): the decision treats it as not dispatchable.")
		}
		d := budget.Decide(snap)
		out.Admit, out.Rule, out.Reason = d.Admit, d.Rule, d.Reason
		runFlag := d.Rule == budget.RuleHalt || d.Rule == budget.RuleDeadline || d.Rule == budget.RuleCap
		out.unenforced = !d.Admit && mode != budget.ModeEnforce && !runFlag
		out.AdmitAt, out.NotBefore = d.AdmitAt, d.NotBefore
		if d.Pools != nil {
			out.Pools = d.Pools
		}
		e := report.EstimateItem(d.Estimate)
		out.Estimate = &e
		out.Notes = append(out.Notes, d.Notes...)
		out.Notes = append(out.Notes, "Explain has no in-process observation floor: a running gate may see a slightly higher level.")
	}
	if *c.json {
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(b))
		return exitPass
	}
	fmt.Print(explainText(out))
	return exitPass
}

// newestModel is the model of the task's newest attempt, else the run's.
func newestModel(db *sql.DB, run, task string) string {
	var m string
	if db.QueryRow(`SELECT model FROM attempts WHERE run_id = ? AND task_id = ? ORDER BY id DESC LIMIT 1`, run, task).Scan(&m) == nil && m != "" {
		return m
	}
	if db.QueryRow(`SELECT model FROM attempts WHERE run_id = ? ORDER BY id DESC LIMIT 1`, run).Scan(&m) == nil {
		return m
	}
	return ""
}

// lastDecision is the newest budget_decision event of the task (nil when
// none): its eid, time, and the recorded fields. It never writes.
func lastDecision(db *sql.DB, run, task string) (any, error) {
	var id int64
	var ts, data string
	err := db.QueryRow(`SELECT id, ts, data_json FROM budget_events WHERE run_id = ? AND task_id = ? AND type = 'budget_decision'
		ORDER BY id DESC LIMIT 1`, run, task).Scan(&id, &ts, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ev struct {
		Now        json.RawMessage `json:"now"`
		Mode       string          `json:"mode"`
		Enforced   bool            `json:"enforced"`
		Decision   json.RawMessage `json:"decision"`
		Suppressed int             `json:"suppressed"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, fmt.Errorf("budget decision event %d: %w", id, err)
	}
	return map[string]any{"eid": id, "ts": ts, "now": ev.Now, "mode": ev.Mode, "enforced": ev.Enforced,
		"suppressed": ev.Suppressed, "decision": ev.Decision}, nil
}

func explainText(e budgetExplain) string {
	var b []byte
	p := func(f string, a ...any) { b = fmt.Appendf(b, f, a...) }
	p("run %s task %s status %s\n", e.Run, e.Task, e.Status)
	if e.Rule != "" {
		p("rule %s: %s\n", e.Rule, e.Reason)
	}
	for _, r := range e.Pools {
		p("  %s  headroom %s at horizon %s  open reserved %s  ahead %s  hold %s  reserve %s (%s)  fits %v",
			r.Pool, pct(r.Headroom), r.HorizonBasis, pct(r.OpenReserved), pct(r.Ahead), pct(r.Hold), pct(r.Reserve), r.ReserveBasis, r.Fits)
		if r.Level != nil && !r.Documented {
			p(" (undocumented)")
		}
		if r.Stale {
			p(" (stale: %s)", r.StaleReason)
		}
		p("\n")
	}
	if e.Estimate != nil {
		tok := "unknown"
		if e.Estimate.Tokens != nil {
			tok = fmt.Sprintf("%.0f", *e.Estimate.Tokens)
		}
		p("estimate for %s: %s tokens (%s)\n", e.Estimate.Model, tok, e.Estimate.Basis)
	}
	if m, ok := e.LastDecision.(map[string]any); ok {
		p("last recorded decision: event %v at %v\n", m["eid"], m["ts"])
	} else {
		p("last recorded decision: none\n")
	}
	for _, n := range e.Notes {
		p("note: %s\n", n)
	}
	switch {
	case e.Rule == ruleNotPending:
		p("not pending: %s\n", e.Status)
	case e.unenforced:
		p("would start now (observe mode: rule %s is recorded, not enforced; enforce mode would park it)\n", e.Rule)
	case e.Admit && e.Rule != budget.RuleAdmit:
		p("would admit now (under the %s rule)\n", e.Rule)
	case e.Admit:
		p("would admit now\n")
	default:
		until := "an unknown time"
		switch {
		case e.AdmitAt != nil:
			until = e.AdmitAt.UTC().Format(time.RFC3339)
		case e.NotBefore != nil:
			until = e.NotBefore.UTC().Format(time.RFC3339)
		}
		p("parked: %s until %s\n", e.Rule, until)
	}
	return string(b)
}

// runConfig is the run's effective config as its gate recorded it in
// budget_runs.config_json; ok is false when the row has none or it does not
// decode.
func runConfig(br state.BudgetRunRow) (budget.Config, bool) {
	if br.ConfigJSON == "" {
		return budget.Config{}, false
	}
	var c budget.Config
	if err := json.Unmarshal([]byte(br.ConfigJSON), &c); err != nil {
		return budget.Config{}, false
	}
	return c, true
}
