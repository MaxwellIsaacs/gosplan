package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/report"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// budgetNow is the clock of the budget surfaces; tests replace it.
var budgetNow = time.Now

// budgetShowSchema is the schema id of `gosplan budget --json`.
const budgetShowSchema = "gosplan.budget/v1"

// budgetView is the `gosplan budget --json` output.
type budgetView struct {
	Schema         string                     `json:"schema"`
	Mode           string                     `json:"mode"`
	ConfigSHA256   string                     `json:"config_sha256"`
	PolicyDefaults []string                   `json:"policy_defaults"`
	Now            string                     `json:"now"`
	Pools          []report.BudgetPool        `json:"pools"`
	Reservations   []report.BudgetReservation `json:"reservations"`
	Breakers       []report.BudgetBreaker     `json:"breakers"`
	Estimates      []report.BudgetEstimate    `json:"estimates"`
	Fits           []report.BudgetFit         `json:"fits"`
	Notes          []string                   `json:"notes"`
}

// loadBudgetConfig reads the budget config for a surface: file when given
// (an error if it is unreadable), else <stateDir>/budget.toml when it
// exists, else the defaults in mode observe. The config is validated for
// its own mode.
func loadBudgetConfig(file, stateDir string) (budget.Config, error) {
	explicit := file != ""
	if !explicit {
		file = filepath.Join(stateDir, "budget.toml")
	}
	b, err := os.ReadFile(file)
	var cfg budget.Config
	switch {
	case err == nil:
		if cfg, _, err = budget.ParseConfig(b); err != nil {
			return cfg, err
		}
	case os.IsNotExist(err) && !explicit:
		cfg = budget.DefaultConfig()
	default:
		return cfg, err
	}
	if err := cfg.Validate(cfg.Mode); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// cmdBudgetShow implements `gosplan budget [--json]`.
func cmdBudgetShow(args []string) int {
	fs, c := newBudgetFlags("")
	cfgFile := fs.String("budget-config", "", "budget config file (default: <state>/budget.toml; absent = defaults, mode observe)")
	if _, ok := parseBudgetArgs(fs, args); !ok {
		return exitError
	}
	s, dir, code, done := openBudgetStore("gosplan budget", c)
	if done {
		return code
	}
	defer s.Close()
	cfg, err := loadBudgetConfig(*cfgFile, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gosplan budget: %v\n", err)
		return exitError
	}
	now := budgetNow()
	snap, err := state.LoadSnapshot(s.DB(), state.SnapshotRequest{Mode: cfg.Mode, Config: cfg, Now: now})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gosplan budget: %v\n", err)
		return exitError
	}
	v := budgetView{
		Schema: budgetShowSchema, Mode: cfg.Mode, ConfigSHA256: snap.ConfigSHA256,
		PolicyDefaults: append([]string{}, cfg.Defaults...), Now: now.UTC().Format(time.RFC3339),
		Pools: report.BudgetPools(snap), Estimates: report.BudgetEstimates(snap), Fits: report.BudgetFits(snap),
		Notes: append(append([]string{}, report.BudgetNotes...),
			"Window data from /usage text can be up to 60 minutes old."),
	}
	if v.Reservations, err = report.OpenReservationItems(s.DB(), now); err == nil {
		v.Breakers, err = report.BudgetBreakers(s.DB(), now)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gosplan budget: %v\n", err)
		return exitError
	}
	if *c.json {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "gosplan budget: %v\n", err)
			return exitError
		}
		fmt.Println(string(b))
		return exitPass
	}
	fmt.Print(budgetShowText(v))
	return exitPass
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func durText(sec float64) string {
	return time.Duration(sec * float64(time.Second)).Round(time.Second).String()
}

// budgetShowText renders v. One line per pool; the last line names the
// policy defaults in effect.
func budgetShowText(v budgetView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "budget mode %s  config %s  now %s\n", v.Mode, short12(v.ConfigSHA256), v.Now)
	b.WriteString("pools:\n")
	if len(v.Pools) == 0 {
		b.WriteString("  (no window observations)\n")
	}
	for _, p := range v.Pools {
		level := "-"
		if p.Level != nil {
			level = pct(*p.Level)
		}
		fmt.Fprintf(&b, "  %s  level %s  rate %.1f%%/h (%s)  projected %s  headroom %s at horizon %s (%s)  age %s  source %s",
			p.Pool, level, p.RatePerHour*100, p.RateBasis, pct(p.Projected), pct(p.Headroom), p.HorizonBasis,
			durText(p.HorizonSeconds), durText(p.AgeSeconds), p.Source)
		if p.Level != nil && !p.Documented {
			b.WriteString(" (undocumented)")
		}
		if p.Stale {
			fmt.Fprintf(&b, " (stale: %s)", p.StaleReason)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "open reservations: %d\n", len(v.Reservations))
	for _, r := range v.Reservations {
		est := "unknown"
		if r.EstTokens != nil {
			est = fmt.Sprintf("%.0f tokens", *r.EstTokens)
		}
		var ps []string
		for _, p := range r.Pools {
			ps = append(ps, fmt.Sprintf("%s %s (%s)", p.Pool, pct(p.Reserve), p.Basis))
		}
		fmt.Fprintf(&b, "  #%d %s/%s attempt %d %s %s  estimate %s (%s)  reserve %s  age %s",
			r.ID, r.Run, r.Task, r.Attempt, r.Model, r.Class, est, r.EstBasis, joinDash(ps), durText(r.AgeSeconds))
		if !r.Enforced {
			b.WriteString("  (observe only)")
		}
		b.WriteString("\n")
	}
	open := 0
	for _, br := range v.Breakers {
		if br.State == "open" {
			open++
		}
	}
	fmt.Fprintf(&b, "breakers: %d open (closed ones: last 24h)\n", open)
	for _, br := range v.Breakers {
		fmt.Fprintf(&b, "  %s %s %s %s  %s", br.Kind, br.Scope, br.Run, br.Task, br.State)
		if br.Reason != "" {
			fmt.Fprintf(&b, "  %s", br.Reason)
		}
		if br.ClosesAt != nil {
			fmt.Fprintf(&b, "  closes %s", *br.ClosesAt)
		}
		if !br.Enforced {
			b.WriteString("  (observe only)")
		}
		b.WriteString("\n")
	}
	b.WriteString("estimates (token numbers are estimates):\n")
	for _, e := range v.Estimates {
		tok := "unknown"
		if e.Tokens != nil {
			tok = fmt.Sprintf("%.0f", *e.Tokens)
		}
		fmt.Fprintf(&b, "  %s  estimate %s tokens at p%.0f (%s, n=%d)", e.Model, tok, e.Quantile*100, e.Basis, e.N)
		if e.N > 0 && math.Ceil(e.Quantile*float64(e.N)-1e-9) >= float64(e.N) && strings.HasPrefix(e.Basis, "history") {
			b.WriteString(" (= sample max)")
		}
		b.WriteString("\n")
	}
	b.WriteString("fits (a fitted ratio is not the plan's formula):\n")
	for _, f := range v.Fits {
		fmt.Fprintf(&b, "  %s  basis %s  k %.3g%%/Mtok  se_k %.3g%%/Mtok  n %d\n", f.Pool, f.Basis, f.KPerMTok*100, f.SEKPerMTok*100, f.N)
	}
	b.WriteString("note: /usage data can be up to 60 min old\n")
	for _, n := range report.BudgetNotes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	fmt.Fprintf(&b, "policy defaults in effect: %s\n", joinDash(v.PolicyDefaults, ", "))
	return b.String()
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// joinDash joins xs with sep (default " "), "-" when empty.
func joinDash(xs []string, sep ...string) string {
	if len(xs) == 0 {
		return "-"
	}
	s := " "
	if len(sep) > 0 {
		s = sep[0]
	}
	return strings.Join(xs, s)
}
