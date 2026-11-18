package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// budgetTestSpec writes a two-task spec; extra adds sidecar lines per task.
func budgetTestSpec(t *testing.T, extra map[string]string) *spec.Spec {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"sequence.txt": "1.a -> 1.b\n", "1.a.md": "a\n", "1.b.md": "b\n",
		"1.a.task.toml": "allow_new_protected = true\n" + extra["1.a"], "1.b.task.toml": "allow_new_protected = true\n" + extra["1.b"]}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(p, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sp, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func budgetOpt(t *testing.T, cfg string) (Options, *bytes.Buffer) {
	t.Helper()
	var stderr bytes.Buffer
	o := Options{StateDir: t.TempDir(), Stderr: &stderr}
	if cfg != "" {
		path := filepath.Join(t.TempDir(), "budget.toml")
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		o.BudgetConfig = path
	}
	return o, &stderr
}

// TestBudgetResolveNoConfigIsObserve: no config file means observe mode,
// one stderr line naming the path, and the same text in the report notes.
func TestBudgetResolveNoConfigIsObserve(t *testing.T) {
	o, stderr := budgetOpt(t, "")
	bs, err := resolveBudget(o, budgetTestSpec(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := "budget: no config at " + filepath.Join(o.StateDir, "budget.toml") + "; observe mode (decisions logged, not enforced)"
	if bs.mode != budget.ModeObserve || bs.cfg.Mode != budget.ModeObserve {
		t.Fatalf("mode: %s / %s", bs.mode, bs.cfg.Mode)
	}
	if got := stderr.String(); got != "gosplan run: "+want+"\n" {
		t.Fatalf("stderr: %q", got)
	}
	if !contains(bs.notes, want) {
		t.Fatalf("notes lack the observe notice: %v", bs.notes)
	}
	for _, n := range []string{"Token and USD numbers are estimates.", "--max-budget-usd is a client-side estimate and does not cap plan usage."} {
		if !contains(bs.notes, n) {
			t.Fatalf("notes lack %q: %v", n, bs.notes)
		}
	}
	// The state dir's budget.toml is the default path and names its mode.
	if err := os.WriteFile(filepath.Join(o.StateDir, "budget.toml"), []byte("[budget]\nmode = \"enforce\"\n[budget.cold]\nunit_fraction = 0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if bs, err = resolveBudget(o, budgetTestSpec(t, nil)); err != nil || bs.mode != budget.ModeEnforce {
		t.Fatalf("<state>/budget.toml: want enforce, got %+v %v", bs, err)
	}
	if strings.Contains(stderr.String(), "no config") {
		t.Fatalf("a config exists: no notice, got %q", stderr.String())
	}
}

// TestBudgetResolveErrors: a config without budget.mode, enforce without
// cold.unit_fraction, an unknown sidecar class, off with run flags, a bad
// --budget and a missing explicit config are errors before any task runs.
func TestBudgetResolveErrors(t *testing.T) {
	dl := time.Now().Add(time.Hour)
	usd := 1.0
	cases := []struct {
		name, cfg string
		mut       func(*Options)
		extra     map[string]string
		want      string
	}{
		{"no mode", "[budget]\njitter = \"0s\"\n", nil, nil, "budget.mode must be set"},
		{"enforce without unit_fraction", "[budget]\nmode = \"enforce\"\n", nil, nil, "unit_fraction"},
		{"--budget enforce over observe file", "[budget]\nmode = \"observe\"\n", func(o *Options) { o.Budget = "enforce" }, nil, "unit_fraction"},
		{"unknown class", "", nil, map[string]string{"1.b": "class = \"urgent\"\n"}, "unknown class in sidecar: 1.b"},
		{"off with deadline", "", func(o *Options) { o.Budget = "off"; o.Deadline = &dl }, nil, "not off"},
		{"off with cap", "", func(o *Options) { o.Budget = "off"; o.CapUSD = &usd }, nil, "not off"},
		{"bad mode", "", func(o *Options) { o.Budget = "strict" }, nil, "--budget must be"},
		{"missing explicit config", "", func(o *Options) { o.BudgetConfig = "/nonexistent/budget.toml" }, nil, "budget config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := budgetOpt(t, tc.cfg)
			if tc.mut != nil {
				tc.mut(&o)
			}
			_, err := resolveBudget(o, budgetTestSpec(t, tc.extra))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestBudgetResolvePreferIgnored: with routing off, every task with a prefer
// list gets one warning; with routing on, none.
func TestBudgetResolvePreferIgnored(t *testing.T) {
	sp := budgetTestSpec(t, map[string]string{"1.a": "prefer = [\"opus\", \"sonnet\"]\n", "1.b": "prefer = [\"sonnet\"]\n"})
	o, stderr := budgetOpt(t, "[budget]\nmode = \"observe\"\n")
	if _, err := resolveBudget(o, sp); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(stderr.String(), "prefer ignored: budget.routing.enabled is false"); n != 2 {
		t.Fatalf("want one warning per task with prefer (2), got %d:\n%s", n, stderr.String())
	}
	o, stderr = budgetOpt(t, "[budget]\nmode = \"observe\"\n[budget.routing]\nenabled = true\n")
	if _, err := resolveBudget(o, sp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "prefer ignored") {
		t.Fatalf("routing on: no warning, got %s", stderr.String())
	}
}

// TestBudgetGateOffIsNilInterface: off builds no gate, and the reconciler
// gets a nil interface (not a typed nil), so master's behaviour holds.
func TestBudgetGateOffIsNilInterface(t *testing.T) {
	o, _ := budgetOpt(t, "")
	o.Budget = "off"
	bs, err := resolveBudget(o, budgetTestSpec(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	o.budget = bs
	st, err := state.Open(filepath.Join(o.StateDir, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	g, notes, err := newBudgetGate(o, st, "run-x", t.TempDir())
	if err != nil || g != nil || notes != nil {
		t.Fatalf("off: want a nil interface and no notes, got %#v %v %v", g, notes, err)
	}
	if budgetIface(nil) != nil {
		t.Fatal("budgetIface(nil) must be a nil interface")
	}
}

// TestBudgetSidecarMaxTurns: a sidecar max_turns overrides --max-turns in
// the worker argv (from the spec in memory).
func TestBudgetSidecarMaxTurns(t *testing.T) {
	sp := budgetTestSpec(t, map[string]string{"1.b": "max_turns = 7\n"})
	m := sidecarMaxTurns(sp)
	if len(m) != 1 || m["1.b"] != 7 {
		t.Fatalf("sidecar max_turns: %v", m)
	}
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := buildConfig(Options{Claude: "claude", MaxTurns: 200, StateDir: dir}, sp, st, "run", dir, nil, nil, nil)
	for id, want := range map[string]string{"1.a": "200", "1.b": "7"} {
		req := cfg.BuildLaunch(state.TaskRow{ID: id}, core.AttemptRef{Run: "run", Task: id, Attempt: 1}, "prompt", "sid", "sonnet", core.Prepared{Dir: dir})
		if got := argAfter(req.Argv, "--max-turns"); got != want {
			t.Fatalf("%s: --max-turns %s, want %s (argv %v)", id, got, want, req.Argv)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func argAfter(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}
