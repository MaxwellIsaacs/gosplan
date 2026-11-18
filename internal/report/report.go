// Package report builds the machine-readable run report (report.json, schema
// gosplan.run-report/v1) and the human status view from the SQLite state.
// It only describes: nothing here changes a task, a run or the database.
package report

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
	"github.com/MaxwellIsaacs/gosplan/internal/window"
)

// Schema is the report's schema id.
const Schema = "gosplan.run-report/v1"

// Notes are the claim limits that apply to every report.
var Notes = []string{
	"cost_usd_estimate is a client-side estimate, not billing or plan usage.",
	"Window figures come from undocumented sources (unifiedWindows, rateLimitType, usage text); they are not an official API.",
	"Mutation results are an advisory signal about test strength on changed lines; they never change a task's status.",
	"A signed verdict means only that the holder of the key made the statement about that tree; it does not show the tests ran or ran honestly.",
}

// Report is the whole run report.
type Report struct {
	Schema            string   `json:"schema"`
	Run               string   `json:"run"`
	SpecDir           string   `json:"spec_dir"`
	Project           string   `json:"project"`
	BaseSHA           string   `json:"base_sha"`
	Status            string   `json:"status"`
	StartedAt         string   `json:"started_at"`
	FinishedAt        *string  `json:"finished_at"`
	IntegrationBranch string   `json:"integration_branch"`
	IntegrationSHA    string   `json:"integration_sha"`
	Tasks             []Task   `json:"tasks"`
	Windows           []Window `json:"windows"`
	Notes             []string `json:"notes"`
	Budget            *Budget  `json:"budget,omitempty"`
}

// Task is one task of the run.
type Task struct {
	ID            string   `json:"id"`
	Tier          int      `json:"tier"`
	Status        string   `json:"status"`
	Deps          []string `json:"deps"`
	DepsWaiting   []string `json:"deps_waiting"` // deps not yet passed
	DepsSource    string   `json:"deps_source"`
	BlockedReason string   `json:"blocked_reason"`
	// LeaseAgeSeconds is the age of the task's lease heartbeat; absent
	// when no lease is held. Not in the documented report; the status view needs it.
	LeaseAgeSeconds *int64    `json:"lease_age_seconds,omitempty"`
	Attempts        []Attempt `json:"attempts"`
}

// Attempt is one worker attempt.
type Attempt struct {
	N               int            `json:"n"`
	Model           string         `json:"model"`
	Outcome         string         `json:"outcome"`
	ExitCode        *int           `json:"exit_code"`
	ResultSubtype   string         `json:"result_subtype"`
	NumTurns        int            `json:"num_turns"`
	DurationMS      int64          `json:"duration_ms"`
	CostUSDEstimate float64        `json:"cost_usd_estimate"`
	HeadSHA         string         `json:"head_sha"`
	Verdict         *Verdict       `json:"verdict"` // null until set
	AttestationPath string         `json:"attestation_path"`
	Mutation        map[string]any `json:"mutation"` // null if none; advisory is always true
}

// Verdict is the verifier's verdict for an attempt.
type Verdict struct {
	Result      string   `json:"result"`
	FailedGates []string `json:"failed_gates"`
	TreeSHA     string   `json:"tree_sha"`
	CacheHit    bool     `json:"cache_hit"`
}

// Window is the latest observation of one usage window.
type Window struct {
	Window      string   `json:"window"`
	Utilization *float64 `json:"utilization"` // fraction 0..1
	ResetsAt    *int64   `json:"resets_at"`   // unix seconds
	Status      string   `json:"status"`
	Source      string   `json:"source"`
	Documented  bool     `json:"documented"`
	ObservedAt  string   `json:"observed_at"`
	AgeSeconds  int64    `json:"age_seconds"`
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Build reads everything for run from s (read-only queries only).
func Build(ctx context.Context, s *state.Store, run string, now time.Time) (Report, error) {
	rr, err := s.GetRun(ctx, run)
	if err != nil {
		return Report{}, err
	}
	r := Report{
		Schema: Schema, Run: rr.ID, SpecDir: rr.SpecDir, Project: rr.Project, BaseSHA: rr.BaseSHA,
		Status: rr.Status, StartedAt: ts(rr.StartedAt),
		IntegrationBranch: rr.IntegrationBranch, IntegrationSHA: rr.IntegrationSHA,
		Tasks: []Task{}, Windows: []Window{}, Notes: append([]string(nil), Notes...),
	}
	if rr.FinishedAt != nil {
		f := ts(*rr.FinishedAt)
		r.FinishedAt = &f
	}
	rows, err := s.ListTasks(ctx, run)
	if err != nil {
		return Report{}, err
	}
	leases, err := s.Leases(ctx, run)
	if err != nil {
		return Report{}, err
	}
	leaseAge := map[string]int64{}
	for _, l := range leases {
		leaseAge[l.Task] = int64(now.Sub(l.HeartbeatAt).Seconds())
	}
	status := map[string]string{}
	for _, t := range rows {
		status[t.ID] = t.Status
	}
	for _, t := range rows {
		task := Task{
			ID: t.ID, Tier: t.Tier, Status: t.Status, Deps: nonNil(t.Deps), DepsWaiting: []string{},
			DepsSource: t.DepsSource, BlockedReason: t.BlockedReason, Attempts: []Attempt{},
		}
		for _, d := range t.Deps {
			if status[d] != "passed" {
				task.DepsWaiting = append(task.DepsWaiting, d)
			}
		}
		if age, ok := leaseAge[t.ID]; ok {
			task.LeaseAgeSeconds = &age
		}
		atts, err := s.ListAttempts(ctx, run, t.ID)
		if err != nil {
			return Report{}, err
		}
		for _, a := range atts {
			task.Attempts = append(task.Attempts, attempt(a))
		}
		r.Tasks = append(r.Tasks, task)
	}
	wins, err := s.LatestWindows(ctx)
	if err != nil {
		return Report{}, err
	}
	for _, w := range wins {
		r.Windows = append(r.Windows, Window{
			Window: w.Window, Utilization: w.Utilization, ResetsAt: w.ResetsAt, Status: w.Status,
			Source: w.Source, Documented: w.Documented, ObservedAt: ts(w.ObservedAt),
			AgeSeconds: int64(now.Sub(w.ObservedAt).Seconds()),
		})
	}
	if r.Budget, err = BuildBudget(ctx, s, run, now); err != nil {
		return Report{}, err
	}
	return r, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func attempt(a state.AttemptRow) Attempt {
	out := Attempt{
		N: a.N, Model: a.Model, Outcome: a.Outcome, ExitCode: a.ExitCode, ResultSubtype: a.ResultSubtype,
		NumTurns: a.NumTurns, DurationMS: a.DurationMS, CostUSDEstimate: a.CostUSDEstimate,
		HeadSHA: a.HeadSHA, AttestationPath: a.AttestationPath,
	}
	if a.VerdictResult != "" {
		v := &Verdict{Result: a.VerdictResult, FailedGates: nonNil(a.FailedGates)}
		var raw struct {
			TreeSHA  string `json:"tree_sha"`
			CacheHit bool   `json:"cache_hit"`
		}
		if len(a.VerdictJSON) > 0 && json.Unmarshal(a.VerdictJSON, &raw) == nil {
			v.TreeSHA, v.CacheHit = raw.TreeSHA, raw.CacheHit
		}
		out.Verdict = v
	}
	if len(a.MutationJSON) > 0 {
		m := map[string]any{}
		if err := json.Unmarshal(a.MutationJSON, &m); err != nil || m == nil {
			m = map[string]any{"status": "unparseable"}
		}
		m["advisory"] = true
		out.Mutation = m
	}
	return out
}

// JSON renders r as indented JSON with a trailing newline.
func JSON(r Report) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Write writes r as indented JSON to path (temp + rename).
func Write(path string, r Report) error {
	b, err := JSON(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".report-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Text renders the human status view.
func Text(r Report, now time.Time) string {
	var b strings.Builder
	fin := "-"
	if r.FinishedAt != nil {
		fin = clock(*r.FinishedAt)
	}
	fmt.Fprintf(&b, "run %s  %s  base %s  started %s  finished %s\n", r.Run, r.Status, short(r.BaseSHA), clock(r.StartedAt), fin)
	if r.IntegrationBranch != "" {
		fmt.Fprintf(&b, "integration %s @ %s\n", r.IntegrationBranch, short(r.IntegrationSHA))
	}
	rows := [][]string{{"TASK", "STATUS", "ATT", "WAITING", "LEASE", "VERDICT", "MUTATION (advisory)"}}
	for _, t := range r.Tasks {
		lease := "-"
		if t.LeaseAgeSeconds != nil {
			lease = window.Age(now, now.Add(-time.Duration(*t.LeaseAgeSeconds)*time.Second))
		}
		rows = append(rows, []string{t.ID, t.Status, fmt.Sprint(len(t.Attempts)), joinOr(t.DepsWaiting, ","),
			lease, verdictText(t), mutationText(t)})
	}
	table(&b, rows)
	if len(r.Windows) > 0 {
		rows = [][]string{{"WINDOW", "USED", "RESETS", "SOURCE", "AGE"}}
		for _, w := range r.Windows {
			used, resets := "-", "-"
			if w.Utilization != nil {
				used = fmt.Sprintf("%.0f%%", *w.Utilization*100)
			}
			if w.ResetsAt != nil {
				resets = time.Unix(*w.ResetsAt, 0).UTC().Format("2006-01-02 15:04")
			}
			age := window.Age(now, now.Add(-time.Duration(w.AgeSeconds)*time.Second))
			if !w.Documented {
				age += " (undocumented)"
			}
			rows = append(rows, []string{w.Window, used, resets, w.Source, age})
		}
		table(&b, rows)
	}
	if r.Budget != nil {
		fmt.Fprintln(&b, budgetLine(r.Budget))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}

func clock(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	return t.UTC().Format("15:04")
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func joinOr(s []string, sep string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, sep)
}

// verdictText describes the newest attempt that has a verdict.
func verdictText(t Task) string {
	for i := len(t.Attempts) - 1; i >= 0; i-- {
		a := t.Attempts[i]
		if a.Verdict == nil {
			continue
		}
		s := a.Verdict.Result
		if len(a.Verdict.FailedGates) > 0 {
			s += " " + strings.Join(a.Verdict.FailedGates, ",")
		}
		if a.AttestationPath != "" {
			s += " (signed)"
		}
		return s
	}
	return "-"
}

// mutationText summarises the newest attempt with a mutation report. The
// field names follow the gremlins JSON; anything absent is skipped.
func mutationText(t Task) string {
	for i := len(t.Attempts) - 1; i >= 0; i-- {
		m := t.Attempts[i].Mutation
		if m == nil {
			continue
		}
		eff, hasEff := m["test_efficacy"].(float64)
		total, hasTotal := m["mutants_total"].(float64)
		switch {
		case hasEff && hasTotal:
			return fmt.Sprintf("%.0f%% of %.0f", eff, total)
		case hasEff:
			return fmt.Sprintf("%.0f%%", eff)
		}
		if st, ok := m["status"].(string); ok && st != "" {
			return st
		}
		return "reported"
	}
	return "-"
}

func table(b *strings.Builder, rows [][]string) {
	width := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			if len(c) > width[i] {
				width[i] = len(c)
			}
		}
	}
	for _, r := range rows {
		var line strings.Builder
		for i, c := range r {
			if i == len(r)-1 {
				line.WriteString(c)
			} else {
				line.WriteString(c + strings.Repeat(" ", width[i]-len(c)+2))
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
}
