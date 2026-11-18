package budgetlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
)

var rpNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func rpF(x float64) *float64 { return &x }

func rpSnapshot() budget.Snapshot {
	c := budget.DefaultConfig()
	c.Mode = budget.ModeEnforce
	c.ColdUnitFraction = 0.05
	reset := rpNow.Add(3 * time.Hour).Unix()
	obs := func(id int64, u float64, ago time.Duration) budget.Obs {
		return budget.Obs{ID: id, Pool: "five_hour", Utilization: rpF(u), ResetsAt: &reset, Status: "allowed",
			Source: "rate_limit_event", Documented: false, ObservedAt: rpNow.Add(-ago)}
	}
	cand := budget.Candidate{Key: budget.Key{Run: "r1", Task: "t1", Attempt: 1}, Class: "normal", Models: []string{"sonnet"},
		RunStartedAt: rpNow.Add(-time.Hour), Tier: 1}
	end := rpNow.Add(-30 * time.Minute)
	return budget.Snapshot{
		Now: rpNow, Mode: budget.ModeEnforce, Config: c, ConfigSHA256: c.SHA256(),
		Obs: map[string][]budget.Obs{"five_hour": {obs(1, 0.10, 50*time.Minute), obs(2, 0.12, 30*time.Minute), obs(3, 0.15, time.Minute)}},
		Spans: []budget.Span{{AttemptID: 7, Run: "r0", Task: "x", Model: "sonnet", Start: rpNow.Add(-45 * time.Minute), End: &end,
			Usage: &budget.Tokens{Input: 1000, Output: 2000}}},
		Samples:    map[string][]budget.Sample{"sonnet": {{AttemptID: 7, Model: "sonnet", Usage: budget.Tokens{Input: 1000, Output: 2000}, NumTurns: 3, DurationMS: 1000}}},
		Floors:     map[string]budget.Obs{},
		Candidates: []budget.Candidate{cand},
		Runs:       []budget.RunState{{Run: "r1", Mode: budget.ModeEnforce}},
		For:        cand.Key,
	}
}

// rpWriter builds a budget.jsonl the way the gate does: inputs once, then
// decisions naming rows by id.
type rpWriter struct {
	t   *testing.T
	l   *Log
	eid int
}

func newRPWriter(t *testing.T) (*rpWriter, string) {
	p := filepath.Join(t.TempDir(), "budget.jsonl")
	l, err := Open(p, "r1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return &rpWriter{t: t, l: l}, p
}

func (w *rpWriter) append(task, typ string, data any) {
	w.t.Helper()
	if _, err := w.l.Append(task, typ, data); err != nil {
		w.t.Fatal(err)
	}
}

func (w *rpWriter) input(kind string, id any, row any) {
	w.append("", TypeInput, map[string]any{"kind": kind, "id": id, "row": row})
}

func (w *rpWriter) inputs(s budget.Snapshot) {
	w.input("config", s.ConfigSHA256, s.Config)
	for _, rows := range s.Obs {
		for _, o := range rows {
			w.input("obs", o.ID, o)
		}
	}
	for _, sp := range s.Spans {
		w.input("span", sp.AttemptID, sp)
	}
	for _, ss := range s.Samples {
		for _, sm := range ss {
			w.input("sample", sm.AttemptID, sm)
		}
	}
}

func rpIDs(s budget.Snapshot) (obs, spans, samples []int64) {
	for _, rows := range s.Obs {
		for _, o := range rows {
			obs = append(obs, o.ID)
		}
	}
	for _, sp := range s.Spans {
		spans = append(spans, sp.AttemptID)
	}
	for _, ss := range s.Samples {
		for _, sm := range ss {
			samples = append(samples, sm.AttemptID)
		}
	}
	return
}

func (w *rpWriter) decide(s budget.Snapshot) budget.Decision {
	d := budget.Decide(s)
	w.eid++
	o, sp, sm := rpIDs(s)
	w.append(s.For.Task, TypeDecision, map[string]any{
		"eid": w.eid, "now": s.Now, "mode": s.Mode, "enforced": s.Mode == budget.ModeEnforce, "attempt": s.For.Attempt,
		"config_sha256": s.ConfigSHA256, "obs_ids": o, "span_ids": sp, "sample_ids": sm, "floors": s.Floors,
		"open": s.Open, "recent": s.Recent, "breakers": s.Breakers, "candidates": s.Candidates, "runs": s.Runs,
		"for": s.For, "decision": d, "suppressed": 0,
	})
	return d
}

// rpLog writes one input set and one decision.
func rpLog(t *testing.T) (string, budget.Decision) {
	w, p := newRPWriter(t)
	s := rpSnapshot()
	w.inputs(s)
	d := w.decide(s)
	return p, d
}

// rpEdit rewrites every event of type typ (nth 0-based) with fn on its data
// map and returns the log's events.
func rpEdit(t *testing.T, p, typ string, nth int, fn func(m map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	seen := 0
	for i, ln := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["type"] != typ {
			continue
		}
		if seen++; seen-1 != nth {
			continue
		}
		fn(ev["data"].(map[string]any))
		nb, _ := json.Marshal(ev)
		lines[i] = string(nb)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func rpReplay(t *testing.T, p string) (int, string) {
	t.Helper()
	evs, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	n, drift, err := Replay(evs)
	if err != nil {
		t.Fatal(err)
	}
	return n, drift
}

func TestBudgetReplayCleanLog(t *testing.T) {
	p, d := rpLog(t)
	if !d.Admit {
		t.Fatalf("fixture decision should admit: %s", d.Reason)
	}
	n, drift := rpReplay(t, p)
	if drift != "" || n != 1 {
		t.Fatalf("n=%d drift=%q, want 1 and clean", n, drift)
	}
}

// TestBudgetReplayDriftNamesEID: each tamper of the decision or of an
// earlier input row is drift, and the line names the eid and type.
func TestBudgetReplayDriftNamesEID(t *testing.T) {
	cases := []struct {
		name  string
		typ   string
		nth   int
		edit  func(m map[string]any)
		field string // substring of the first differing field
	}{
		{"flip admit", TypeDecision, 0, func(m map[string]any) {
			m["decision"].(map[string]any)["admit"] = false
		}, "decision.admit"},
		{"pool headroom", TypeDecision, 0, func(m map[string]any) {
			pools := m["decision"].(map[string]any)["pools"].([]any)
			pools[0].(map[string]any)["headroom"] = 0.99
		}, "decision.pools[0].headroom"},
		{"decision now", TypeDecision, 0, func(m map[string]any) {
			m["now"] = rpNow.Add(10 * time.Minute).Format(time.RFC3339Nano)
		}, "decision."},
		{"mode", TypeDecision, 0, func(m map[string]any) { m["mode"] = budget.ModeObserve }, "decision."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _ := rpLog(t)
			rpEdit(t, p, c.typ, c.nth, c.edit)
			_, drift := rpReplay(t, p)
			if !strings.HasPrefix(drift, "budget drift: eid 1 (budget_decision): ") || !strings.Contains(drift, c.field) {
				t.Fatalf("drift = %q, want eid 1 (budget_decision) naming %q", drift, c.field)
			}
		})
	}
	t.Run("obs input utilization", func(t *testing.T) {
		p, d := rpLog(t)
		_ = d
		// Edit the newest obs row's level up: 0.15 -> 0.95 leaves no headroom.
		evs, _ := ReadFile(p)
		nth := -1
		k := 0
		for _, ev := range evs {
			if ev.Type != TypeInput {
				continue
			}
			var in struct {
				Kind string `json:"kind"`
				ID   int64  `json:"id"`
			}
			_ = json.Unmarshal(ev.Data, &in)
			if in.Kind == "obs" && in.ID == 3 {
				nth = k
			}
			k++
		}
		rpEdit(t, p, TypeInput, nth, func(m map[string]any) { m["row"].(map[string]any)["utilization"] = 0.95 })
		_, drift := rpReplay(t, p)
		if !strings.HasPrefix(drift, "budget drift: eid 1 (budget_decision): ") {
			t.Fatalf("edited obs row replayed as %q", drift)
		}
	})
}

// TestBudgetReplayMissingInput: a decision naming an obs/span/sample id
// that no earlier budget_input line carries is drift, including an input
// logged only after the decision.
func TestBudgetReplayMissingInput(t *testing.T) {
	for _, key := range []string{"obs_ids", "span_ids", "sample_ids"} {
		t.Run(key, func(t *testing.T) {
			p, _ := rpLog(t)
			rpEdit(t, p, TypeDecision, 0, func(m map[string]any) { m[key] = append(m[key].([]any), float64(999)) })
			_, drift := rpReplay(t, p)
			if !strings.Contains(drift, "eid 1 (budget_decision): "+key+": id 999 has no earlier budget_input") {
				t.Fatalf("drift = %q", drift)
			}
		})
	}
	t.Run("input after decision", func(t *testing.T) {
		w, p := newRPWriter(t)
		s := rpSnapshot()
		w.input("config", s.ConfigSHA256, s.Config)
		for _, sp := range s.Spans {
			w.input("span", sp.AttemptID, sp)
		}
		for _, sm := range s.Samples["sonnet"] {
			w.input("sample", sm.AttemptID, sm)
		}
		w.decide(s) // names obs ids that are only logged next
		for _, o := range s.Obs["five_hour"] {
			w.input("obs", o.ID, o)
		}
		_, drift := rpReplay(t, p)
		if !strings.Contains(drift, "obs_ids: id") {
			t.Fatalf("drift = %q, want a missing-input drift", drift)
		}
	})
}

// TestBudgetReplayConsistentEditIsNotDetected documents the limit of
// replay: it proves the decision follows from the recorded inputs, not
// that the inputs were true. Editing an input row and re-recording the
// decision that follows from it replays clean.
func TestBudgetReplayConsistentEditIsNotDetected(t *testing.T) {
	w, p := newRPWriter(t)
	s := rpSnapshot()
	rows := s.Obs["five_hour"]
	rows[2].Utilization = rpF(0.95) // the "forged" level, recorded as an input
	s.Obs["five_hour"] = rows
	w.inputs(s)
	d := w.decide(s)
	if d.Admit {
		t.Fatalf("fixture: a 0.95 level should refuse, got admit")
	}
	n, drift := rpReplay(t, p)
	if drift != "" || n != 1 {
		t.Fatalf("consistent edit: n=%d drift=%q, want clean (the documented limit)", n, drift)
	}
}

// TestBudgetReplayLastInputWins: an id re-logged later (a span that ended)
// is used by decisions after it and not by decisions before it.
func TestBudgetReplayLastInputWins(t *testing.T) {
	w, p := newRPWriter(t)
	s := rpSnapshot()
	running := s.Spans[0]
	running.End, running.Usage = nil, nil
	s.Spans = []budget.Span{running}
	w.inputs(s)
	w.decide(s)
	ended := rpSnapshot().Spans[0]
	w.input("span", ended.AttemptID, ended)
	s.Spans = []budget.Span{ended}
	w.decide(s)
	n, drift := rpReplay(t, p)
	if drift != "" || n != 2 {
		t.Fatalf("n=%d drift=%q", n, drift)
	}
}

func rpBreakerInput() budget.BreakerInput {
	c := budget.DefaultConfig()
	c.BreakerRetryStormRetries = 2
	c.BreakerRetryStormWindow = time.Hour
	at := func(m int) time.Time { return rpNow.Add(time.Duration(-m) * time.Minute) }
	return budget.BreakerInput{Now: rpNow, Mode: budget.ModeEnforce, Config: c, Run: "r1", Task: "t1",
		Attempts: []budget.AttemptFact{
			{AttemptID: 1, Task: "a", StartedAt: at(30)}, {AttemptID: 2, Task: "a", StartedAt: at(20)},
			{AttemptID: 3, Task: "a", StartedAt: at(10)},
		}}
}

func TestBudgetReplayBreaker(t *testing.T) {
	in := rpBreakerInput()
	changes := budget.EvalBreakers(in)
	var storm *budget.BreakerChange
	for i := range changes {
		if changes[i].Breaker.Kind == budget.RetryStorm {
			storm = &changes[i]
		}
	}
	if storm == nil {
		t.Fatalf("fixture: no RETRY_STORM in %+v", changes)
	}
	w, p := newRPWriter(t)
	w.append("", TypeBreaker, map[string]any{"eid": 1, "action": storm.Action, "breaker": storm.Breaker, "inputs": in})
	if n, drift := rpReplay(t, p); drift != "" || n != 0 {
		t.Fatalf("clean breaker: n=%d drift=%q", n, drift)
	}
	rpEdit(t, p, TypeBreaker, 0, func(m map[string]any) { m["breaker"].(map[string]any)["count"] = 99 })
	if _, drift := rpReplay(t, p); !strings.HasPrefix(drift, "budget drift: eid 1 (budget_breaker): breaker.count") {
		t.Fatalf("tampered breaker: %q", drift)
	}
	rpEdit(t, p, TypeBreaker, 0, func(m map[string]any) { m["action"] = "close" })
	if _, drift := rpReplay(t, p); !strings.Contains(drift, "(budget_breaker): action") {
		t.Fatalf("wrong action: %q", drift)
	}
}

func TestBudgetReplayFit(t *testing.T) {
	s := rpSnapshot()
	var iv []budget.Interval
	for i := 0; i < 6; i++ {
		tok := float64(100000 * (i + 1))
		from := rpNow.Add(time.Duration(-60+i*10) * time.Minute)
		iv = append(iv, budget.Interval{From: from, To: from.Add(10 * time.Minute), DeltaU: tok * 1e-6, Tokens: tok,
			ByClass: budget.Tokens{Input: tok}, ByModel: map[string]float64{"sonnet": tok}, Complete: true})
	}
	fit := budget.FitRatio("five_hour", iv, s.Config)
	w, p := newRPWriter(t)
	w.input("config", s.ConfigSHA256, s.Config)
	w.append("", TypeFit, map[string]any{"eid": 1, "pool": "five_hour", "intervals": iv, "fit": fit})
	if _, drift := rpReplay(t, p); drift != "" {
		t.Fatalf("clean fit: %q", drift)
	}
	rpEdit(t, p, TypeFit, 0, func(m map[string]any) { m["fit"].(map[string]any)["k_per_token"] = 0.5 })
	if _, drift := rpReplay(t, p); !strings.HasPrefix(drift, "budget drift: eid 1 (budget_fit): fit.k_per_token") {
		t.Fatalf("tampered fit: %q", drift)
	}
}
