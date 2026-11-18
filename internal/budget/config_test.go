package budget

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const budgetMinimalObserve = "[budget]\nmode = \"observe\"\n"

func budgetMustParse(t *testing.T, src string) (Config, []string) {
	t.Helper()
	c, defs, err := ParseConfig([]byte(src))
	if err != nil {
		t.Fatalf("ParseConfig(%q): %v", src, err)
	}
	return c, defs
}

// TestBudgetConfigDefaults: every default equals the documented table, one
// assertion per key, both for DefaultConfig and for a file naming only mode.
func TestBudgetConfigDefaults(t *testing.T) {
	parsed, _ := budgetMustParse(t, budgetMinimalObserve)
	for name, c := range map[string]Config{"DefaultConfig": DefaultConfig(), "ParseConfig(mode only)": parsed} {
		t.Run(name, func(t *testing.T) {
			eqD := func(key string, got, want time.Duration) {
				if got != want {
					t.Errorf("%s: got %s, want %s", key, got, want)
				}
			}
			eqF := func(key string, got, want float64) {
				if got != want {
					t.Errorf("%s: got %v, want %v", key, got, want)
				}
			}
			eqI := func(key string, got, want int) {
				if got != want {
					t.Errorf("%s: got %d, want %d", key, got, want)
				}
			}
			if c.Mode != ModeObserve {
				t.Errorf("mode: got %q, want observe", c.Mode)
			}
			eqD("stale_after", c.StaleAfter, 20*time.Minute)
			eqD("probe_min_interval", c.ProbeMinInterval, 15*time.Minute)
			eqD("lookback", c.Lookback, 2*time.Hour)
			eqD("max_horizon", c.MaxHorizon, 2*time.Hour)
			eqF("reset_epsilon", c.ResetEpsilon, 0.01)
			eqD("jitter", c.Jitter, 30*time.Second)
			eqD("recheck_max", c.RecheckMax, 5*time.Minute)
			eqD("stale_recheck", c.StaleRecheck, time.Minute)
			eqD("fairness_recheck", c.FairnessRecheck, 5*time.Second)
			eqD("run_ttl", c.RunTTL, 60*time.Second)
			eqI("stale_max_open", c.StaleMaxOpen, 1)
			if !reflect.DeepEqual(c.ModelFamilies, []string{"opus", "sonnet", "haiku", "fable"}) {
				t.Errorf("model_families: got %v", c.ModelFamilies)
			}
			if len(c.Alias) != 0 {
				t.Errorf("alias: want none, got %v", c.Alias)
			}
			eqF("estimate.quantile", c.EstimateQuantile, 0.9)
			eqI("estimate.min_samples", c.EstimateMinSamples, 5)
			eqI("estimate.window", c.EstimateWindow, 200)
			eqF("estimate.cold_tokens", c.EstimateColdTokens, 0)
			eqF("estimate.safety_factor", c.EstimateSafetyFactor, 1.5)
			eqD("estimate.cold_duration", c.EstimateColdDuration, 0)
			eqD("fit.lookback", c.FitLookback, 168*time.Hour)
			eqI("fit.min_points", c.FitMinPoints, 8)
			eqF("fit.max_rel_se", c.FitMaxRelSE, 0.5)
			eqD("fit.max_interval", c.FitMaxInterval, 30*time.Minute)
			eqF("cold.unit_fraction (observe)", c.ColdUnitFraction, 0.05)
			if got := c.Classes[ClassCritical]; got != (Class{Rank: 3, ReserveFraction: 0.10}) {
				t.Errorf("class.critical: got %+v", got)
			}
			if got := c.Classes[ClassNormal]; got != (Class{Rank: 2, ReserveFraction: 0.15}) {
				t.Errorf("class.normal: got %+v", got)
			}
			if got := c.Classes[ClassBestEffort]; got != (Class{Rank: 1, ReserveFraction: 0}) {
				t.Errorf("class.best_effort: got %+v", got)
			}
			if len(c.Classes) != 3 {
				t.Errorf("classes: want 3 defaults, got %v", c.Classes)
			}
			if c.BeltUSDFactor != nil {
				t.Errorf("belt.usd_factor: want unset, got %v", *c.BeltUSDFactor)
			}
			eqI("breaker.retry_storm.retries", c.BreakerRetryStormRetries, 6)
			eqD("breaker.retry_storm.window", c.BreakerRetryStormWindow, 15*time.Minute)
			eqD("breaker.retry_storm.cooldown", c.BreakerRetryStormCooldown, 15*time.Minute)
			eqI("breaker.same_failure.attempts", c.BreakerSameFailureAttempts, 3)
			eqD("breaker.rate_limited.cooldown_base", c.BreakerRateLimitedCooldownBase, 5*time.Minute)
			eqD("breaker.rate_limited.cooldown_max", c.BreakerRateLimitedCooldownMax, time.Hour)
			if c.RoutingEnabled {
				t.Errorf("routing.enabled: want false")
			}
			if err := c.Validate(ModeObserve); err != nil {
				t.Errorf("defaults do not validate in observe: %v", err)
			}
		})
	}
}

// TestBudgetConfigModeRequired: a file without budget.mode is an error.
func TestBudgetConfigModeRequired(t *testing.T) {
	for _, src := range []string{"", "[budget]\nstale_after = \"5m\"\n", "[budget.estimate]\nquantile = 0.8\n"} {
		_, _, err := ParseConfig([]byte(src))
		if err == nil || !strings.Contains(err.Error(), "budget.mode must be set") {
			t.Errorf("ParseConfig(%q): want 'budget.mode must be set', got %v", src, err)
		}
	}
	if _, _, err := ParseConfig([]byte("[budget]\nmode = \"strict\"\n")); err == nil {
		t.Error("ParseConfig accepted mode = strict")
	}
}

// TestBudgetConfigUnitFraction: enforce needs cold.unit_fraction typed (in
// the file, or for a resolved mode of enforce); observe defaults it to 0.05.
func TestBudgetConfigUnitFraction(t *testing.T) {
	if _, _, err := ParseConfig([]byte("[budget]\nmode = \"enforce\"\n")); err == nil || !strings.Contains(err.Error(), "unit_fraction") {
		t.Errorf("enforce without cold.unit_fraction: want error naming unit_fraction, got %v", err)
	}
	c, defs := budgetMustParse(t, budgetMinimalObserve)
	if c.ColdUnitFraction != 0.05 {
		t.Errorf("observe without cold.unit_fraction: got %v, want 0.05", c.ColdUnitFraction)
	}
	if !budgetHas(defs, "budget.cold.unit_fraction") {
		t.Errorf("defaults list lacks budget.cold.unit_fraction: %v", defs)
	}
	// --budget enforce over an observe file that never typed it: refused.
	if err := c.Validate(ModeEnforce); err == nil || !strings.Contains(err.Error(), "unit_fraction") {
		t.Errorf("Validate(enforce) on a defaulted unit_fraction: want error, got %v", err)
	}
	if err := DefaultConfig().Validate(ModeEnforce); err == nil {
		t.Error("Validate(enforce) on DefaultConfig (no file): want error")
	}
	e, defs := budgetMustParse(t, "[budget]\nmode = \"enforce\"\n[budget.cold]\nunit_fraction = 0.05\n")
	if e.ColdUnitFraction != 0.05 || budgetHas(defs, "budget.cold.unit_fraction") {
		t.Errorf("typed unit_fraction: got %v, defaults %v", e.ColdUnitFraction, defs)
	}
	if err := e.Validate(ModeEnforce); err != nil {
		t.Errorf("Validate(enforce) with a typed unit_fraction: %v", err)
	}
}

// TestBudgetConfigBeltUnset: no belt.usd_factor means no belt (nil).
func TestBudgetConfigBeltUnset(t *testing.T) {
	c, defs := budgetMustParse(t, budgetMinimalObserve)
	if c.BeltUSDFactor != nil {
		t.Fatalf("belt.usd_factor: want nil, got %v", *c.BeltUSDFactor)
	}
	if budgetHas(defs, "budget.belt.usd_factor") {
		t.Errorf("belt.usd_factor has no default and must not be listed as one: %v", defs)
	}
	c, _ = budgetMustParse(t, budgetMinimalObserve+"[budget.belt]\nusd_factor = 2\n")
	if c.BeltUSDFactor == nil || *c.BeltUSDFactor != 2 {
		t.Fatalf("belt.usd_factor = 2: got %v", c.BeltUSDFactor)
	}
}

// TestBudgetConfigUnknownKeys: every unknown key is an error, including the
// key that does not exist (breaker.verifier_error.*) and a misspelling.
func TestBudgetConfigUnknownKeys(t *testing.T) {
	for _, extra := range []string{
		"[budget.breaker.verifier_error]\nconsecutive = 3\n",
		"stale_afer = \"5m\"\n",
		"[budget.estimate]\nquantil = 0.8\n",
		"[budget.breaker.same_failure]\nconsecutive = 3\n",
		"[budget.class.normal]\nrnak = 2\n",
		"[other]\nx = 1\n",
	} {
		_, _, err := ParseConfig([]byte(budgetMinimalObserve + extra))
		if err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("ParseConfig(+%q): want unknown key error, got %v", extra, err)
		}
	}
	// The real breaker key parses.
	c, _ := budgetMustParse(t, budgetMinimalObserve+"[budget.breaker.same_failure]\nattempts = 4\n")
	if c.BreakerSameFailureAttempts != 4 {
		t.Errorf("breaker.same_failure.attempts: got %d", c.BreakerSameFailureAttempts)
	}
}

// TestBudgetConfigTypeErrors: a value of the wrong type is an error, and a
// duration must parse with time.ParseDuration.
func TestBudgetConfigTypeErrors(t *testing.T) {
	for _, extra := range []string{
		"stale_after = 20\n",
		"stale_after = \"twenty minutes\"\n",
		"stale_max_open = 1.5\n",
		"model_families = \"opus\"\n",
		"[budget.routing]\nenabled = \"yes\"\n",
		"[budget.class.vip]\nrank = 9\n", // new class without reserve_fraction
	} {
		if _, _, err := ParseConfig([]byte(budgetMinimalObserve + extra)); err == nil {
			t.Errorf("ParseConfig(+%q): want an error", extra)
		}
	}
}

// TestBudgetConfigValidation: each documented validation rule rejects a violation.
func TestBudgetConfigValidation(t *testing.T) {
	cases := []struct {
		name, extra, want string
	}{
		{"negative duration", "jitter = \"-1s\"\n", "budget.jitter"},
		{"stale_after zero", "stale_after = \"0s\"\n", "budget.stale_after"},
		{"quantile 0", "[budget.estimate]\nquantile = 0.0\n", "quantile"},
		{"quantile 1", "[budget.estimate]\nquantile = 1.0\n", "quantile"},
		{"unit_fraction 0", "[budget.cold]\nunit_fraction = 0.0\n", "unit_fraction"},
		{"unit_fraction > 1", "[budget.cold]\nunit_fraction = 1.5\n", "unit_fraction"},
		{"usd_factor 0", "[budget.belt]\nusd_factor = 0.0\n", "usd_factor"},
		{"max_horizon > lookback", "max_horizon = \"3h\"\n", "max_horizon"},
		{"max_rel_se 0", "[budget.fit]\nmax_rel_se = 0.0\n", "max_rel_se"},
		{"reserve_fraction 1", "[budget.class.critical]\nreserve_fraction = 1.0\n", "reserve_fraction"},
		{"reserve_fraction < 0", "[budget.class.best_effort]\nreserve_fraction = -0.1\n", "reserve_fraction"},
		{"sum above lowest >= 1", "[budget.class.critical]\nreserve_fraction = 0.5\n[budget.class.normal]\nreserve_fraction = 0.5\n", "sum"},
		{"ranks not distinct", "[budget.class.normal]\nrank = 3\n", "distinct"},
		{"reset_epsilon 0.5", "reset_epsilon = 0.5\n", "reset_epsilon"},
		{"reset_epsilon < 0", "reset_epsilon = -0.01\n", "reset_epsilon"},
		{"negative int", "stale_max_open = -1\n", "stale_max_open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := budgetMustParse(t, budgetMinimalObserve+tc.extra)
			err := c.Validate(ModeObserve)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate: want error containing %q, got %v", tc.want, err)
			}
		})
	}
	// The lowest class is excluded from the sum: best_effort may hold 0.9.
	c, _ := budgetMustParse(t, budgetMinimalObserve+"[budget.class.best_effort]\nreserve_fraction = 0.9\n")
	if err := c.Validate(ModeObserve); err != nil {
		t.Errorf("lowest class excluded from the sum: %v", err)
	}
	if err := DefaultConfig().Validate("strict"); err == nil {
		t.Error("Validate accepted resolved mode strict")
	}
}

// TestBudgetConfigMaxHorizonFollowsLookback: max_horizon defaults to the
// (possibly set) lookback.
func TestBudgetConfigMaxHorizonFollowsLookback(t *testing.T) {
	c, defs := budgetMustParse(t, budgetMinimalObserve+"lookback = \"1h\"\n")
	if c.MaxHorizon != time.Hour {
		t.Fatalf("max_horizon: got %s, want = lookback 1h", c.MaxHorizon)
	}
	if !budgetHas(defs, "budget.max_horizon") || budgetHas(defs, "budget.lookback") {
		t.Fatalf("defaults: %v", defs)
	}
}

// TestBudgetConfigSHA256: stable across parses of one file, and changes when
// any key changes.
func TestBudgetConfigSHA256(t *testing.T) {
	// mk builds a file: top-level [budget] keys first, then the alias table,
	// then more tables (TOML puts bare keys into the last table opened).
	mk := func(top, tables string) string {
		return budgetMinimalObserve + top + "[budget.alias]\nsession = \"five_hour\"\n" + tables
	}
	src := mk("", "")
	a, _ := budgetMustParse(t, src)
	b, _ := budgetMustParse(t, src)
	if a.SHA256() != b.SHA256() || len(a.SHA256()) != 64 {
		t.Fatalf("sha256 not stable: %s vs %s", a.SHA256(), b.SHA256())
	}
	if string(a.Canonical()) != string(b.Canonical()) {
		t.Fatal("canonical JSON not stable")
	}
	seen := map[string]string{a.SHA256(): "base"}
	variants := map[string]string{
		"mode":                  "[budget]\nmode = \"enforce\"\n[budget.cold]\nunit_fraction = 0.05\n[budget.alias]\nsession = \"five_hour\"\n",
		"alias":                 budgetMinimalObserve + "[budget.alias]\nsession = \"seven_day\"\n",
		"stale_after":           mk("stale_after = \"21m\"\n", ""),
		"estimate.window":       mk("", "[budget.estimate]\nwindow = 201\n"),
		"fit.max_interval":      mk("", "[budget.fit]\nmax_interval = \"31m\"\n"),
		"cold.unit_fraction":    mk("", "[budget.cold]\nunit_fraction = 0.06\n"),
		"class.normal.rank":     mk("", "[budget.class.normal]\nrank = 4\n"),
		"belt.usd_factor":       mk("", "[budget.belt]\nusd_factor = 2.0\n"),
		"breaker.retry_storm":   mk("", "[budget.breaker.retry_storm]\nretries = 7\n"),
		"routing.enabled":       mk("", "[budget.routing]\nenabled = true\n"),
		"model_families":        mk("model_families = [\"opus\"]\n", ""),
		"rate_limited.cooldown": mk("", "[budget.breaker.rate_limited]\ncooldown_max = \"2h\"\n"),
	}
	for name, v := range variants {
		c, _ := budgetMustParse(t, v)
		h := c.SHA256()
		if prev, dup := seen[h]; dup {
			t.Errorf("changing %s gives the same sha256 as %s", name, prev)
		}
		seen[h] = name
	}
	// Writing a key at its default value is the same effective config.
	d, _ := budgetMustParse(t, mk("stale_after = \"20m\"\n", ""))
	if d.SHA256() != a.SHA256() {
		t.Error("an explicit default changed the sha256 of the effective config")
	}
}

// TestBudgetConfigJSONRoundTrip: durations are Go duration strings, keys
// are the TOML paths with '_', and Unmarshal(Canonical) restores the config.
func TestBudgetConfigJSONRoundTrip(t *testing.T) {
	c, _ := budgetMustParse(t, budgetMinimalObserve+"[budget.belt]\nusd_factor = 1.5\n[budget.class.vip]\nrank = 9\nreserve_fraction = 0.05\n[budget.alias]\nsession = \"five_hour\"\n")
	var m map[string]any
	if err := json.Unmarshal(c.Canonical(), &m); err != nil {
		t.Fatal(err)
	}
	if m["stale_after"] != "20m0s" || m["estimate_min_samples"] != float64(5) || m["breaker_same_failure_attempts"] != float64(3) {
		t.Fatalf("canonical JSON fields: %v", m)
	}
	var back Config
	if err := json.Unmarshal(c.Canonical(), &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Canonical()) != string(c.Canonical()) {
		t.Fatalf("round trip changed the config:\n%s\n%s", c.Canonical(), back.Canonical())
	}
	if r, ok := back.ClassRank("vip"); !ok || r != 9 {
		t.Fatalf("ClassRank(vip) after round trip: %d %v", r, ok)
	}
	if _, ok := back.ClassRank("nope"); ok {
		t.Fatal("ClassRank(nope): want false")
	}
	if b, _ := json.Marshal(DefaultConfig()); !strings.Contains(string(b), `"belt_usd_factor":null`) {
		t.Fatalf("unset belt must encode as null: %s", b)
	}
}

// TestBudgetConfigDefaultsList: the list ParseConfig returns names exactly
// the keys the file left untouched.
func TestBudgetConfigDefaultsList(t *testing.T) {
	_, all := budgetMustParse(t, budgetMinimalObserve)
	_, some := budgetMustParse(t, budgetMinimalObserve+"jitter = \"1s\"\n[budget.estimate]\nquantile = 0.8\n[budget.class.normal]\nrank = 2\n")
	for _, k := range []string{"budget.jitter", "budget.estimate.quantile", "budget.class.normal.rank"} {
		if !budgetHas(all, k) {
			t.Errorf("untouched file: defaults lack %s", k)
		}
		if budgetHas(some, k) {
			t.Errorf("%s was set but is listed as default", k)
		}
	}
	if len(all)-len(some) != 3 {
		t.Errorf("want exactly 3 fewer defaults, got %d vs %d", len(all), len(some))
	}
	for _, k := range []string{"budget.class.normal.reserve_fraction", "budget.stale_after", "budget.routing.enabled"} {
		if !budgetHas(some, k) {
			t.Errorf("untouched %s missing from defaults %v", k, some)
		}
	}
	if budgetHas(all, "budget.mode") {
		t.Error("mode has no default")
	}
}

func budgetHas(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
