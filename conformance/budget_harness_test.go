// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// budget_harness_test.go: helpers for the budget tests.
// Black box like the rest of the suite: the tests drive `gosplan run`, `gosplan budget`,
// `gosplan replay` and `gosplan status`, read gosplan.db with the sqlite3 CLI, read
// <state>/runs/<run>/budget.jsonl and the
// fake's ledger. The fake emits rate_limit_event / api_retry lines and result
// usage from the scenario. Nothing spends plan usage.
package conformance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bgConfig writes a budget.toml in enforce mode with short
// rechecks and no jitter, plus top (more [budget] keys) and tables (more
// [budget.*] tables). Returns its path. Enforce mode has no default
// cold.unit_fraction, so when tables has no [budget.cold]
// table this writes the test's own policy value, 0.05.
func bgConfig(t *testing.T, top, tables string) string {
	t.Helper()
	dir := t.TempDir()
	if !strings.Contains(tables, "[budget.cold]") {
		tables = "[budget.cold]\nunit_fraction = 0.05\n" + tables
	}
	writeFile(t, dir, "budget.toml", "[budget]\nmode = \"enforce\"\njitter = \"0s\"\nstale_recheck = \"1s\"\n"+
		"fairness_recheck = \"1s\"\nrecheck_max = \"2s\"\n"+top+"\n"+tables)
	return filepath.Join(dir, "budget.toml")
}

// bgSidecar appends lines to the sidecar of task id in f's spec.
func bgSidecar(t *testing.T, specDir, id, extra string) {
	t.Helper()
	p := filepath.Join(specDir, "prompts", id+".task.toml")
	writeFile(t, specDir, "prompts/"+id+".task.toml", readFile(t, p)+extra)
}

// bgPool is one pool entry of a decision (budget.PoolResult) or of `gosplan budget`.
type bgPool struct {
	Pool         string   `json:"pool"`
	Level        *float64 `json:"level"`
	ObservedAt   *string  `json:"observed_at"`
	AgeSeconds   *float64 `json:"age_seconds"`
	Source       string   `json:"source"`
	Documented   *bool    `json:"documented"`
	Stale        bool     `json:"stale"`
	StaleReason  string   `json:"stale_reason"`
	ResetsAt     *int64   `json:"resets_at"`
	Projected    *float64 `json:"projected"`
	Headroom     *float64 `json:"headroom"`
	OpenReserved *float64 `json:"open_reserved"`
	Hold         *float64 `json:"hold"`
	Reserve      *float64 `json:"reserve"`
	ReserveBasis string   `json:"reserve_basis"`
	Fits         *bool    `json:"fits"`
}

// bgDecision is the data of one budget_decision event.
type bgDecision struct {
	EID       int64            `json:"eid"`
	ConfigSHA string           `json:"config_sha256"`
	ObsIDs    []int64          `json:"obs_ids"`
	SpanIDs   []int64          `json:"span_ids"`
	SampleIDs []int64          `json:"sample_ids"`
	Open      []map[string]any `json:"open"`
	Enforced  bool             `json:"enforced"`
	Mode      string           `json:"mode"`
	Attempt   int              `json:"attempt"`
	Decision  struct {
		For struct {
			Run     string `json:"run"`
			Task    string `json:"task"`
			Attempt int    `json:"attempt"`
		} `json:"for"`
		Admit      bool     `json:"admit"`
		Model      string   `json:"model"`
		Rule       string   `json:"rule"`
		Reason     string   `json:"reason"`
		Breaker    string   `json:"breaker"`
		StalePools []string `json:"stale_pools"`
		AdmitAt    *string  `json:"admit_at"`
		NotBefore  *string  `json:"not_before"`
		Pools      []bgPool `json:"pools"`
	} `json:"decision"`
	ts string
}

func (d bgDecision) pool(name string) (bgPool, bool) {
	for _, p := range d.Decision.Pools {
		if p.Pool == name {
			return p, true
		}
	}
	return bgPool{}, false
}

// bgLogPath is <state>/runs/<run>/budget.jsonl.
func bgLogPath(f *runFix, run string) string {
	return filepath.Join(f.state, "runs", run, "budget.jsonl")
}

// bgLog reads a run's budget.jsonl strictly (same envelope as events.jsonl).
func bgLog(t *testing.T, f *runFix, run string) []event {
	t.Helper()
	p := bgLogPath(f, run)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("budget.jsonl missing for run %s: %v", run, err)
	}
	return readEvents(t, p)
}

// bgDecisions returns the budget_decision events of evs for task ("" = all), in order.
func bgDecisions(t *testing.T, evs []event, task string) []bgDecision {
	t.Helper()
	var out []bgDecision
	for _, ev := range evs {
		if ev.Type != "budget_decision" {
			continue
		}
		var d bgDecision
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("budget_decision seq %d: %v: %s", ev.Seq, err, ev.Data)
		}
		d.ts = ev.TS
		if task == "" || d.Decision.For.Task == task {
			out = append(out, d)
		}
	}
	return out
}

// bgOfType returns the data of every event of type typ, in order.
func bgOfType(evs []event, typ string) []map[string]any {
	var out []map[string]any
	for _, ev := range evs {
		if ev.Type != typ {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(ev.Data, &m)
		m["_ts"] = ev.TS
		out = append(out, m)
	}
	return out
}

// bgSpan is one worker process from the fake's ledger.
type bgSpan struct {
	task, attempt, model string
	start, end           int64 // ms; end 0 = no end record
}

// bgSpans pairs the ledger's start and end records by pid.
func bgSpans(f *runFix) []bgSpan {
	byPID := map[int]*bgSpan{}
	var order []int
	for _, r := range f.ledger() {
		switch r.Event {
		case "start":
			byPID[r.PID] = &bgSpan{task: r.Task, attempt: r.Attempt, model: r.Model, start: r.TMS}
			order = append(order, r.PID)
		case "end":
			if s := byPID[r.PID]; s != nil {
				s.end = r.TMS
			}
		}
	}
	var out []bgSpan
	for _, pid := range order {
		out = append(out, *byPID[pid])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// bgMaxOverlap is the largest number of worker spans alive at one instant.
func bgMaxOverlap(spans []bgSpan) int {
	type pt struct {
		t int64
		d int
	}
	var pts []pt
	for _, s := range spans {
		end := s.end
		if end == 0 {
			end = 1 << 62
		}
		pts = append(pts, pt{s.start, 1}, pt{end, -1})
	}
	sort.Slice(pts, func(i, j int) bool {
		if pts[i].t != pts[j].t {
			return pts[i].t < pts[j].t
		}
		return pts[i].d < pts[j].d // an end at the same ms closes first
	})
	n, best := 0, 0
	for _, p := range pts {
		n += p.d
		if n > best {
			best = n
		}
	}
	return best
}

func bgFirstStart(spans []bgSpan, task string) (int64, bool) {
	for _, s := range spans {
		if s.task == task {
			return s.start, true
		}
	}
	return 0, false
}

func bgParseTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	return ts
}

// bgParkFix: 1.a reports five_hour at 99% (rateLimitType + utilization)
// resetting in reset seconds, and seven_day at 10% (unifiedWindows); 2.a
// depends on 1.a. With one 5% unit reservation, 2.a must wait for the reset.
func bgParkFix(t *testing.T, reset int) (*runFix, string) {
	t.Helper()
	f := newRunFix(t, "1.a -> 2.a", nil)
	f.scenario(map[string][]map[string]any{
		"1.a": {{"op": "rate_limit", "window": "five_hour", "utilization": 0.99, "resets_in": reset,
			"unified": map[string]any{"seven_day": map[string]any{"utilization": 0.10, "resets_in": 86400}}},
			{"op": "commit"}},
	})
	return f, bgConfig(t, "stale_after = \"10m\"\n", "[budget.cold]\nunit_fraction = 0.05\n")
}

// bgStartWatched starts `gosplan run` like startNR and also reports its exit.
func bgStartWatched(f *runFix, extra ...string) (*exec.Cmd, <-chan struct{}) {
	cmd := f.startNR(extra...)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	return cmd, done
}

func bgExited(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func bgStop(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }

// bgJSON runs gosplan with args and decodes stdout as a JSON object.
func bgJSON(t *testing.T, f *runFix, what string, args ...string) map[string]any {
	t.Helper()
	res := gosplan(t, f.project, f.env(), args...)
	if res.code != 0 {
		t.Fatalf("%s: want exit 0, got %s", what, res)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &m); err != nil {
		t.Fatalf("%s: stdout is not a JSON object: %v\n%s", what, err, res)
	}
	return m
}

func bgList(m map[string]any, key string) []map[string]any {
	xs, _ := m[key].([]any)
	var out []map[string]any
	for _, x := range xs {
		if xm, ok := x.(map[string]any); ok {
			out = append(out, xm)
		}
	}
	return out
}

func bgNum(m map[string]any, key string) (float64, bool) {
	v, ok := m[key].(float64)
	return v, ok
}

var bgBudgetType = regexp.MustCompile(`^budget_(input|decision|settle|breaker|fit|probe|halt)$`)

// bgOtherRun returns the run id under f.state/runs that is not known.
func bgOtherRun(t *testing.T, f *runFix, known string) string {
	t.Helper()
	for _, id := range f.runIDs() {
		if id != known {
			return id
		}
	}
	t.Fatalf("no second run dir under %s/runs (have %v)", f.state, f.runIDs())
	return ""
}

func bgContainsAll(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, s := range subs {
		if !strings.Contains(low, strings.ToLower(s)) {
			t.Fatalf("%s lacks %q:\n%s", what, s, clip(text))
		}
	}
}
