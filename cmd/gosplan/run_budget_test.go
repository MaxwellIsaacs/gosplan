package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetrt"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// End-to-end budget tests: the built gosplan with
// the conformance fake claude, fixtures under testdata/budget-spec. Every
// config uses jitter 0s and 1–2 s rechecks; no test asserts an exact sleep.

// bgFix copies testdata/budget-spec, sets its sequence and appends extra
// sidecar lines per task.
func bgFix(t *testing.T, seq string, sidecars map[string]string) *runFix {
	t.Helper()
	f := runNewFix(t, "budget-spec")
	write(t, f.spec, "prompts/sequence.txt", seq+"\n")
	for id, extra := range sidecars {
		p := filepath.Join(f.spec, "prompts", id+".task.toml")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		write(t, f.spec, "prompts/"+id+".task.toml", string(b)+extra)
	}
	return f
}

// bgCfg writes an enforce-mode budget.toml with short rechecks and no
// jitter; body adds keys and tables (a [budget.cold] table is added when
// body has none, with the test's own unit_fraction 0.05).
func bgCfg(t *testing.T, top, tables string) string {
	t.Helper()
	if !strings.Contains(tables, "[budget.cold]") {
		tables = "[budget.cold]\nunit_fraction = 0.05\n" + tables
	}
	dir := t.TempDir()
	write(t, dir, "budget.toml", "[budget]\nmode = \"enforce\"\njitter = \"0s\"\nstale_recheck = \"1s\"\n"+
		"fairness_recheck = \"1s\"\nrecheck_max = \"2s\"\n"+top+"\n"+tables)
	return filepath.Join(dir, "budget.toml")
}

// brun runs gosplan run with the probe and mutation off plus extra flags.
func (f *runFix) brun(extra ...string) runResult {
	f.t.Helper()
	return f.run(append([]string{"--usage-probe=false", "--mutation", "off"}, extra...)...)
}

func (f *runFix) scenario(task string, steps []map[string]any) {
	runScenario(f.t, f.scn, task, map[string][]map[string]any{"*": steps})
}

// bgEvents reads the latest run's budget.jsonl strictly.
func bgEvents(t *testing.T, f *runFix) []budgetlog.Event {
	t.Helper()
	evs, err := budgetlog.ReadFile(filepath.Join(f.runDir(), "budget.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := budgetlog.Validate(evs); err != nil {
		t.Fatal(err)
	}
	return evs
}

type bgDec struct {
	budgetrt.DecisionEvent
	EID int64 `json:"eid"`
}

func bgDecisions(t *testing.T, evs []budgetlog.Event, task string) []bgDec {
	t.Helper()
	var out []bgDec
	for _, ev := range evs {
		if ev.Type != budgetlog.TypeDecision {
			continue
		}
		var d bgDec
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("decision seq %d: %v", ev.Seq, err)
		}
		if task == "" || d.For.Task == task {
			out = append(out, d)
		}
	}
	return out
}

func bgOfType(evs []budgetlog.Event, typ string) []map[string]any {
	var out []map[string]any
	for _, ev := range evs {
		if ev.Type == typ {
			var m map[string]any
			_ = json.Unmarshal(ev.Data, &m)
			out = append(out, m)
		}
	}
	return out
}

// bgSpan is one fake worker process from the ledger.
type bgSpan struct {
	task, attempt, model string
	argv                 []string
	start, end           int64
}

func bgSpans(t *testing.T, f *runFix) []bgSpan {
	t.Helper()
	fh, err := os.Open(filepath.Join(f.scn, "ledger.jsonl"))
	if err != nil {
		return nil
	}
	defer fh.Close()
	byPID := map[float64]*bgSpan{}
	var order []float64
	sc := bufio.NewScanner(fh)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r struct {
			Event, Task, Attempt, Model string
			PID                         float64  `json:"pid"`
			TMS                         int64    `json:"t_ms"`
			Argv                        []string `json:"argv"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.Event {
		case "start":
			byPID[r.PID] = &bgSpan{task: r.Task, attempt: r.Attempt, model: r.Model, argv: r.Argv, start: r.TMS}
			order = append(order, r.PID)
		case "end":
			if s := byPID[r.PID]; s != nil {
				s.end = r.TMS
			}
		}
	}
	var out []bgSpan
	for _, p := range order {
		out = append(out, *byPID[p])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

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
		return pts[i].d < pts[j].d
	})
	n, best := 0, 0
	for _, p := range pts {
		n += p.d
		best = max(best, n)
	}
	return best
}

func bgStarts(spans []bgSpan, task string) []bgSpan {
	var out []bgSpan
	for _, s := range spans {
		if s.task == task {
			out = append(out, s)
		}
	}
	return out
}

// bgReplay recomputes every decision of evs with budget.Decide on the
// snapshot rebuilt from its budget_input lines.
func bgReplay(t *testing.T, evs []budgetlog.Event) {
	t.Helper()
	n, err := budgetrt.Replay(evs)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if want := len(bgDecisions(t, evs, "")); n != want {
		t.Fatalf("replay recomputed %d of %d decisions", n, want)
	}
}

func bgPool(d bgDec, name string) (map[string]any, bool) {
	b, _ := json.Marshal(d.Decision.Pools)
	var ps []map[string]any
	_ = json.Unmarshal(b, &ps)
	for _, p := range ps {
		if p["pool"] == name {
			return p, true
		}
	}
	return nil, false
}

func (f *runFix) task(id string) state.TaskRow {
	f.t.Helper()
	tk, err := f.store().GetTask(context.Background(), f.runID(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return tk
}

func (f *runFix) sql1(q string, args ...any) string {
	f.t.Helper()
	var s sqlNullString
	if err := f.store().DB().QueryRow(q, args...).Scan(&s); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

type sqlNullString struct{ String string }

func (n *sqlNullString) Scan(v any) error {
	if v != nil {
		n.String = fmt.Sprint(v)
		if b, ok := v.([]byte); ok {
			n.String = string(b)
		}
	}
	return nil
}

// TestBudgetE2EParkUntilReset: enforce mode, five_hour at 99% resetting in
// 6 s: 2.a is refused (rule headroom, every input number in the event),
// parked through the reconciler's txn (the run never halts state_tampered),
// rechecked without re-logging identical refusals, and started after the
// reset under the stale rule. Every decision replays.
func TestBudgetE2EParkUntilReset(t *testing.T) {
	f := bgFix(t, "1.a -> 2.a", nil)
	f.scenario("1.a", []map[string]any{{"op": "rate_limit", "window": "five_hour", "utilization": 0.99, "resets_in": 6,
		"unified": map[string]any{"seven_day": map[string]any{"utilization": 0.10, "resets_in": 86400}}}, {"op": "commit"}})
	res := f.brun("--budget-config", bgCfg(t, "stale_after = \"10m\"\n", ""))
	f.mustExit(res, 0)
	evs := bgEvents(t, f)
	ds := bgDecisions(t, evs, "2.a")
	if len(ds) < 2 {
		t.Fatalf("2.a: want a refusal and a later admit, got %d decisions", len(ds))
	}
	first, last := ds[0], ds[len(ds)-1]
	if first.Decision.Admit || first.Decision.Rule != "headroom" || !first.Enforced || !strings.Contains(first.Decision.Reason, "five_hour") {
		t.Fatalf("2.a first decision: want an enforced headroom refusal naming five_hour, got %+v", first.Decision)
	}
	p, ok := bgPool(first, "five_hour")
	if !ok || p["level"] != 0.99 || p["reserve"] != 0.05 || p["reserve_basis"] != "unit" || p["fits"] != false || p["documented"] != false {
		t.Fatalf("five_hour numbers: %v", p)
	}
	if sd, ok := bgPool(first, "seven_day"); !ok || sd["fits"] != true {
		t.Fatalf("seven_day at 10%% must be checked and fit: %v", sd)
	}
	if first.Decision.NotBefore == nil || first.Decision.AdmitAt == nil {
		t.Fatal("a refusal carries admit_at and not_before")
	}
	reset := int64(p["resets_at"].(float64))
	if !last.Decision.Admit || !strings.Contains(strings.Join(last.Decision.StalePools, ","), "five_hour") {
		t.Fatalf("2.a last decision: want admitted with five_hour stale (past_reset), got %+v", last.Decision)
	}
	if last.Suppressed < 1 {
		t.Fatalf("identical refusals while parked must be suppressed, the admit line says suppressed=%d (%d lines)", last.Suppressed, len(ds))
	}
	st := bgStarts(bgSpans(t, f), "2.a")
	if len(st) != 1 || st[0].start < reset*1000 {
		t.Fatalf("2.a must start once, after the reset %d s: %+v", reset, st)
	}
	if tk := f.task("2.a"); tk.Status != "passed" {
		t.Fatalf("2.a: %s", tk.Status)
	}
	bgReplay(t, evs)
}

// TestBudgetE2EBootstrapOneAtATime: enforce mode with no observation at
// all admits exactly one attempt at a time and never deadlocks.
func TestBudgetE2EBootstrapOneAtATime(t *testing.T) {
	f := bgFix(t, "(1.a || 1.b || 1.c)", nil)
	work := []map[string]any{{"op": "sleep", "seconds": 2, "heartbeat": true}, {"op": "commit"}}
	for _, id := range []string{"1.a", "1.b", "1.c"} {
		f.scenario(id, work)
	}
	f.mustExit(f.brun("--budget-config", bgCfg(t, "", "")), 0)
	spans := bgSpans(t, f)
	if n := bgMaxOverlap(spans); n != 1 || len(spans) != 3 {
		t.Fatalf("bootstrap: want 3 workers one at a time, got %d spans, max overlap %d", len(spans), n)
	}
	refused := 0
	for _, d := range bgDecisions(t, bgEvents(t, f), "") {
		if !d.Decision.Admit {
			refused++
			if d.Decision.Rule != "bootstrap" || !d.Enforced {
				t.Fatalf("with no observation a refusal must be an enforced bootstrap one: %+v", d.Decision)
			}
		}
	}
	if refused == 0 {
		t.Fatal("no bootstrap refusal recorded")
	}
}

// TestBudgetE2EObserveByDefault: no budget config: observe mode, said on
// stderr; nothing enforced (two workers at once) and every decision logged
// enforced=false, including the bootstrap refusal enforce would make.
func TestBudgetE2EObserveByDefault(t *testing.T) {
	f := bgFix(t, "(1.a || 1.b)", nil)
	work := []map[string]any{{"op": "sleep", "seconds": 2, "heartbeat": true}, {"op": "commit"}}
	f.scenario("1.a", work)
	f.scenario("1.b", work)
	res := f.brun()
	f.mustExit(res, 0)
	if !strings.Contains(res.stderr, "; observe mode (decisions logged, not enforced)") {
		t.Fatalf("no observe-mode notice:\n%s", res.stderr)
	}
	if n := bgMaxOverlap(bgSpans(t, f)); n < 2 {
		t.Fatalf("observe mode must not enforce: max overlap %d", n)
	}
	ds := bgDecisions(t, bgEvents(t, f), "")
	wouldRefuse := false
	for _, d := range ds {
		if d.Enforced || d.Mode != "observe" {
			t.Fatalf("observe decision: enforced=%v mode=%s", d.Enforced, d.Mode)
		}
		wouldRefuse = wouldRefuse || (!d.Decision.Admit && d.Decision.Rule == "bootstrap")
	}
	if len(ds) < 2 || !wouldRefuse {
		t.Fatalf("want a decision per dispatch including the would-be bootstrap refusal, got %d", len(ds))
	}
}

// TestBudgetE2ESettle: a reservation is settled from the result line's
// usage (sum of the four classes); an attempt with no result line (killed
// as stalled) settles unknown.
func TestBudgetE2ESettle(t *testing.T) {
	f := bgFix(t, "(1.a || 1.b)", nil)
	f.scenario("1.a", []map[string]any{{"op": "usage", "input": 1000, "output": 2000, "cache_read": 3000, "cache_create": 4000}, {"op": "commit"}})
	// 1.b's first attempt hangs silently and is killed as stalled: no result line.
	runScenario(t, f.scn, "1.b", map[string][]map[string]any{"1": {{"op": "hang"}}, "*": runCommitSteps("1.b", "ok\n")})
	f.mustExit(f.brun(), 0)
	got := f.sql1(`SELECT group_concat(task_id||'|'||state||'|'||ifnull(actual_tokens,'-')||'|'||actual_basis, ' ') FROM (SELECT r.* FROM budget_reservations r JOIN attempts a ON a.id = r.attempt_id ORDER BY r.task_id, a.n)`)
	if got != "1.a|settled|10000|usage 1.b|settled|-|unknown 1.b|settled|6|usage" {
		t.Fatalf("reservations: %s", got)
	}
	if n := len(bgOfType(bgEvents(t, f), budgetlog.TypeSettle)); n != 3 {
		t.Fatalf("want 3 budget_settle events, got %d", n)
	}
}

// TestBudgetE2ERateLimitedHolds: a rejected rate_limit_event opens
// RATE_LIMITED: no new attempt starts before the observed reset, and the
// worker that saw it is not killed.
func TestBudgetE2ERateLimitedHolds(t *testing.T) {
	f := bgFix(t, "1.a -> (2.a || 2.b)", nil)
	f.scenario("1.a", []map[string]any{{"op": "rate_limit", "window": "five_hour", "status": "rejected", "resets_in": 5},
		{"op": "sleep", "seconds": 1, "heartbeat": true}, {"op": "commit"}})
	f.mustExit(f.brun("--budget-config", bgCfg(t, "", "")), 0)
	spans := bgSpans(t, f)
	if n := len(bgStarts(spans, "1.a")); n != 1 {
		t.Fatalf("1.a started %d times: the worker that saw the limit must not be killed", n)
	}
	reset := int64(0)
	fmt.Sscan(f.sql1(`SELECT resets_at FROM window_observations WHERE status = 'rejected'`), &reset)
	for _, id := range []string{"2.a", "2.b"} {
		st := bgStarts(spans, id)
		if len(st) == 0 || st[0].start < reset*1000 {
			t.Fatalf("%s started before the reset %d while RATE_LIMITED was open: %+v", id, reset, st)
		}
	}
	evs := bgEvents(t, f)
	acts := map[string]bool{}
	for _, b := range bgOfType(evs, budgetlog.TypeBreaker) {
		if br, _ := b["breaker"].(map[string]any); br["kind"] == "RATE_LIMITED" {
			acts[b["action"].(string)] = true
		}
	}
	held := false
	for _, d := range bgDecisions(t, evs, "") {
		held = held || (!d.Decision.Admit && d.Decision.Breaker == "RATE_LIMITED")
	}
	if !acts["open"] || !acts["close"] || !held {
		t.Fatalf("want RATE_LIMITED open, close and a refusal under it: %v held=%v", acts, held)
	}
	bgReplay(t, evs)
}

// TestBudgetE2ESameFailureBlocks: three attempts with the same signature
// (no commit) block the task at 3 although attempts remain.
func TestBudgetE2ESameFailureBlocks(t *testing.T) {
	f := bgFix(t, "1.a", nil)
	f.scenario("1.a", []map[string]any{{"op": "result", "text": "I did nothing"}})
	f.mustExit(f.brun("--budget-config", bgCfg(t, "", ""), "--max-attempts", "5"), 1)
	if n := len(bgStarts(bgSpans(t, f), "1.a")); n != 3 {
		t.Fatalf("SAME_FAILURE: want 3 attempts, got %d", n)
	}
	if tk := f.task("1.a"); tk.Status != "blocked" || !strings.HasPrefix(tk.BlockedReason, "SAME_FAILURE: 3 attempts with no-commit") {
		t.Fatalf("1.a: %s %q", tk.Status, tk.BlockedReason)
	}
}

// TestBudgetE2EVerifierErrorHalts: --max-verify-errors 2 halts the run
// verifier_error in observe mode (no config); the gate logs a
// VERIFIER_ERROR breaker open event; the dependent task never starts.
func TestBudgetE2EVerifierErrorHalts(t *testing.T) {
	f := bgFix(t, "(1.a || 1.b) -> 2.a", nil)
	broken := filepath.Join(t.TempDir(), "gosplan-broken")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\necho 'verifier unavailable' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := f.brun("--max-verify-errors", "2", "--gosplan-bin", broken)
	f.mustExit(res, 1)
	if !strings.Contains(res.stderr, "verifier_error") {
		t.Fatalf("want a verifier_error halt:\n%s", res.stderr)
	}
	if n := len(bgStarts(bgSpans(t, f), "2.a")); n != 0 {
		t.Fatal("2.a started after the verifier broke")
	}
	opened := false
	for _, b := range bgOfType(bgEvents(t, f), budgetlog.TypeBreaker) {
		br, _ := b["breaker"].(map[string]any)
		opened = opened || (br["kind"] == "VERIFIER_ERROR" && b["action"] == "open")
	}
	if !opened {
		t.Fatal("no VERIFIER_ERROR open event in budget.jsonl")
	}
}

// TestBudgetE2ERetryStormCooldown: two retries within the window open
// RETRY_STORM; no attempt of the run starts within the cooldown.
func TestBudgetE2ERetryStormCooldown(t *testing.T) {
	f := bgFix(t, "(1.a || 1.b)", nil)
	fail := []map[string]any{{"op": "exit", "code": 3}}
	runScenario(t, f.scn, "1.a", map[string][]map[string]any{"1": fail, "2": fail, "*": runCommitSteps("1.a", "ok\n")})
	runScenario(t, f.scn, "1.b", map[string][]map[string]any{"1": fail, "2": fail, "*": runCommitSteps("1.b", "ok\n")})
	cfg := bgCfg(t, "", "[budget.breaker.retry_storm]\nretries = 2\nwindow = \"1m\"\ncooldown = \"4s\"\n")
	f.mustExit(f.brun("--budget-config", cfg, "--max-attempts", "4"), 0)
	evs := bgEvents(t, f)
	var openAt time.Time
	for _, ev := range evs {
		if ev.Type != budgetlog.TypeBreaker || !openAt.IsZero() {
			continue
		}
		var b budgetrt.BreakerEvent
		_ = json.Unmarshal(ev.Data, &b)
		if b.Breaker.Kind == "RETRY_STORM" && b.Action == "open" {
			openAt = b.Breaker.OpenedAt.UTC()
		}
	}
	if openAt.IsZero() {
		t.Fatal("two retries within 1m never opened RETRY_STORM")
	}
	// The rule is on admissions: no attempt of the run is admitted while the
	// breaker is open (a worker admitted just before it opened may still be
	// spawning, so process start times are not the measure).
	later, refused := 0, false
	for _, d := range bgDecisions(t, evs, "") {
		if !d.Now.After(openAt) {
			continue
		}
		if d.Decision.Admit {
			later++
			if d.Now.Before(openAt.Add(4 * time.Second)) {
				t.Fatalf("%s attempt %d admitted %s after RETRY_STORM opened (cooldown 4s)", d.For.Task, d.For.Attempt, d.Now.Sub(openAt))
			}
		} else if d.Decision.Breaker == "RETRY_STORM" {
			refused = true
		}
	}
	if later == 0 || !refused {
		t.Fatalf("want refusals under RETRY_STORM and admits after the cooldown, got %d later admits, refused=%v", later, refused)
	}
}

// TestBudgetE2EDeadlineStops: at --deadline the live worker is killed, the
// attempt is refunded (not consumed), the run ends halted "deadline: parked
// …" and both tasks stay pending.
func TestBudgetE2EDeadlineStops(t *testing.T) {
	f := bgFix(t, "1.a -> 2.a", nil)
	f.scenario("1.a", []map[string]any{{"op": "sleep", "seconds": 40, "heartbeat": true}, {"op": "commit"}})
	t0 := time.Now()
	res := f.brun("--deadline", t0.Add(5*time.Second).UTC().Format(time.RFC3339))
	f.mustExit(res, 1)
	if el := time.Since(t0); el > 35*time.Second {
		t.Fatalf("the run went on %s past a 5s deadline", el)
	}
	if !strings.Contains(res.stderr, "halted (deadline: parked 1.a, 2.a)") {
		t.Fatalf("want halted (deadline: parked 1.a, 2.a):\n%s", res.stderr)
	}
	for _, id := range []string{"1.a", "2.a"} {
		if tk := f.task(id); tk.Status != "pending" || tk.AttemptsUsed != 0 {
			t.Fatalf("%s after the deadline: want pending with no attempt consumed, got %s used=%d", id, tk.Status, tk.AttemptsUsed)
		}
	}
	if o := f.sql1(`SELECT outcome FROM attempts WHERE task_id = '1.a'`); o != "killed" {
		t.Fatalf("1.a attempt outcome: %s", o)
	}
	halts := bgOfType(bgEvents(t, f), budgetlog.TypeHalt)
	if len(halts) == 0 || halts[len(halts)-1]["reason"] != "deadline" || fmt.Sprint(halts[len(halts)-1]["parked"]) != "[1.a 2.a]" {
		t.Fatalf("budget_halt events: %v", halts)
	}
}

// TestBudgetE2ECapUSDDrains: --cap-usd-estimate drains the run once the
// settled cost estimates reach it (no no_progress halt); later tasks stay
// pending.
func TestBudgetE2ECapUSDDrains(t *testing.T) {
	f := bgFix(t, "1.a -> 2.a -> 3.a", nil)
	res := f.brun("--cap-usd-estimate", "0.0015")
	f.mustExit(res, 1)
	if !strings.Contains(res.stderr, "halted (cap:usd_estimate: parked 3.a)") || strings.Contains(res.stderr, "no_progress") {
		t.Fatalf("want halted (cap:usd_estimate: parked 3.a):\n%s", res.stderr)
	}
	if n := len(bgStarts(bgSpans(t, f), "3.a")); n != 0 {
		t.Fatal("3.a started after the cap")
	}
	if tk := f.task("3.a"); tk.Status != "pending" {
		t.Fatalf("3.a: %s", tk.Status)
	}
}

// TestBudgetE2EBeltArgv: --max-budget-usd (p90 cost estimate x factor,
// rounded up to the cent) is passed in enforce mode with belt.usd_factor and
// cost history only: never in observe mode, never without the factor.
func TestBudgetE2EBeltArgv(t *testing.T) {
	f := bgFix(t, "(1.a || 1.b || 1.c || 2.a || 2.b)", nil)
	for i, id := range []string{"1.a", "1.b", "1.c", "2.a", "2.b"} {
		f.scenario(id, []map[string]any{{"op": "usage", "cost": 0.1 * float64(i+1)}, {"op": "commit"}})
	}
	f.mustExit(f.brun(), 0)
	for _, s := range bgSpans(t, f) {
		if strings.Contains(strings.Join(s.argv, " "), "--max-budget-usd") {
			t.Fatalf("observe mode passed --max-budget-usd: %v", s.argv)
		}
	}
	for _, tc := range []struct{ tables, want string }{
		{"[budget.estimate]\nmin_samples = 5\n[budget.belt]\nusd_factor = 2.0\n", "--max-budget-usd 1.00"},
		{"[budget.estimate]\nmin_samples = 5\n", ""},
	} {
		g := bgFix(t, "3.a", nil)
		g.state = f.state
		g.mustExit(g.brun("--budget-config", bgCfg(t, "", tc.tables)), 0)
		st := bgStarts(bgSpans(t, g), "3.a")
		if len(st) != 1 {
			t.Fatalf("3.a starts: %+v", st)
		}
		argv := strings.Join(st[0].argv, " ")
		if tc.want != "" && !strings.Contains(argv, tc.want) {
			t.Fatalf("want %q in argv: %s", tc.want, argv)
		}
		if tc.want == "" && strings.Contains(argv, "--max-budget-usd") {
			t.Fatalf("no usd_factor: no belt, argv %s", argv)
		}
	}
}

// TestBudgetE2ESharedStateDir: two gosplan run processes on one state dir. With
// five_hour at 88% (two 5% reservations of headroom), run A holds two open
// reservations; run B's task is refused for headroom counting them, and
// starts only once five_hour is stale. A's settles re-arm B's park; neither
// run halts state_tampered.
func TestBudgetE2ESharedStateDir(t *testing.T) {
	f := bgFix(t, "(1.a || 1.b)", nil)
	work := []map[string]any{{"op": "sleep", "seconds": 6, "heartbeat": true}, {"op": "commit"}}
	f.scenario("1.a", work)
	f.scenario("1.b", work)
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(f.state, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	u, reset := 0.88, time.Now().Add(time.Hour).Unix()
	if err := st.RecordWindows(context.Background(), []state.WindowObservation{{Window: "five_hour", Utilization: &u, ResetsAt: &reset,
		Status: "allowed", Source: "rate_limit_event", ObservedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	cfg := bgCfg(t, "stale_after = \"12s\"\n", "[budget.class.critical]\nreserve_fraction = 0.0\n[budget.class.normal]\nreserve_fraction = 0.0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmdA := exec.CommandContext(ctx, f.bins.gosplan, f.args("--usage-probe=false", "--mutation", "off", "--budget-config", cfg)...)
	cmdA.Dir = f.project
	cmdA.Env = append(os.Environ(), "CONF_FAKE_DIR="+f.scn)
	var errA bytes.Buffer
	cmdA.Stderr = &errA
	if err := cmdA.Start(); err != nil {
		t.Fatal(err)
	}
	doneA := make(chan error, 1)
	go func() { doneA <- cmdA.Wait() }()
	deadline := time.Now().Add(60 * time.Second)
	for len(bgSpans(t, f)) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("run A never started both workers:\n%s", errA.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	g := bgFix(t, "1.c", nil)
	g.state = f.state
	resB := g.brun("--budget-config", cfg)
	g.mustExit(resB, 0)
	if err := <-doneA; err != nil {
		t.Fatalf("run A: %v\n%s", err, errA.String())
	}
	runB := g.runID()
	evsB, err := budgetlog.ReadFile(filepath.Join(f.state, "runs", runB, "budget.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ds := bgDecisions(t, evsB, "1.c")
	if len(ds) < 2 || ds[0].Decision.Admit || ds[0].Decision.Rule != "headroom" {
		t.Fatalf("run B's 1.c: want a headroom refusal first, got %+v", ds)
	}
	if p, _ := bgPool(ds[0], "five_hour"); p["open_reserved"].(float64) < 0.0999 {
		t.Fatalf("run B must count run A's two open reservations: %v", p)
	}
	runs := map[string]bool{}
	for _, r := range ds[0].Open {
		runs[r.Run] = true
	}
	if len(runs) != 1 || runs[runB] {
		t.Fatalf("run B's refusal must list run A's open reservations: %+v", ds[0].Open)
	}
	if last := ds[len(ds)-1]; !last.Decision.Admit || !strings.Contains(strings.Join(last.Decision.StalePools, ","), "five_hour") {
		t.Fatalf("run B's 1.c: want admitted once five_hour went stale, got %+v", last.Decision)
	}
	if n := bgMaxOverlap(append(bgSpans(t, f), bgSpans(t, g)...)); n > 2 {
		t.Fatalf("headroom for two attempts, %d ran at once across both runs", n)
	}
	bgReplay(t, evsB)
}
