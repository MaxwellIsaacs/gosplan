package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetrt"
	"github.com/MaxwellIsaacs/gosplan/internal/driver"
	"github.com/MaxwellIsaacs/gosplan/internal/reconcile"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/window"
)

// Budget notes for the report.
var budgetClaimNotes = []string{
	"Token and USD numbers are estimates.",
	"Window utilisation from unifiedWindows, rateLimitType and /usage text is undocumented.",
	"--max-budget-usd is a client-side estimate and does not cap plan usage.",
	"Admission reduces rate-limit stalls; it does not prevent them.",
	"Class fractions and budget defaults are policy, not measurement.",
	"Headroom is headroom at the horizon (the reset only when a decision's horizon_basis is reset).",
	"gosplan replay --check proves a budget decision follows from its recorded inputs, not that the inputs were true.",
}

// budgetSetup is the budget config resolved once at run start: it is never re-read mid-run.
type budgetSetup struct {
	mode     string
	cfg      budget.Config
	defaults []string
	path     string
	notes    []string
}

// resolveBudget resolves the mode and reads the config once: --budget, else
// budget.mode of the config file (which must name it), else observe when no
// file exists (with a stderr notice and a report note). It validates the
// config for the mode, the run flags against off, and the classes the
// spec's sidecars name; it warns once per task whose prefer list is ignored.
func resolveBudget(opt Options, sp *spec.Spec) (*budgetSetup, error) {
	switch opt.Budget {
	case "", budget.ModeOff, budget.ModeObserve, budget.ModeEnforce:
	default:
		return nil, fmt.Errorf("--budget must be off, observe or enforce, got %q", opt.Budget)
	}
	path := opt.BudgetConfig
	explicit := path != ""
	if !explicit {
		path = filepath.Join(opt.StateDir, "budget.toml")
	}
	b, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && (explicit || !errors.Is(err, os.ErrNotExist)) {
		return nil, fmt.Errorf("budget config: %w", err)
	}
	bs := &budgetSetup{path: path}
	if exists {
		if bs.cfg, bs.defaults, err = budget.ParseConfig(b); err != nil {
			return nil, err
		}
		bs.mode = bs.cfg.Mode
	} else {
		bs.cfg = budget.DefaultConfig()
		bs.defaults = append([]string(nil), bs.cfg.Defaults...)
		bs.mode = budget.ModeObserve
	}
	if opt.Budget != "" {
		bs.mode = opt.Budget
	}
	if bs.mode == budget.ModeOff {
		if opt.Deadline != nil || opt.CapUSD != nil || len(opt.CapWindow) > 0 {
			return nil, errors.New("--deadline, --cap-window and --cap-usd-estimate need the budget gate (--budget observe or enforce), not off")
		}
		return bs, nil
	}
	if err := bs.cfg.Validate(bs.mode); err != nil {
		return nil, err
	}
	bs.cfg.Mode = bs.mode
	if !exists && opt.Budget == "" {
		msg := fmt.Sprintf("budget: no config at %s; observe mode (decisions logged, not enforced)", path)
		fmt.Fprintf(opt.Stderr, "gosplan run: %s\n", msg)
		bs.notes = append(bs.notes, msg)
	} else {
		bs.notes = append(bs.notes, fmt.Sprintf("budget: mode %s (config %s)", bs.mode, configLabel(exists, path)))
	}
	for p, f := range opt.CapWindow {
		if !(f > 0 && f <= 1) {
			return nil, fmt.Errorf("--cap-window %s=%v: the fraction must be in (0,1]", p, f)
		}
	}
	var unknown []string
	for _, t := range sp.Tasks {
		class := t.Class
		if class == "" {
			class = spec.DefaultClass
		}
		if _, ok := bs.cfg.ClassRank(class); !ok {
			unknown = append(unknown, fmt.Sprintf("%s (class %q)", t.ID, class))
		}
		if len(t.Prefer) > 0 && !bs.cfg.RoutingEnabled {
			fmt.Fprintf(opt.Stderr, "gosplan run: warning: task %s: prefer ignored: budget.routing.enabled is false\n", t.ID)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("budget: unknown class in sidecar: %s (classes come from budget.class.* in %s)", strings.Join(unknown, ", "), path)
	}
	if len(bs.defaults) > 0 {
		bs.notes = append(bs.notes, "Budget policy defaults in effect: "+strings.Join(bs.defaults, ", ")+".")
	}
	bs.notes = append(bs.notes, budgetClaimNotes...)
	return bs, nil
}

func configLabel(exists bool, path string) string {
	if exists {
		return path
	}
	return "none"
}

// newBudgetGate builds the run's gate from the setup resolveBudget made
// (opt.budget). off gives a nil gate (and no notes): no budget code runs.
// The second result is the report notes.
func newBudgetGate(opt Options, store *state.Store, run, runDir string) (reconcile.Budget, []string, error) {
	bs := opt.budget
	if bs == nil || bs.mode == budget.ModeOff {
		return nil, nil, nil
	}
	var prober *window.Prober
	if opt.UsageProbe {
		prober = &window.Prober{ClaudeBin: opt.Claude, Env: filterEnv(os.Environ()), Now: store.Now,
			MaxAge: bs.cfg.StaleAfter, MinInterval: bs.cfg.ProbeMinInterval}
	}
	tasks := map[string]budgetrt.TaskMeta{}
	if opt.spec != nil {
		for _, t := range opt.spec.Tasks {
			tasks[t.ID] = budgetrt.TaskMeta{Class: t.Class, Prefer: t.Prefer, MaxTurns: t.MaxTurns}
		}
	}
	g, err := budgetrt.New(store, budgetrt.Options{
		Run: run, StateDir: opt.StateDir, RunDir: runDir,
		Mode: bs.mode, Config: bs.cfg, ConfigSHA: bs.cfg.SHA256(), DefaultsUsed: bs.defaults,
		MaxParallel: opt.MaxParallel, Deadline: opt.Deadline, CapUSD: opt.CapUSD, CapWindow: opt.CapWindow,
		Owner: driver.LeaseOwner(), Prober: prober, QuietMax: opt.StallQuietMax, Now: store.Now, Stderr: opt.Stderr,
		Tasks: tasks, MaxTurns: opt.MaxTurns, Model: opt.Model,
	})
	if err != nil {
		return nil, nil, err
	}
	return budgetIface(g), append([]string(nil), bs.notes...), nil
}

// budgetIface is g as a reconcile.Budget: a nil interface (not a typed nil)
// when there is no gate, so the reconciler keeps master's behaviour.
func budgetIface(g *budgetrt.Gate) reconcile.Budget {
	if g == nil {
		return nil
	}
	return g
}

// sidecarMaxTurns is the per-task max_turns override from the spec in memory.
func sidecarMaxTurns(sp *spec.Spec) map[string]int {
	out := map[string]int{}
	if sp == nil {
		return out
	}
	for _, t := range sp.Tasks {
		if t.MaxTurns > 0 {
			out[t.ID] = t.MaxTurns
		}
	}
	return out
}
