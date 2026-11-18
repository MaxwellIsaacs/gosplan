package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// calibrateSchema is the --json schema of `gosplan budget calibrate`.
const calibrateSchema = "gosplan.budget-calibrate/v1"

// calibrateNow is the clock of the command (tests swap it).
var calibrateNow = func() time.Time { return time.Now().UTC() }

// cmdBudgetCalibrate implements `gosplan budget calibrate [--since DUR]`:
// the one-week check of whether the budget model is working. It
// reads gosplan.db read-only and every run's budget.jsonl, and does no fitting or
// arithmetic of its own: budget.Calibrate is pure.
func cmdBudgetCalibrate(args []string) int {
	fs, c := newBudgetFlags("calibrate")
	sinceDur := fs.Duration("since", 0, "only data newer than this (default: all)")
	cfgPath := fs.String("budget-config", "", "budget config file (default: the newest run's effective config, else defaults)")
	if _, ok := parseBudgetArgs(fs, args); !ok {
		return exitError
	}
	if *sinceDur < 0 {
		fmt.Fprintln(os.Stderr, "gosplan budget calibrate: --since must not be negative")
		return exitError
	}
	s, dir, code, done := openBudgetStore("gosplan budget calibrate", c)
	if done {
		return code
	}
	defer s.Close()

	now := calibrateNow()
	in, err := loadCalibrateInput(s, dir, *cfgPath, now, *sinceDur)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan budget calibrate:", err)
		return exitError
	}
	cal := budget.Calibrate(in)
	if *c.json {
		out := struct {
			Schema string `json:"schema"`
			budget.Calibration
		}{calibrateSchema, cal}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintln(os.Stderr, "gosplan budget calibrate:", err)
			return exitError
		}
		return exitPass
	}
	printCalibration(os.Stdout, cal)
	return exitPass
}

func loadCalibrateInput(s *state.Store, dir, cfgPath string, now time.Time, since time.Duration) (budget.CalibrateInput, error) {
	in := budget.CalibrateInput{Now: now}
	if since > 0 {
		in.Since = now.Add(-since)
	}
	cfg, err := calibrateConfig(s, cfgPath)
	if err != nil {
		return in, err
	}
	in.Config = cfg
	snap, err := state.LoadSnapshot(s.DB(), state.SnapshotRequest{Now: now, Mode: cfg.Mode, Config: cfg})
	if err != nil {
		return in, err
	}
	in.Obs, in.Samples = snap.Obs, snap.Samples
	pools := make([]string, 0, len(snap.Obs))
	for p := range snap.Obs {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	for _, p := range pools {
		iv := budget.Intervals(p, snap.Obs[p], snap.Spans, cfg.ModelFamilies, cfg.ResetEpsilon, now, cfg.FitLookback)
		in.Fits = append(in.Fits, budget.FitRatio(p, iv, cfg))
	}

	rows, err := s.DB().Query(`SELECT DISTINCT run_id FROM budget_reservations ORDER BY run_id`)
	if err != nil {
		return in, err
	}
	var runs []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return in, err
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return in, err
	}
	rows.Close()
	for _, r := range runs {
		rr, err := state.RunReservations(s.DB(), r)
		if err != nil {
			return in, err
		}
		for _, x := range rr {
			if x.State != "settled" || x.SettledAt == nil || x.ActualBasis == "orphan" {
				continue
			}
			sr := budget.SettledRes{ID: x.ID, Run: x.Run, Task: x.Task, Model: x.Model, SettledAt: *x.SettledAt, EstTokens: x.EstTokens, Pools: x.Pools}
			if x.ActualTokens != nil {
				a := float64(*x.ActualTokens)
				sr.ActualTokens = &a
			}
			in.Settled = append(in.Settled, sr)
		}
	}

	logs, err := filepath.Glob(filepath.Join(dir, "runs", "*", "budget.jsonl"))
	if err != nil {
		return in, err
	}
	sort.Strings(logs)
	for _, p := range logs {
		evs, err := budgetlog.ReadFile(p)
		if err != nil {
			return in, err
		}
		for _, ev := range evs {
			if ev.Type != budgetlog.TypeDecision {
				continue
			}
			var d struct {
				EID      int64           `json:"eid"`
				Now      time.Time       `json:"now"`
				Decision budget.Decision `json:"decision"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return in, fmt.Errorf("%s seq %d: %w", p, ev.Seq, err)
			}
			in.Decisions = append(in.Decisions, budget.RecordedDecision{EID: d.EID, Run: ev.Run, Task: ev.Task, Now: d.Now, Decision: d.Decision})
		}
	}
	return in, nil
}

// calibrateConfig is --budget-config, else the newest run's effective
// config, else the defaults (policy).
func calibrateConfig(s *state.Store, path string) (budget.Config, error) {
	if path != "" {
		b, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return budget.Config{}, err
		}
		cfg, _, err := budget.ParseConfig(b)
		return cfg, err
	}
	var js string
	err := s.DB().QueryRow(`SELECT config_json FROM budget_runs ORDER BY julianday(heartbeat_at) DESC LIMIT 1`).Scan(&js)
	if err != nil {
		return budget.DefaultConfig(), nil // no run has recorded a config yet
	}
	var cfg budget.Config
	if err := json.Unmarshal([]byte(js), &cfg); err != nil {
		return budget.Config{}, fmt.Errorf("newest run's config_json: %w", err)
	}
	return cfg, nil
}

func printCalibration(w *os.File, cal budget.Calibration) {
	fmt.Fprint(w, formatCalibration(cal))
}

// formatCalibration renders the calibration as text: what each number is,
// its n and basis, then the one-week check.
func formatCalibration(cal budget.Calibration) string {
	var b strings.Builder
	since := "all data"
	if cal.Since != nil {
		since = "since " + cal.Since.Format(time.RFC3339)
	}
	fmt.Fprintf(&b, "budget calibrate (%s)\n", since)
	b.WriteString("\nestimates (token estimates from finished attempts; overrun = actual tokens > estimate):\n")
	for _, e := range cal.Estimates {
		fmt.Fprintf(&b, "  %-10s n=%d p50=%.0f p90=%.0f p99=%.0f q=%.2f  overrun %d/%d = %.3f (expected %.3f)\n",
			e.Model, e.N, e.P50, e.P90, e.P99, e.Quantile, e.OverrunN, e.SettledN, e.OverrunRate, e.ExpectedOverrunRate)
	}
	if len(cal.Estimates) == 0 {
		b.WriteString("  none\n")
	}
	b.WriteString("\nfitted ratios (utilisation per million tokens, regression over observed intervals; not the plan's formula):\n")
	for _, f := range cal.Fits {
		if f.Basis == "fit" {
			fmt.Fprintf(&b, "  %-10s %.4f per Mtok  residual_rms %.4f  n=%d  basis fit\n", f.Pool, f.KPerToken*1e6, f.ResidualRMS, f.N)
		} else {
			fmt.Fprintf(&b, "  %-10s no fit (basis %s, n=%d)\n", f.Pool, f.Basis, f.N)
		}
	}
	if len(cal.Fits) == 0 {
		b.WriteString("  none\n")
	}
	b.WriteString("\nrefusals by rule:\n")
	rules := make([]string, 0, len(cal.Refusals))
	for r := range cal.Refusals {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	for _, r := range rules {
		fmt.Fprintf(&b, "  %-10s %d\n", r, cal.Refusals[r])
	}
	if len(rules) == 0 {
		b.WriteString("  none\n")
	}
	pe := cal.ProjectionError
	fmt.Fprintf(&b, "\nprojection error (predicted − observed level at reset; > 0 = over-projected): n=%d mean %+.4f mean_abs %.4f\n", pe.N, pe.Mean, pe.MeanAbs)
	b.WriteString("\nalias evidence (rows of two pools within 10 min):\n")
	for _, a := range cal.AliasEvidence {
		fmt.Fprintf(&b, "  %s ~ %s  pairs=%d mean_abs_diff %.4f\n", a.A, a.B, a.Pairs, a.MeanAbsDiff)
	}
	if len(cal.AliasEvidence) == 0 {
		b.WriteString("  none\n")
	}
	b.WriteString("\nsegments per pool:\n")
	for _, sg := range cal.Segments {
		fmt.Fprintf(&b, "  %-10s %d segment(s)", sg.Pool, sg.N)
		for _, sp := range sg.Splits {
			fmt.Fprintf(&b, "  [%s %s]", sp.At.Format("01-02T15:04Z"), sp.Rule)
		}
		b.WriteString("\n")
	}
	if len(cal.Segments) == 0 {
		b.WriteString("  none\n")
	}
	b.WriteString("\none-week check:\n")
	for _, c := range cal.Checks {
		fmt.Fprintf(&b, "  %-11s %s: %s\n", c.Status, c.Name, c.Detail)
	}
	b.WriteString("\nnotes:\n")
	for _, n := range cal.Notes {
		fmt.Fprintf(&b, "  - %s\n", n)
	}
	b.WriteString("\nIf any check is FAIL, run in observe mode until it is fixed.\n")
	return b.String()
}
