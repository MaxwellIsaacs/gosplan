package budget

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Class is one admission class: its rank (higher is admitted
// first and holds headroom from lower ranks) and the share of each pool's
// projected headroom it holds back from lower classes. Both are policy.
type Class struct {
	Rank            int     `json:"rank"`
	ReserveFraction float64 `json:"reserve_fraction"`
}

// Config is the effective budget config: every key of
// budget.toml with defaults filled. EVERY DEFAULT HERE IS POLICY, chosen by
// the operator or by this spec, not measured: `gosplan budget` lists the keys
// left at default as "policy defaults in effect".
//
// JSON names are the TOML key path below [budget] with '.' replaced by '_'
// ("estimate.min_samples" -> "estimate_min_samples"); classes are the
// object "class" and aliases the object "alias". Durations are Go duration
// strings ("20m0s") in JSON, so Canonical is stable.
type Config struct {
	// Mode is budget.mode as written in the file: off | observe | enforce.
	// Required whenever a file is parsed (no default).
	Mode string

	StaleAfter       time.Duration     // budget.stale_after, policy 20m
	ProbeMinInterval time.Duration     // budget.probe_min_interval, policy 15m
	Lookback         time.Duration     // budget.lookback, policy 2h
	MaxHorizon       time.Duration     // budget.max_horizon, policy = lookback; must be <= lookback
	ResetEpsilon     float64           // budget.reset_epsilon, 0.01 (integer percents observed)
	Jitter           time.Duration     // budget.jitter, policy 30s
	RecheckMax       time.Duration     // budget.recheck_max, policy 5m
	StaleRecheck     time.Duration     // budget.stale_recheck, policy 1m
	FairnessRecheck  time.Duration     // budget.fairness_recheck, policy 5s
	RunTTL           time.Duration     // budget.run_ttl, policy 60s
	StaleMaxOpen     int               // budget.stale_max_open, policy 1
	ModelFamilies    []string          // budget.model_families, names only: opus sonnet haiku fable
	Alias            map[string]string // budget.alias.<source_window> = pool; no default (empty)

	EstimateQuantile     float64       // budget.estimate.quantile, policy 0.9
	EstimateMinSamples   int           // budget.estimate.min_samples, policy 5
	EstimateWindow       int           // budget.estimate.window, policy 200
	EstimateColdTokens   float64       // budget.estimate.cold_tokens, 0 = unknown
	EstimateSafetyFactor float64       // budget.estimate.safety_factor, policy 1.5
	EstimateColdDuration time.Duration // budget.estimate.cold_duration, 0 = unset

	FitLookback    time.Duration // budget.fit.lookback, policy 168h
	FitMinPoints   int           // budget.fit.min_points, policy 8
	FitMaxRelSE    float64       // budget.fit.max_rel_se, policy 0.5
	FitMaxInterval time.Duration // budget.fit.max_interval, policy 30m

	// ColdUnitFraction is budget.cold.unit_fraction. No default in enforce
	// mode (Validate("enforce") fails unless the operator typed it); in
	// observe mode a labelled policy guess of 0.05.
	ColdUnitFraction float64

	// Classes is budget.class.<name>.{rank,reserve_fraction}. Policy
	// defaults: critical 3/0.10, normal 2/0.15, best_effort 1/0. A config
	// may add classes (both keys required for a new class).
	Classes map[string]Class

	// BeltUSDFactor is budget.belt.usd_factor. nil = unset = no
	// --max-budget-usd belt (no default: there is no basis for a factor).
	BeltUSDFactor *float64

	BreakerRetryStormRetries       int           // budget.breaker.retry_storm.retries, policy 6
	BreakerRetryStormWindow        time.Duration // budget.breaker.retry_storm.window, policy 15m
	BreakerRetryStormCooldown      time.Duration // budget.breaker.retry_storm.cooldown, policy 15m
	BreakerSameFailureAttempts     int           // budget.breaker.same_failure.attempts, 3 (brief)
	BreakerRateLimitedCooldownBase time.Duration // budget.breaker.rate_limited.cooldown_base, policy 5m
	BreakerRateLimitedCooldownMax  time.Duration // budget.breaker.rate_limited.cooldown_max, policy 1h

	RoutingEnabled bool // budget.routing.enabled, policy false

	// Defaults lists the keys (full "budget.…" paths, sorted) that were left
	// at their policy default, as ParseConfig returned them. It is not part
	// of the effective config (not in JSON, not hashed); Validate uses it to
	// refuse enforce mode on a cold.unit_fraction nobody typed.
	Defaults []string `json:"-"`
}

// Default class names.
const (
	ClassCritical   = "critical"
	ClassNormal     = "normal"
	ClassBestEffort = "best_effort"
)

// defaultColdUnitFraction is the observe-mode policy guess.
const defaultColdUnitFraction = 0.05

// kind is the value type of one config key.
type kind int

const (
	kDur kind = iota
	kInt
	kFloat
	kOptFloat
	kBool
	kStrings
)

// keyDef is one scalar key: its path below [budget] and where it lives.
type keyDef struct {
	path string
	kind kind
	ptr  func(c *Config) any
}

// keys is every scalar config key except mode, alias and class, which
// are handled separately. Order is irrelevant (outputs are sorted).
var keys = []keyDef{
	{"stale_after", kDur, func(c *Config) any { return &c.StaleAfter }},
	{"probe_min_interval", kDur, func(c *Config) any { return &c.ProbeMinInterval }},
	{"lookback", kDur, func(c *Config) any { return &c.Lookback }},
	{"max_horizon", kDur, func(c *Config) any { return &c.MaxHorizon }},
	{"reset_epsilon", kFloat, func(c *Config) any { return &c.ResetEpsilon }},
	{"jitter", kDur, func(c *Config) any { return &c.Jitter }},
	{"recheck_max", kDur, func(c *Config) any { return &c.RecheckMax }},
	{"stale_recheck", kDur, func(c *Config) any { return &c.StaleRecheck }},
	{"fairness_recheck", kDur, func(c *Config) any { return &c.FairnessRecheck }},
	{"run_ttl", kDur, func(c *Config) any { return &c.RunTTL }},
	{"stale_max_open", kInt, func(c *Config) any { return &c.StaleMaxOpen }},
	{"model_families", kStrings, func(c *Config) any { return &c.ModelFamilies }},
	{"estimate.quantile", kFloat, func(c *Config) any { return &c.EstimateQuantile }},
	{"estimate.min_samples", kInt, func(c *Config) any { return &c.EstimateMinSamples }},
	{"estimate.window", kInt, func(c *Config) any { return &c.EstimateWindow }},
	{"estimate.cold_tokens", kFloat, func(c *Config) any { return &c.EstimateColdTokens }},
	{"estimate.safety_factor", kFloat, func(c *Config) any { return &c.EstimateSafetyFactor }},
	{"estimate.cold_duration", kDur, func(c *Config) any { return &c.EstimateColdDuration }},
	{"fit.lookback", kDur, func(c *Config) any { return &c.FitLookback }},
	{"fit.min_points", kInt, func(c *Config) any { return &c.FitMinPoints }},
	{"fit.max_rel_se", kFloat, func(c *Config) any { return &c.FitMaxRelSE }},
	{"fit.max_interval", kDur, func(c *Config) any { return &c.FitMaxInterval }},
	{"cold.unit_fraction", kFloat, func(c *Config) any { return &c.ColdUnitFraction }},
	{"belt.usd_factor", kOptFloat, func(c *Config) any { return &c.BeltUSDFactor }},
	{"breaker.retry_storm.retries", kInt, func(c *Config) any { return &c.BreakerRetryStormRetries }},
	{"breaker.retry_storm.window", kDur, func(c *Config) any { return &c.BreakerRetryStormWindow }},
	{"breaker.retry_storm.cooldown", kDur, func(c *Config) any { return &c.BreakerRetryStormCooldown }},
	{"breaker.same_failure.attempts", kInt, func(c *Config) any { return &c.BreakerSameFailureAttempts }},
	{"breaker.rate_limited.cooldown_base", kDur, func(c *Config) any { return &c.BreakerRateLimitedCooldownBase }},
	{"breaker.rate_limited.cooldown_max", kDur, func(c *Config) any { return &c.BreakerRateLimitedCooldownMax }},
	{"routing.enabled", kBool, func(c *Config) any { return &c.RoutingEnabled }},
}

// noDefault are keys with no default: never listed as "left at default".
var noDefault = map[string]bool{"belt.usd_factor": true}

func jsonName(path string) string { return strings.ReplaceAll(path, ".", "_") }

func defaultClasses() map[string]Class {
	return map[string]Class{
		ClassCritical:   {Rank: 3, ReserveFraction: 0.10},
		ClassNormal:     {Rank: 2, ReserveFraction: 0.15},
		ClassBestEffort: {Rank: 1, ReserveFraction: 0},
	}
}

// DefaultConfig is the config with every policy default and
// mode observe (the mode when no config file exists). Its Defaults list
// names every defaulted key, including cold.unit_fraction, so
// Validate(ModeEnforce) refuses it.
func DefaultConfig() Config {
	c := Config{
		Mode:                           ModeObserve,
		StaleAfter:                     20 * time.Minute,
		ProbeMinInterval:               15 * time.Minute,
		Lookback:                       2 * time.Hour,
		MaxHorizon:                     2 * time.Hour,
		ResetEpsilon:                   0.01,
		Jitter:                         30 * time.Second,
		RecheckMax:                     5 * time.Minute,
		StaleRecheck:                   time.Minute,
		FairnessRecheck:                5 * time.Second,
		RunTTL:                         60 * time.Second,
		StaleMaxOpen:                   1,
		ModelFamilies:                  []string{"opus", "sonnet", "haiku", "fable"},
		Alias:                          map[string]string{},
		EstimateQuantile:               0.9,
		EstimateMinSamples:             5,
		EstimateWindow:                 200,
		EstimateColdTokens:             0,
		EstimateSafetyFactor:           1.5,
		EstimateColdDuration:           0,
		FitLookback:                    168 * time.Hour,
		FitMinPoints:                   8,
		FitMaxRelSE:                    0.5,
		FitMaxInterval:                 30 * time.Minute,
		ColdUnitFraction:               defaultColdUnitFraction,
		Classes:                        defaultClasses(),
		BeltUSDFactor:                  nil,
		BreakerRetryStormRetries:       6,
		BreakerRetryStormWindow:        15 * time.Minute,
		BreakerRetryStormCooldown:      15 * time.Minute,
		BreakerSameFailureAttempts:     3,
		BreakerRateLimitedCooldownBase: 5 * time.Minute,
		BreakerRateLimitedCooldownMax:  time.Hour,
		RoutingEnabled:                 false,
	}
	c.Defaults = defaultKeyList(nil, nil)
	return c
}

// defaultKeyList returns the sorted full paths of every defaulted key not in
// set, plus each default class key not in classSet.
func defaultKeyList(set map[string]bool, classSet map[string]bool) []string {
	var out []string
	for _, k := range keys {
		if !set[k.path] && !noDefault[k.path] {
			out = append(out, "budget."+k.path)
		}
	}
	for name := range defaultClasses() {
		for _, f := range []string{"rank", "reserve_fraction"} {
			p := "class." + name + "." + f
			if !classSet[p] {
				out = append(out, "budget."+p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ParseConfig decodes budget.toml. Every key lives under
// [budget]; any other key, or an unknown key under [budget], is an error, so
// a typo cannot silently fall back to a default. budget.mode is required.
// When the file names mode "enforce", cold.unit_fraction is required too;
// otherwise it defaults to the 0.05 policy guess. Durations are Go duration
// strings (time.ParseDuration). It returns the effective config and the
// sorted full paths of the keys left at their policy default. It does not
// run Validate: the caller validates for the resolved mode.
func ParseConfig(b []byte) (Config, []string, error) {
	var raw map[string]any
	if _, err := toml.Decode(string(b), &raw); err != nil {
		return Config{}, nil, fmt.Errorf("budget config: %w", err)
	}
	for k := range raw {
		if k != "budget" {
			return Config{}, nil, fmt.Errorf("budget config: unknown key %q (every key lives under [budget])", k)
		}
	}
	top, ok := raw["budget"].(map[string]any)
	if !ok {
		return Config{}, nil, errors.New("budget config: budget.mode must be set")
	}
	flat := map[string]any{}
	var flatten func(prefix string, m map[string]any) error
	flatten = func(prefix string, m map[string]any) error {
		for k, v := range m {
			p := prefix + k
			if p == "alias" || p == "class" {
				flat[p] = v // handled below
				continue
			}
			if sub, ok := v.(map[string]any); ok {
				if err := flatten(p+".", sub); err != nil {
					return err
				}
				continue
			}
			flat[p] = v
		}
		return nil
	}
	if err := flatten("", top); err != nil {
		return Config{}, nil, err
	}

	c := DefaultConfig()
	c.Alias = map[string]string{}
	set := map[string]bool{}
	classSet := map[string]bool{}

	modeV, ok := flat["mode"]
	if !ok {
		return Config{}, nil, errors.New("budget config: budget.mode must be set")
	}
	mode, ok := modeV.(string)
	if !ok || !validMode(mode) {
		return Config{}, nil, fmt.Errorf("budget config: budget.mode must be off, observe or enforce, got %v", modeV)
	}
	c.Mode = mode
	delete(flat, "mode")

	if v, ok := flat["alias"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return Config{}, nil, errors.New("budget config: budget.alias must be a table of window = \"pool\"")
		}
		for src, dst := range m {
			s, ok := dst.(string)
			if !ok || s == "" {
				return Config{}, nil, fmt.Errorf("budget config: budget.alias.%s must be a non-empty string", src)
			}
			c.Alias[src] = s
		}
		delete(flat, "alias")
	}
	if v, ok := flat["class"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return Config{}, nil, errors.New("budget config: budget.class must be a table of class tables")
		}
		for name, cv := range m {
			cm, ok := cv.(map[string]any)
			if !ok {
				return Config{}, nil, fmt.Errorf("budget config: budget.class.%s must be a table", name)
			}
			cl, known := c.Classes[name]
			for f, fv := range cm {
				p := "budget.class." + name + "." + f
				switch f {
				case "rank":
					n, ok := fv.(int64)
					if !ok {
						return Config{}, nil, fmt.Errorf("budget config: %s must be an integer", p)
					}
					cl.Rank = int(n)
				case "reserve_fraction":
					x, ok := toFloat(fv)
					if !ok {
						return Config{}, nil, fmt.Errorf("budget config: %s must be a number", p)
					}
					cl.ReserveFraction = x
				default:
					return Config{}, nil, fmt.Errorf("budget config: unknown key %q", p)
				}
				classSet["class."+name+"."+f] = true
			}
			if !known && (!classSet["class."+name+".rank"] || !classSet["class."+name+".reserve_fraction"]) {
				return Config{}, nil, fmt.Errorf("budget config: new class %q needs both rank and reserve_fraction", name)
			}
			c.Classes[name] = cl
		}
		delete(flat, "class")
	}

	byPath := map[string]keyDef{}
	for _, k := range keys {
		byPath[k.path] = k
	}
	for p, v := range flat {
		k, ok := byPath[p]
		if !ok {
			return Config{}, nil, fmt.Errorf("budget config: unknown key %q", "budget."+p)
		}
		if err := setTOML(&c, k, v); err != nil {
			return Config{}, nil, fmt.Errorf("budget config: budget.%s: %w", p, err)
		}
		set[p] = true
	}
	if !set["max_horizon"] {
		c.MaxHorizon = c.Lookback // default: = lookback
	}
	if !set["cold.unit_fraction"] && c.Mode == ModeEnforce {
		return Config{}, nil, errors.New("budget config: budget.cold.unit_fraction must be set in enforce mode (no default)")
	}
	c.Defaults = defaultKeyList(set, classSet)
	return c, append([]string(nil), c.Defaults...), nil
}

func validMode(m string) bool { return m == ModeOff || m == ModeObserve || m == ModeEnforce }

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func setTOML(c *Config, k keyDef, v any) error {
	switch p := k.ptr(c).(type) {
	case *time.Duration:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("want a duration string such as \"15m\", got %v", v)
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*p = d
	case *int:
		n, ok := v.(int64)
		if !ok {
			return fmt.Errorf("want an integer, got %v", v)
		}
		*p = int(n)
	case *float64:
		x, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("want a number, got %v", v)
		}
		*p = x
	case **float64:
		x, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("want a number, got %v", v)
		}
		*p = &x
	case *bool:
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("want true or false, got %v", v)
		}
		*p = b
	case *[]string:
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("want an array of strings, got %v", v)
		}
		out := []string{}
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("want an array of strings, got element %v", e)
			}
			out = append(out, s)
		}
		*p = out
	default:
		return fmt.Errorf("internal: unhandled key type %T", p)
	}
	return nil
}

// Validate applies the config rules for the resolved mode (the --budget
// flag, else the file's mode, else observe). Exit 2 territory: every error
// names the key.
func (c Config) Validate(mode string) error {
	if !validMode(mode) {
		return fmt.Errorf("budget: mode must be off, observe or enforce, got %q", mode)
	}
	if !validMode(c.Mode) {
		return errors.New("budget.mode must be set")
	}
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	for _, k := range keys {
		switch p := k.ptr(&c).(type) {
		case *time.Duration:
			if *p < 0 {
				bad("budget.%s must be >= 0, got %s", k.path, *p)
			}
		case *int:
			if *p < 0 {
				bad("budget.%s must be >= 0, got %d", k.path, *p)
			}
		case *float64:
			if math.IsNaN(*p) || math.IsInf(*p, 0) {
				bad("budget.%s must be finite", k.path)
			}
		}
	}
	if c.StaleAfter <= 0 {
		bad("budget.stale_after must be > 0, got %s", c.StaleAfter)
	}
	if !(c.EstimateQuantile > 0 && c.EstimateQuantile < 1) {
		bad("budget.estimate.quantile must be in (0,1), got %v", c.EstimateQuantile)
	}
	if mode == ModeEnforce && c.unitFractionDefaulted() {
		bad("budget.cold.unit_fraction must be set in enforce mode (no default)")
	}
	if !(c.ColdUnitFraction > 0 && c.ColdUnitFraction <= 1) {
		bad("budget.cold.unit_fraction must be in (0,1], got %v", c.ColdUnitFraction)
	}
	if c.BeltUSDFactor != nil && !(*c.BeltUSDFactor > 0) {
		bad("budget.belt.usd_factor must be > 0 when set, got %v", *c.BeltUSDFactor)
	}
	if c.MaxHorizon > c.Lookback {
		bad("budget.max_horizon (%s) must be <= budget.lookback (%s)", c.MaxHorizon, c.Lookback)
	}
	if !(c.FitMaxRelSE > 0) {
		bad("budget.fit.max_rel_se must be > 0, got %v", c.FitMaxRelSE)
	}
	if !(c.ResetEpsilon >= 0 && c.ResetEpsilon < 0.5) {
		bad("budget.reset_epsilon must be in [0,0.5), got %v", c.ResetEpsilon)
	}
	if len(c.Classes) == 0 {
		bad("budget.class: at least one class is required")
	}
	names := c.classNames()
	ranks := map[int]string{}
	lowest := ""
	for _, n := range names {
		cl := c.Classes[n]
		if !(cl.ReserveFraction >= 0 && cl.ReserveFraction < 1) {
			bad("budget.class.%s.reserve_fraction must be in [0,1), got %v", n, cl.ReserveFraction)
		}
		if other, dup := ranks[cl.Rank]; dup {
			bad("budget.class: ranks must be distinct (%s and %s both have rank %d)", other, n, cl.Rank)
		}
		ranks[cl.Rank] = n
		if lowest == "" || cl.Rank < c.Classes[lowest].Rank {
			lowest = n
		}
	}
	sum := 0.0
	for _, n := range names {
		if n != lowest {
			sum += c.Classes[n].ReserveFraction
		}
	}
	if len(names) > 0 && !(sum < 1) {
		bad("budget.class: the reserve_fractions of the classes above the lowest (%s) must sum to < 1, got %v", lowest, sum)
	}
	for i, f := range c.ModelFamilies {
		if strings.TrimSpace(f) == "" {
			bad("budget.model_families[%d] is empty", i)
		}
	}
	return errors.Join(errs...)
}

func (c Config) unitFractionDefaulted() bool {
	for _, k := range c.Defaults {
		if k == "budget.cold.unit_fraction" {
			return true
		}
	}
	return false
}

func (c Config) classNames() []string {
	names := make([]string, 0, len(c.Classes))
	for n := range c.Classes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ClassRank returns the rank of class name, and false when the config does
// not define it.
func (c Config) ClassRank(name string) (int, bool) {
	cl, ok := c.Classes[name]
	return cl.Rank, ok
}

// MarshalJSON encodes the effective config with durations as Go duration
// strings, nil BeltUSDFactor as null, and object keys sorted.
func (c Config) MarshalJSON() ([]byte, error) {
	m := map[string]any{"mode": c.Mode}
	for _, k := range keys {
		switch p := k.ptr(&c).(type) {
		case *time.Duration:
			m[jsonName(k.path)] = p.String()
		case *[]string:
			v := *p
			if v == nil {
				v = []string{}
			}
			m[jsonName(k.path)] = v
		case *int:
			m[jsonName(k.path)] = *p
		case *float64:
			m[jsonName(k.path)] = *p
		case **float64:
			m[jsonName(k.path)] = *p
		case *bool:
			m[jsonName(k.path)] = *p
		}
	}
	alias := c.Alias
	if alias == nil {
		alias = map[string]string{}
	}
	m["alias"] = alias
	classes := c.Classes
	if classes == nil {
		classes = map[string]Class{}
	}
	m["class"] = classes
	return json.Marshal(m)
}

// UnmarshalJSON is the inverse of MarshalJSON. Unknown fields are an error.
func (c *Config) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var out Config
	byName := map[string]keyDef{}
	for _, k := range keys {
		byName[jsonName(k.path)] = k
	}
	for name, raw := range m {
		switch name {
		case "mode":
			if err := json.Unmarshal(raw, &out.Mode); err != nil {
				return fmt.Errorf("budget config json: mode: %w", err)
			}
			continue
		case "alias":
			if err := json.Unmarshal(raw, &out.Alias); err != nil {
				return fmt.Errorf("budget config json: alias: %w", err)
			}
			continue
		case "class":
			if err := json.Unmarshal(raw, &out.Classes); err != nil {
				return fmt.Errorf("budget config json: class: %w", err)
			}
			continue
		}
		k, ok := byName[name]
		if !ok {
			return fmt.Errorf("budget config json: unknown field %q", name)
		}
		switch p := k.ptr(&out).(type) {
		case *time.Duration:
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("budget config json: %s: %w", name, err)
			}
			d, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("budget config json: %s: %w", name, err)
			}
			*p = d
		default:
			if err := json.Unmarshal(raw, p); err != nil {
				return fmt.Errorf("budget config json: %s: %w", name, err)
			}
		}
	}
	*c = out
	return nil
}

// Canonical is the canonical JSON of the effective config: object keys
// sorted at every level, durations as Go duration strings, floats in Go's
// shortest round-tripping form. Defaults (provenance) is not included.
func (c Config) Canonical() []byte {
	b, err := c.MarshalJSON() // maps marshal with sorted keys
	if err != nil {
		// Every field is a plain value; Marshal cannot fail on it.
		panic("budget: canonical config: " + err.Error())
	}
	return b
}

// SHA256 is the hex sha256 of Canonical(): config_sha256 in every snapshot.
func (c Config) SHA256() string {
	h := sha256.Sum256(c.Canonical())
	return hex.EncodeToString(h[:])
}
