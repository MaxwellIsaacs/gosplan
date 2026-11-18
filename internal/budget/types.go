// Package budget holds gosplan's pure budget decisions. Nothing here does I/O or reads a clock: every exported function
// is a deterministic function of its arguments, so `gosplan replay --check` can
// recompute each recorded decision. Estimates are estimates, a fitted ratio
// is not the plan's formula, and every config default is policy.
package budget

import "time"

// Modes.
const (
	ModeOff     = "off"
	ModeObserve = "observe"
	ModeEnforce = "enforce"
)

// Rules: why a decision came out as it did (Decision.Rule).
const (
	RuleAdmit     = "admit"
	RuleBootstrap = "bootstrap"  // no observation at all: one attempt at a time
	RuleStale     = "stale"      // an applicable pool is stale: stale_max_open
	RuleHeadroom  = "headroom"   // projected headroom too small in a pool
	RuleClassHold = "class_hold" // fits only into headroom held for higher classes
	RuleFairness  = "fairness"   // fits alone, not after the candidates ahead
	RuleBreaker   = "breaker"
	RuleDeadline  = "deadline"
	RuleCap       = "cap"
	RuleHalt      = "halt"
)

// Breaker kinds and scopes. VerifierError is logged only: the reconciler
// owns that halt.
const (
	RetryStorm    = "RETRY_STORM"
	SameFailure   = "SAME_FAILURE"
	VerifierError = "VERIFIER_ERROR"
	RateLimited   = "RATE_LIMITED"

	ScopeGlobal = "global"
	ScopeRun    = "run"
	ScopeTask   = "task"
)

// Obs is one window_observations row, window already aliased to its pool.
type Obs struct {
	ID          int64     `json:"id"`
	Pool        string    `json:"pool"`
	Utilization *float64  `json:"utilization"`
	ResetsAt    *int64    `json:"resets_at"` // unix seconds
	Status      string    `json:"status"`
	Source      string    `json:"source"`
	Documented  bool      `json:"documented"`
	ObservedAt  time.Time `json:"observed_at"`
}

// Tokens is a usage breakdown; Total is the sum of the four classes.
type Tokens struct {
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CacheRead   float64 `json:"cache_read"`
	CacheCreate float64 `json:"cache_create"`
}

func (t Tokens) Total() float64 { return t.Input + t.Output + t.CacheRead + t.CacheCreate }

// Span is one attempt as the fit and the fairness rule see it.
type Span struct {
	AttemptID int64      `json:"attempt_id"`
	Run       string     `json:"run"`
	Task      string     `json:"task"`
	Model     string     `json:"model"`
	Start     time.Time  `json:"start"`
	End       *time.Time `json:"end"`   // nil while running
	Usage     *Tokens    `json:"usage"` // nil when unknown
}

// Sample is one finished attempt with usage, for estimates.
type Sample struct {
	AttemptID  int64   `json:"attempt_id"`
	Model      string  `json:"model"`
	Usage      Tokens  `json:"usage"`
	NumTurns   int     `json:"num_turns"`
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd_estimate"`
}

// PoolReserve is one pool's share of a reservation.
type PoolReserve struct {
	Pool    string  `json:"pool"`
	Reserve float64 `json:"reserve"` // utilisation fraction
	Basis   string  `json:"basis"`   // "ratio" | "unit"
}

// Reservation is a live open reservation, or (in Snapshot.Recent) one
// settled within stale_after (any run in the state dir).
type Reservation struct {
	ID           int64         `json:"id"`
	Run          string        `json:"run"`
	Task         string        `json:"task"`
	AttemptID    int64         `json:"attempt_id"`
	Model        string        `json:"model"`
	Class        string        `json:"class"`
	Pools        []PoolReserve `json:"pools"`
	Enforced     bool          `json:"enforced"`
	CreatedAt    time.Time     `json:"created_at"`
	SettledAt    *time.Time    `json:"settled_at"`    // nil while open
	ActualTokens *float64      `json:"actual_tokens"` // nil while open or unknown
}

// Key names a candidate attempt.
type Key struct {
	Run     string `json:"run"`
	Task    string `json:"task"`
	Attempt int    `json:"attempt"` // the attempt number it would get
}

// Candidate is a dispatchable task in some live run.
type Candidate struct {
	Key
	Class        string    `json:"class"`
	Models       []string  `json:"models"` // one model, or the prefer list when routing is on
	MaxTurns     int       `json:"max_turns"`
	RunStartedAt time.Time `json:"run_started_at"`
	Tier         int       `json:"tier"`
}

// RunState is what the decision needs about one live run.
type RunState struct {
	Run      string     `json:"run"`
	Mode     string     `json:"mode"`
	Deadline *time.Time `json:"deadline"`
	// Runs with Halt set contribute no candidates.
	Halt          string             `json:"halt"` // "", "drain", "stop"
	HaltReason    string             `json:"halt_reason"`
	CapUSD        *float64           `json:"cap_usd_estimate"`
	CapWindow     map[string]float64 `json:"cap_window"`
	SpentUSD      float64            `json:"spent_usd_estimate"` // settled cost + open belts
	Attributable  map[string]float64 `json:"attributable"`       // pool -> utilisation attributed to the run
	SettledTokens map[string]float64 `json:"settled_tokens"`     // pool -> settled tokens in the pool's current segment
	Running       int                `json:"running"`            // running + verifying
	Pending       []string           `json:"pending"`            // pending task ids (for halt/parked lists)
}

// Breaker is one budget_breakers row.
type Breaker struct {
	ID       int64      `json:"id"`
	Kind     string     `json:"kind"`
	Scope    string     `json:"scope"`
	Run      string     `json:"run"`
	Task     string     `json:"task"`
	State    string     `json:"state"` // "open" | "closed"
	Count    int        `json:"count"`
	OpenedAt *time.Time `json:"opened_at"`
	ClosesAt *time.Time `json:"closes_at"`
	ClosedAt *time.Time `json:"closed_at"`
	Reason   string     `json:"reason"`
	Enforced bool       `json:"enforced"`
}

// Snapshot is every input of one admission decision.
type Snapshot struct {
	Now          time.Time           `json:"now"`
	Mode         string              `json:"mode"`
	Config       Config              `json:"config"`
	ConfigSHA256 string              `json:"config_sha256"`
	Obs          map[string][]Obs    `json:"obs"`      // pool -> rows in max(lookback, fit.lookback), sorted
	Status       []Obs               `json:"status"`   // newest rows incl. window "unknown", for RATE_LIMITED
	Spans        []Span              `json:"spans"`    // attempts overlapping fit.lookback
	Samples      map[string][]Sample `json:"samples"`  // model -> newest estimate.window samples
	Floors       map[string]Obs      `json:"floors"`   // pool -> newest row this process ingested itself
	Open         []Reservation       `json:"open"`     // live open reservations
	Recent       []Reservation       `json:"recent"`   // settled within stale_after
	Breakers     []Breaker           `json:"breakers"` // open breakers
	Candidates   []Candidate         `json:"candidates"`
	Runs         []RunState          `json:"runs"`
	For          Key                 `json:"for"`
}

// Rate is a burn rate.
type Rate struct {
	PerHour         float64 `json:"per_hour"`
	Pairs           int     `json:"pairs"`
	SpanSeconds     float64 `json:"span_seconds"`
	ResidualPerHour float64 `json:"residual_per_hour"`
	Basis           string  `json:"basis"` // "fit_background" | "observed_total" | "none"
}

// Interval is one fit interval.
type Interval struct {
	From     time.Time          `json:"from"`
	To       time.Time          `json:"to"`
	DeltaU   float64            `json:"delta_u"`
	Tokens   float64            `json:"tokens"`
	ByClass  Tokens             `json:"by_class"`
	ByModel  map[string]float64 `json:"by_model"`
	Complete bool               `json:"complete"`
}

// Fit is the fitted token->utilisation ratio of one pool.
type Fit struct {
	Pool              string  `json:"pool"`
	KPerToken         float64 `json:"k_per_token"`
	SEK               float64 `json:"se_k"` // standard error of KPerToken
	BackgroundPerHour float64 `json:"background_per_hour"`
	N                 int     `json:"n"`
	ResidualRMS       float64 `json:"residual_rms"`
	R2                float64 `json:"r2"`
	SpanSeconds       float64 `json:"span_seconds"`
	Basis             string  `json:"basis"` // "fit" | "none:<reason>", incl. "none:poor_fit"
}

// Estimate is one model's estimate with its basis.
type Estimate struct {
	Model         string   `json:"model"`
	Tokens        *float64 `json:"tokens"` // nil = unknown
	TokensBasis   string   `json:"tokens_basis"`
	N             int      `json:"n"`
	Quantile      float64  `json:"quantile"`
	DurationMS    *int64   `json:"duration_ms"`
	DurationBasis string   `json:"duration_basis"`
	USD           *float64 `json:"usd_estimate"`
	USDBasis      string   `json:"usd_basis"`
}

// PoolView is one pool at decision time.
type PoolView struct {
	Pool           string     `json:"pool"`
	Level          *float64   `json:"level"`
	ObservedAt     *time.Time `json:"observed_at"`
	AgeSeconds     float64    `json:"age_seconds"`
	Source         string     `json:"source"`
	Documented     bool       `json:"documented"`
	Status         string     `json:"status"`
	ResetsAt       *int64     `json:"resets_at"`
	Stale          bool       `json:"stale"`
	StaleReason    string     `json:"stale_reason"`
	Rate           Rate       `json:"rate"`
	HorizonSeconds float64    `json:"horizon_seconds"`
	HorizonBasis   string     `json:"horizon_basis"` // "reset" | "max_horizon"
	Projected      float64    `json:"projected"`
	Headroom       float64    `json:"headroom"` // headroom at the horizon
	Fit            Fit        `json:"fit"`
}

// PoolResult is one pool's check for one candidate model.
type PoolResult struct {
	PoolView
	OpenReserved float64 `json:"open_reserved"` // live open + settled-after-observation (all runs)
	Ahead        float64 `json:"ahead"`         // virtual reservations of candidates ahead
	Hold         float64 `json:"hold"`
	Reserve      float64 `json:"reserve"`
	ReserveBasis string  `json:"reserve_basis"`
	Fits         bool    `json:"fits"`
}

// Decision is the outcome of Decide for Snapshot.For.
type Decision struct {
	For        Key           `json:"for"`
	Admit      bool          `json:"admit"`
	Model      string        `json:"model"`
	Rule       string        `json:"rule"`
	Reason     string        `json:"reason"`
	Pools      []PoolResult  `json:"pools"`
	StalePools []string      `json:"stale_pools"`
	Estimate   Estimate      `json:"estimate"`
	Reserve    []PoolReserve `json:"reserve"` // what to reserve when admitted
	BeltUSD    *float64      `json:"belt_usd"`
	AdmitAt    *time.Time    `json:"admit_at"`
	NotBefore  *time.Time    `json:"not_before"` // set on refusal
	Order      []Key         `json:"order"`      // fairness order of all candidates
	Breaker    string        `json:"breaker"`    // kind, when Rule == breaker
	Notes      []string      `json:"notes"`
}

// AttemptFact is one finished attempt of a run, for breakers. The gate
// builds these from its hook calls (memory), not from attempts rows.
type AttemptFact struct {
	AttemptID   int64     `json:"attempt_id"`
	Task        string    `json:"task"`
	N           int       `json:"n"`
	Refunded    bool      `json:"refunded"` // start_failed or lost: master refunded it
	StartedAt   time.Time `json:"started_at"`
	BaseSHA     string    `json:"base_sha"`
	HeadSHA     string    `json:"head_sha"`
	Verdict     string    `json:"verdict"` // pass | fail | error | ""
	FailedGates []string  `json:"failed_gates"`
}

// BreakerInput is every input of one breaker evaluation.
type BreakerInput struct {
	Now      time.Time     `json:"now"`
	Mode     string        `json:"mode"`
	Config   Config        `json:"config"`
	Run      string        `json:"run"`
	Task     string        `json:"task"`     // set for SAME_FAILURE checks
	Attempts []AttemptFact `json:"attempts"` // the run's attempts, by id
	Verdict  string        `json:"verdict"`  // the verdict just recorded (VERIFIER_ERROR is logged only)
	Status   []Obs         `json:"status"`   // newest observations, for RATE_LIMITED
	Current  []Breaker     `json:"current"`  // current rows of the kinds evaluated
}

// BreakerChange is one action EvalBreakers asks for.
type BreakerChange struct {
	Action  string  `json:"action"` // "open" | "close" | "count"
	Breaker Breaker `json:"breaker"`
}

// Halt is a run-level stop.
type Halt struct {
	Kind   string   `json:"kind"` // "" | "drain" | "stop"
	Reason string   `json:"reason"`
	Parked []string `json:"parked"`
}
