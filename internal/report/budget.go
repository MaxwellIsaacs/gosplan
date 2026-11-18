package report

// The budget block of the run report and the views `gosplan budget` prints.
// Read-only: it only calls state loaders and
// the pure budget package. Token and USD numbers are estimates; every
// utilisation number from unifiedWindows, rateLimitType or /usage text is
// undocumented; every default is policy.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// BudgetNotes are the five fixed claim limits of the budget report.
var BudgetNotes = []string{
	"Token and USD numbers are estimates.",
	"Window utilisation from unifiedWindows, rateLimitType and /usage text is undocumented.",
	"--max-budget-usd is a client-side estimate and does not cap plan usage.",
	"Admission reduces rate-limit stalls; it does not prevent them.",
	"Class fractions and budget defaults are policy, not measurement.",
}

// Budget is the run report's budget block.
type Budget struct {
	Mode         string              `json:"mode"`
	Pools        []BudgetPool        `json:"pools"`
	Reservations []BudgetReservation `json:"reservations"`
	Breakers     []BudgetBreaker     `json:"breakers"`
	Estimates    []BudgetEstimate    `json:"estimates"`
	Decisions    BudgetDecisions     `json:"decisions"`
	Halt         *BudgetHalt         `json:"halt"`
	Notes        []string            `json:"notes"`
}

// BudgetDecisions counts the run's recorded admission decisions. Refused
// counts every refusal, including those a later decision event summarises
// as `suppressed: n`; ByRule counts refusals by rule.
type BudgetDecisions struct {
	Admitted int            `json:"admitted"`
	Refused  int            `json:"refused"`
	ByRule   map[string]int `json:"by_rule"`
}

// BudgetHalt is the run's budget halt.
type BudgetHalt struct {
	Kind   string   `json:"kind"`
	Reason string   `json:"reason"`
	Parked []string `json:"parked"`
}

// BudgetPool is one pool at `now`. Headroom is the headroom at
// the horizon named by HorizonBasis, not necessarily at the reset.
type BudgetPool struct {
	Pool           string   `json:"pool"`
	Level          *float64 `json:"level"`
	ObservedAt     *string  `json:"observed_at"`
	AgeSeconds     float64  `json:"age_seconds"`
	Source         string   `json:"source"`
	Documented     bool     `json:"documented"`
	Stale          bool     `json:"stale"`
	StaleReason    string   `json:"stale_reason"`
	ResetsAt       *int64   `json:"resets_at"`
	RatePerHour    float64  `json:"rate_per_hour"`
	RateBasis      string   `json:"rate_basis"`
	HorizonSeconds float64  `json:"horizon_seconds"`
	HorizonBasis   string   `json:"horizon_basis"`
	Projected      float64  `json:"projected"`
	Headroom       float64  `json:"headroom"`
	OpenReserved   float64  `json:"open_reserved"`
}

// BudgetReservation is one reservation. Estimates are estimates; BeltUSD is the client-side belt.
type BudgetReservation struct {
	ID           int64                `json:"id"`
	Run          string               `json:"run"`
	Task         string               `json:"task"`
	Attempt      int                  `json:"attempt"`
	Model        string               `json:"model"`
	Class        string               `json:"class"`
	EstTokens    *float64             `json:"est_tokens"`
	EstBasis     string               `json:"est_basis"`
	Pools        []budget.PoolReserve `json:"pools"`
	BeltUSD      *float64             `json:"belt_usd"`
	AgeSeconds   float64              `json:"age_seconds"`
	Enforced     bool                 `json:"enforced"`
	State        string               `json:"state,omitempty"`
	ActualTokens *int64               `json:"actual_tokens,omitempty"`
	Overrun      *bool                `json:"overrun,omitempty"`
}

// BudgetBreaker is one breaker.
type BudgetBreaker struct {
	Kind     string  `json:"kind"`
	Scope    string  `json:"scope"`
	Run      string  `json:"run"`
	Task     string  `json:"task"`
	State    string  `json:"state"`
	Reason   string  `json:"reason"`
	OpenedAt *string `json:"opened_at"`
	ClosesAt *string `json:"closes_at"`
	Enforced bool    `json:"enforced"`
}

// BudgetEstimate is one model's token estimate with its basis.
type BudgetEstimate struct {
	Model       string   `json:"model"`
	Tokens      *float64 `json:"tokens"`
	Quantile    float64  `json:"quantile"`
	N           int      `json:"n"`
	Basis       string   `json:"basis"`
	DurationMS  *int64   `json:"duration_ms"`
	USDEstimate *float64 `json:"usd_estimate"`
	USDBasis    string   `json:"usd_basis"`
}

// BudgetFit is one pool's fitted token->utilisation ratio (not the plan's
// formula).
type BudgetFit struct {
	Pool              string  `json:"pool"`
	KPerMTok          float64 `json:"k_per_mtok"`
	SEKPerMTok        float64 `json:"se_k_per_mtok"`
	BackgroundPerHour float64 `json:"background_per_hour"`
	N                 int     `json:"n"`
	ResidualRMS       float64 `json:"residual_rms"`
	R2                float64 `json:"r2"`
	SpanSeconds       float64 `json:"span_seconds"`
	Basis             string  `json:"basis"`
}

func optTS(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

// PoolViews fits and views every pool of s exactly as budget.Decide does.
func PoolViews(s budget.Snapshot) []budget.PoolView {
	cfg := s.Config
	names := make([]string, 0, len(s.Obs))
	for p := range s.Obs {
		names = append(names, p)
	}
	sort.Strings(names)
	out := make([]budget.PoolView, 0, len(names))
	for _, pool := range names {
		rows := s.Obs[pool]
		iv := budget.Intervals(pool, rows, s.Spans, cfg.ModelFamilies, cfg.ResetEpsilon, s.Now, cfg.FitLookback)
		fit := budget.FitRatio(pool, iv, cfg)
		var floor *budget.Obs
		if f, ok := s.Floors[pool]; ok {
			floor = &f
		}
		out = append(out, budget.ViewPool(pool, rows, floor, fit, s.Now, cfg))
	}
	return out
}

// openReserved is Σ reserve of the open reservations plus the settled-after-
// observation part, over every enforcement kind.
func openReserved(s budget.Snapshot, v budget.PoolView) float64 {
	sum := 0.0
	for _, r := range s.Open {
		for _, pr := range r.Pools {
			if pr.Pool == v.Pool {
				sum += pr.Reserve
			}
		}
	}
	k := 0.0
	if v.Fit.Basis == "fit" {
		k = v.Fit.KPerToken
	}
	for _, r := range s.Recent {
		if r.SettledAt == nil || (v.ObservedAt != nil && !r.SettledAt.After(*v.ObservedAt)) {
			continue
		}
		for _, pr := range r.Pools {
			if pr.Pool != v.Pool {
				continue
			}
			x := pr.Reserve
			if r.ActualTokens != nil {
				x = math.Max(k**r.ActualTokens, pr.Reserve)
			}
			sum += x
		}
	}
	return sum
}

// PoolItem converts a view to its JSON form.
func PoolItem(v budget.PoolView, openReserved float64) BudgetPool {
	return BudgetPool{
		Pool: v.Pool, Level: v.Level, ObservedAt: optTS(v.ObservedAt), AgeSeconds: v.AgeSeconds,
		Source: v.Source, Documented: v.Documented, Stale: v.Stale, StaleReason: v.StaleReason,
		ResetsAt: v.ResetsAt, RatePerHour: v.Rate.PerHour, RateBasis: v.Rate.Basis,
		HorizonSeconds: v.HorizonSeconds, HorizonBasis: v.HorizonBasis,
		Projected: v.Projected, Headroom: v.Headroom, OpenReserved: openReserved,
	}
}

// BudgetPools is every pool of s as JSON items.
func BudgetPools(s budget.Snapshot) []BudgetPool {
	out := []BudgetPool{}
	for _, v := range PoolViews(s) {
		out = append(out, PoolItem(v, openReserved(s, v)))
	}
	return out
}

// BudgetFits is the fit of every pool of s.
func BudgetFits(s budget.Snapshot) []BudgetFit {
	out := []BudgetFit{}
	for _, v := range PoolViews(s) {
		f := v.Fit
		out = append(out, BudgetFit{
			Pool: v.Pool, KPerMTok: f.KPerToken * 1e6, SEKPerMTok: f.SEK * 1e6, BackgroundPerHour: f.BackgroundPerHour,
			N: f.N, ResidualRMS: f.ResidualRMS, R2: f.R2, SpanSeconds: f.SpanSeconds, Basis: f.Basis,
		})
	}
	return out
}

// EstimateItem converts an estimate to its JSON form.
func EstimateItem(e budget.Estimate) BudgetEstimate {
	return BudgetEstimate{
		Model: e.Model, Tokens: e.Tokens, Quantile: e.Quantile, N: e.N, Basis: e.TokensBasis,
		DurationMS: e.DurationMS, USDEstimate: e.USD, USDBasis: e.USDBasis,
	}
}

// BudgetEstimates is the estimate of every model with samples in s.
func BudgetEstimates(s budget.Snapshot) []BudgetEstimate {
	models := make([]string, 0, len(s.Samples))
	for m := range s.Samples {
		models = append(models, m)
	}
	sort.Strings(models)
	out := []BudgetEstimate{}
	for _, m := range models {
		out = append(out, EstimateItem(budget.EstimateFor(m, s.Samples[m], 0, s.Config)))
	}
	return out
}

func reservationItem(db *sql.DB, r state.ReservationRow, now time.Time) BudgetReservation {
	n := 0
	_ = db.QueryRow(`SELECT n FROM attempts WHERE id = ?`, r.AttemptID).Scan(&n)
	pools := r.Pools
	if pools == nil {
		pools = []budget.PoolReserve{}
	}
	out := BudgetReservation{
		ID: r.ID, Run: r.Run, Task: r.Task, Attempt: n, Model: r.Model, Class: r.Class,
		EstTokens: r.EstTokens, EstBasis: r.EstBasis, Pools: pools, BeltUSD: r.BeltUSD,
		AgeSeconds: now.Sub(r.CreatedAt).Seconds(), Enforced: r.Enforced, State: r.State,
	}
	if r.State == "settled" {
		out.ActualTokens = r.ActualTokens
		if r.ActualTokens != nil && r.EstTokens != nil {
			o := float64(*r.ActualTokens) > *r.EstTokens
			out.Overrun = &o
		}
	}
	return out
}

// OpenReservationItems is every open reservation in the state dir.
func OpenReservationItems(db *sql.DB, now time.Time) ([]BudgetReservation, error) {
	open, err := state.OpenReservations(db)
	if err != nil {
		return nil, err
	}
	runs := map[string]bool{}
	for _, r := range open {
		runs[r.Run] = true
	}
	names := make([]string, 0, len(runs))
	for r := range runs {
		names = append(names, r)
	}
	sort.Strings(names)
	out := []BudgetReservation{}
	for _, run := range names {
		rows, err := state.RunReservations(db, run)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.State == "open" {
				out = append(out, reservationItem(db, r, now))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// BudgetBreakers is every open breaker plus those closed in the last 24 h.
func BudgetBreakers(db *sql.DB, now time.Time) ([]BudgetBreaker, error) {
	rows, err := db.Query(`SELECT kind, scope, run_id, task_id, state, reason, opened_at, closes_at, enforced FROM budget_breakers
		WHERE state = 'open' OR (closed_at IS NOT NULL AND julianday(closed_at) >= julianday(?)) ORDER BY id`,
		now.Add(-24*time.Hour).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("report: budget breakers: %w", err)
	}
	defer rows.Close()
	out := []BudgetBreaker{}
	for rows.Next() {
		var b BudgetBreaker
		var opened, closes sql.NullString
		if err := rows.Scan(&b.Kind, &b.Scope, &b.Run, &b.Task, &b.State, &b.Reason, &opened, &closes, &b.Enforced); err != nil {
			return nil, err
		}
		if opened.Valid {
			b.OpenedAt = &opened.String
		}
		if closes.Valid {
			b.ClosesAt = &closes.String
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// decisionCounts reads the run's budget_decision events (flushed or not).
func decisionCounts(db *sql.DB, run string) (BudgetDecisions, error) {
	d := BudgetDecisions{ByRule: map[string]int{}}
	rows, err := db.Query(`SELECT task_id, data_json FROM budget_events WHERE run_id = ? AND type = 'budget_decision' ORDER BY id`, run)
	if err != nil {
		return d, fmt.Errorf("report: budget decisions: %w", err)
	}
	defer rows.Close()
	lastRule := map[string]string{} // task -> rule of its previous refusal
	for rows.Next() {
		var task, data string
		if err := rows.Scan(&task, &data); err != nil {
			return d, err
		}
		var ev struct {
			Decision struct {
				Admit bool   `json:"admit"`
				Rule  string `json:"rule"`
			} `json:"decision"`
			Suppressed int `json:"suppressed"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return d, fmt.Errorf("report: budget decision event of %s: %w", task, err)
		}
		if ev.Decision.Admit {
			d.Admitted++
			if ev.Suppressed > 0 { // suppressed repeats of the previous refusal
				d.Refused += ev.Suppressed
				d.ByRule[lastRule[task]] += ev.Suppressed
			}
			continue
		}
		n := 1 + ev.Suppressed
		d.Refused += n
		d.ByRule[ev.Decision.Rule] += n
		lastRule[task] = ev.Decision.Rule
	}
	return d, rows.Err()
}

// BuildBudget builds the budget block of run's report at now. It returns nil
// for a schema-1 database (no budget tables). The pools are computed with the
// run's own effective config (budget_runs.config_json) when it has one.
func BuildBudget(ctx context.Context, s *state.Store, run string, now time.Time) (*Budget, error) {
	if s.SchemaVersion() < 2 {
		return nil, nil
	}
	db := s.DB()
	cfg, mode := budget.DefaultConfig(), "off"
	br, err := state.GetBudgetRun(db, run)
	switch {
	case err == nil:
		mode = br.Mode
		var c budget.Config
		if br.ConfigJSON != "" && json.Unmarshal([]byte(br.ConfigJSON), &c) == nil {
			cfg = c
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}
	snap, err := state.LoadSnapshot(db, state.SnapshotRequest{Run: run, Mode: mode, Config: cfg, Now: now})
	if err != nil {
		return nil, err
	}
	b := &Budget{
		Mode: mode, Pools: BudgetPools(snap), Estimates: BudgetEstimates(snap),
		Notes: append([]string(nil), BudgetNotes...),
	}
	rows, err := state.RunReservations(db, run)
	if err != nil {
		return nil, err
	}
	b.Reservations = []BudgetReservation{}
	for _, r := range rows {
		b.Reservations = append(b.Reservations, reservationItem(db, r, now))
	}
	if b.Breakers, err = BudgetBreakers(db, now); err != nil {
		return nil, err
	}
	if b.Decisions, err = decisionCounts(db, run); err != nil {
		return nil, err
	}
	if br.Halt != "" {
		parks, err := state.Parks(db, run)
		if err != nil {
			return nil, err
		}
		h := &BudgetHalt{Kind: br.Halt, Reason: br.HaltReason, Parked: []string{}}
		seen := map[string]bool{}
		add := func(id string) {
			if id != "" && !seen[id] {
				seen[id] = true
				h.Parked = append(h.Parked, id)
			}
		}
		for _, p := range parks {
			add(p.Task)
		}
		// The tasks the halt left pending are in the newest budget_halt event, which the
		// reconciler writes when it finishes the run; a pending task
		// that was never refused has no budget_parks row.
		halted, err := haltParked(db, run)
		if err != nil {
			return nil, err
		}
		for _, id := range halted {
			add(id)
		}
		sort.Strings(h.Parked)
		b.Halt = h
	}
	return b, nil
}

// haltParked is the parked list of the run's newest budget_halt event (nil
// when there is none).
func haltParked(db *sql.DB, run string) ([]string, error) {
	var data string
	err := db.QueryRow(`SELECT data_json FROM budget_events WHERE run_id = ? AND type = 'budget_halt' ORDER BY id DESC LIMIT 1`, run).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("report: budget halt event: %w", err)
	}
	var h budget.Halt
	if err := json.Unmarshal([]byte(data), &h); err != nil {
		return nil, fmt.Errorf("report: budget halt event: %w", err)
	}
	return h.Parked, nil
}

// budgetLine is the one `budget:` line of the status view.
func budgetLine(b *Budget) string {
	pools := []string{}
	for _, p := range b.Pools {
		s := fmt.Sprintf("%s %.0f%%", p.Pool, p.Headroom*100)
		if p.Level == nil {
			s = p.Pool + " -"
		}
		pools = append(pools, s)
	}
	open := 0
	for _, r := range b.Reservations {
		if r.State == "open" {
			open++
		}
	}
	kinds, seen := []string{}, map[string]bool{}
	for _, br := range b.Breakers {
		if br.State == "open" && !seen[br.Kind] {
			seen[br.Kind] = true
			kinds = append(kinds, br.Kind)
		}
	}
	halt := "-"
	if b.Halt != nil {
		halt = b.Halt.Reason
		if halt == "" {
			halt = b.Halt.Kind
		}
	}
	return fmt.Sprintf("budget: %s; pools %s; open %d; breakers %s; halt %s",
		b.Mode, joinOr(pools, " "), open, joinOr(kinds, ","), strings.TrimSpace(halt))
}
