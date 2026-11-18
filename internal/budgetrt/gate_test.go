package budgetrt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/budgetlog"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/reconcile"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// Gate tests against a real gosplan.db: each test plays the reconciler's part
// (dispatch tx: NextAttempt, Admit, transition, StartAttempt, Reserve;
// finish tx: FinishAttempt, Settle) with a controllable clock.

type budgetClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *budgetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *budgetClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type budgetFix struct {
	t     *testing.T
	s     *state.Store
	dir   string
	run   string
	clk   *budgetClock
	g     *Gate
	opt   Options
	spec  *spec.Spec
	cfg   budget.Config
	model string
}

const budgetTestCfg = "jitter = \"0s\"\nstale_recheck = \"1s\"\nfairness_recheck = \"1s\"\nrecheck_max = \"2s\"\n"

// budgetSpec writes a spec dir with the sequence and sidecar extras.
func budgetSpec(t *testing.T, seq string, extra map[string]string) *spec.Spec {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(p, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("sequence.txt", seq+"\n")
	for _, id := range strings.FieldsFunc(seq, func(r rune) bool { return strings.ContainsRune(" ()|->\n", r) }) {
		must(id+".md", "# "+id+"\n")
		must(id+".task.toml", "allow_new_protected = true\n"+extra[id])
	}
	sp, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// budgetConfig parses a config with mode and the test's short rechecks.
func budgetConfig(t *testing.T, mode, body string) budget.Config {
	t.Helper()
	if mode == budget.ModeEnforce && !strings.Contains(body, "[budget.cold]") {
		body += "\n[budget.cold]\nunit_fraction = 0.05\n"
	}
	toml := "[budget]\nmode = \"" + mode + "\"\n" + budgetTestCfg + body
	// Top-level keys must come before the first table.
	if i := strings.Index(body, "["); i >= 0 {
		toml = "[budget]\nmode = \"" + mode + "\"\n" + budgetTestCfg + body[:i] + "\n" + body[i:]
	}
	c, _, err := budget.ParseConfig([]byte(toml))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, toml)
	}
	if err := c.Validate(mode); err != nil {
		t.Fatal(err)
	}
	return c
}

func newBudgetFix(t *testing.T, seq, mode, cfgBody string, extra map[string]string, mut func(*Options)) *budgetFix {
	t.Helper()
	dir := t.TempDir()
	clk := &budgetClock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	s, err := state.Open(filepath.Join(dir, "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.Now = clk.Now
	f := &budgetFix{t: t, s: s, dir: dir, clk: clk, model: "sonnet"}
	f.spec = budgetSpec(t, seq, extra)
	f.cfg = budgetConfig(t, mode, cfgBody)
	f.run = f.importRun("run-a")
	tasks := map[string]TaskMeta{}
	for _, tk := range f.spec.Tasks {
		tasks[tk.ID] = TaskMeta{Class: tk.Class, Prefer: tk.Prefer, MaxTurns: tk.MaxTurns}
	}
	f.opt = Options{Run: f.run, StateDir: dir, RunDir: filepath.Join(dir, "runs", f.run), Mode: mode, Config: f.cfg,
		MaxParallel: 3, Owner: "test-owner", Now: clk.Now, MaxTurns: 200, Model: f.model, Tasks: tasks}
	if mut != nil {
		mut(&f.opt)
	}
	f.g = f.newGate(f.opt)
	return f
}

func (f *budgetFix) importRun(run string) string {
	f.t.Helper()
	ctx := context.Background()
	if err := f.s.ImportRun(ctx, run, "/project", "base0", f.spec); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.ImportBudgetTasks(ctx, run, f.spec); err != nil {
		f.t.Fatal(err)
	}
	return run
}

func (f *budgetFix) newGate(o Options) *Gate {
	f.t.Helper()
	if err := os.MkdirAll(o.RunDir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	g, err := New(f.s, o)
	if err != nil {
		f.t.Fatal(err)
	}
	return g
}

// imm runs fn in one IMMEDIATE tx and fails the test on error.
func (f *budgetFix) imm(fn func(tx *sql.Tx) error) {
	f.t.Helper()
	if err := f.s.Immediate(context.Background(), fn); err != nil {
		f.t.Fatal(err)
	}
}

// dispatch plays reconcile.dispatch for task on gate g in one tx: Admit,
// then on admit pending->running, StartAttempt and Reserve; on refusal the
// reconciler's not_before update.
func (f *budgetFix) dispatch(g *Gate, run, task string) (reconcile.AdmitResult, core.AttemptRef) {
	f.t.Helper()
	var res reconcile.AdmitResult
	var ref core.AttemptRef
	f.imm(func(tx *sql.Tx) error {
		var err error
		ref, res, err = f.dispatchTx(tx, g, run, task)
		return err
	})
	return res, ref
}

func (f *budgetFix) dispatchTx(tx *sql.Tx, g *Gate, run, task string) (core.AttemptRef, reconcile.AdmitResult, error) {
	now := f.clk.Now()
	n, err := state.NextAttempt(tx, run, task)
	if err != nil {
		return core.AttemptRef{}, reconcile.AdmitResult{}, err
	}
	res, err := g.Admit(tx, reconcile.AdmitRequest{Run: run, Task: task, Attempt: n, Model: f.model, Now: now})
	if err != nil {
		return core.AttemptRef{}, res, err
	}
	if !res.Admit {
		nb := res.NotBefore
		p := &nb
		return core.AttemptRef{}, res, state.UpdateTask(tx, run, task, state.TaskUpdate{NotBefore: &p})
	}
	if err := state.Transition(tx, now, run, task, core.StatusPending, core.StatusRunning, "attempt"); err != nil {
		return core.AttemptRef{}, res, err
	}
	ref, err := state.StartAttempt(tx, now, state.AttemptStart{Run: run, Task: task, SessionID: "s-" + task, Model: res.Model, Owner: "o", StreamPath: "/dev/null", BaseSHA: "base0"})
	if err != nil {
		return ref, res, err
	}
	return ref, res, g.Reserve(tx, ref, res)
}

// finish plays handleExit: FinishAttempt + Settle, then running->to.
func (f *budgetFix) finish(g *Gate, ref core.AttemptRef, end state.AttemptEnd, to string) {
	f.t.Helper()
	f.imm(func(tx *sql.Tx) error {
		now := f.clk.Now()
		if err := state.FinishAttempt(tx, now, ref, end); err != nil {
			return err
		}
		if err := g.Settle(tx, ref, end); err != nil {
			return err
		}
		if to != "" {
			return state.Transition(tx, now, ref.Run, ref.Task, core.StatusRunning, to, "done")
		}
		return nil
	})
}

func (f *budgetFix) observe(window string, u float64, resetsIn time.Duration, mine bool) {
	f.t.Helper()
	uu := u
	o := state.WindowObservation{Window: window, Utilization: &uu, Status: "allowed", Source: "rate_limit_event", ObservedAt: f.clk.Now()}
	if resetsIn != 0 {
		r := f.clk.Now().Add(resetsIn).Unix()
		o.ResetsAt = &r
	}
	if err := f.s.RecordWindows(context.Background(), []state.WindowObservation{o}); err != nil {
		f.t.Fatal(err)
	}
	if mine {
		f.g.Observed(o)
	}
}

func (f *budgetFix) events(g *Gate) []budgetlog.Event {
	f.t.Helper()
	if err := g.Flush(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	evs, err := budgetlog.ReadFile(g.logPath)
	if err != nil {
		f.t.Fatal(err)
	}
	return evs
}

func decisionsOf(t *testing.T, evs []budgetlog.Event, task string) []DecisionEvent {
	t.Helper()
	var out []DecisionEvent
	for _, ev := range evs {
		if ev.Type != budgetlog.TypeDecision {
			continue
		}
		var d DecisionEvent
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if task == "" || d.For.Task == task {
			out = append(out, d)
		}
	}
	return out
}

func (f *budgetFix) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.DB().QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *budgetFix) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.s.DB().Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

// fullFix: five_hour at 0.99 resetting in 1h, seen by this process.
func fullFix(t *testing.T, mode string) *budgetFix {
	f := newBudgetFix(t, "1.a", mode, "", nil, nil)
	f.observe("five_hour", 0.99, time.Hour, true)
	return f
}

// TestBudgetGateEnforcedRefusalParks: a nearly full pool refuses with rule
// headroom; the park row and the decision event are written in the dispatch
// tx and agree (eid, rule); the gate never writes tasks.not_before.
func TestBudgetGateEnforcedRefusalParks(t *testing.T) {
	f := fullFix(t, budget.ModeEnforce)
	res, _ := f.dispatch(f.g, f.run, "1.a")
	if res.Admit || res.Rule != budget.RuleHeadroom {
		t.Fatalf("want refusal headroom, got admit=%v rule=%s (%s)", res.Admit, res.Rule, res.Reason)
	}
	if !res.NotBefore.After(f.clk.Now()) {
		t.Fatalf("not_before %s not after now", res.NotBefore)
	}
	parks, err := state.Parks(f.s.DB(), f.run)
	if err != nil || len(parks) != 1 {
		t.Fatalf("want one park row, got %v %v", parks, err)
	}
	var typ, data string
	if err := f.s.DB().QueryRow(`SELECT type, data_json FROM budget_events WHERE id = ?`, parks[0].DecisionEID).Scan(&typ, &data); err != nil {
		t.Fatalf("park decision_eid %d names no budget event: %v", parks[0].DecisionEID, err)
	}
	var ev DecisionEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatal(err)
	}
	if typ != budgetlog.TypeDecision || ev.Decision.Rule != parks[0].Rule || ev.Decision.Admit || !ev.Enforced {
		t.Fatalf("park and decision disagree: park %+v, event %s %+v", parks[0], typ, ev.Decision)
	}
	// The gate wrote no not_before itself: the test's dispatch did, with res.NotBefore.
	task, err := f.s.GetTask(context.Background(), f.run, "1.a")
	if err != nil || task.NotBefore == nil || !task.NotBefore.Equal(res.NotBefore) {
		t.Fatalf("not_before: want the reconciler's %s, got %v", res.NotBefore, task.NotBefore)
	}
}

// TestBudgetGateParkLeavesTasksUntouched: an enforced refusal writes the
// park row only. Inside the dispatch tx, before the caller sets not_before,
// every tasks row is exactly what it was before Admit, for the first logged
// refusal and for a suppressed repeat. (Through the real reconciler a gate
// write of the same not_before is masked: the reconciler writes that value
// in the same tx, so its memory check cannot see it. This test does.)
func TestBudgetGateParkLeavesTasksUntouched(t *testing.T) {
	f := fullFix(t, budget.ModeEnforce)
	for i := 0; i < 2; i++ {
		f.imm(func(tx *sql.Tx) error {
			before := dumpTasks(t, tx)
			n, err := state.NextAttempt(tx, f.run, "1.a")
			if err != nil {
				return err
			}
			res, err := f.g.Admit(tx, reconcile.AdmitRequest{Run: f.run, Task: "1.a", Attempt: n, Model: f.model, Now: f.clk.Now()})
			if err != nil {
				return err
			}
			if res.Admit {
				t.Fatalf("round %d: want a refusal", i)
			}
			if n := countTx(t, tx, `SELECT count(*) FROM budget_parks`); n != 1 {
				t.Fatalf("round %d: want the park row, got %d", i, n)
			}
			if after := dumpTasks(t, tx); after != before {
				t.Fatalf("round %d: Admit wrote tasks:\nbefore %s\nafter  %s", i, before, after)
			}
			nb := res.NotBefore
			p := &nb
			return state.UpdateTask(tx, f.run, "1.a", state.TaskUpdate{NotBefore: &p})
		})
		f.clk.Advance(time.Second)
	}
}

// dumpTasks renders every column of every tasks row, in key order.
func dumpTasks(t *testing.T, tx *sql.Tx) string {
	t.Helper()
	rows, err := tx.Query(`SELECT * FROM tasks ORDER BY run_id, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, c := range cols {
			if v, ok := vals[i].([]byte); ok {
				vals[i] = string(v)
			}
			b.WriteString(c + "=" + strings.TrimSpace(fmt.Sprint(vals[i])) + " ")
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestBudgetGateRolledBackDecisionLeavesNothing: a dispatch tx that rolls
// back after Admit leaves no decision event and no park row, and the gate's
// memory follows (SelfCheck finds nothing missing, cross-checks stay quiet).
func TestBudgetGateRolledBackDecisionLeavesNothing(t *testing.T) {
	f := fullFix(t, budget.ModeEnforce)
	boom := errors.New("rollback")
	err := f.s.Immediate(context.Background(), func(tx *sql.Tx) error {
		if _, res, err := f.dispatchTx(tx, f.g, f.run, "1.a"); err != nil || res.Admit {
			t.Fatalf("dispatch: %v admit=%v", err, res.Admit)
		}
		if n := countTx(t, tx, `SELECT count(*) FROM budget_parks`); n != 1 {
			t.Fatalf("inside the tx: want the park row, got %d", n)
		}
		if n := countTx(t, tx, `SELECT count(*) FROM budget_events WHERE type = 'budget_decision'`); n != 1 {
			t.Fatalf("inside the tx: want the decision event, got %d", n)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM budget_parks`); n != 0 {
		t.Fatalf("rolled back: park rows %d", n)
	}
	if n := f.count(`SELECT count(*) FROM budget_events WHERE type = 'budget_decision'`); n != 0 {
		t.Fatalf("rolled back: decision events %d", n)
	}
	// The next decision commits; memory has no ghost of the first.
	res, _ := f.dispatch(f.g, f.run, "1.a")
	if res.Admit {
		t.Fatal("want a refusal")
	}
	if _, err := f.g.Tick(context.Background(), f.clk.Now()); err != nil {
		t.Fatalf("tick after rollback: %v", err)
	}
	if r := f.g.SelfCheck(context.Background()); r != "" {
		t.Fatalf("SelfCheck after a rolled-back decision: %s", r)
	}
	if n := len(decisionsOf(t, f.events(f.g), "1.a")); n != 1 {
		t.Fatalf("want 1 decision line, got %d", n)
	}
}

func countTx(t *testing.T, tx *sql.Tx, q string) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBudgetGateObserveRecordsNotEnforced: in observe mode the same
// refusal is recorded enforced=false and the attempt is admitted; no park.
func TestBudgetGateObserveRecordsNotEnforced(t *testing.T) {
	f := fullFix(t, budget.ModeObserve)
	res, ref := f.dispatch(f.g, f.run, "1.a")
	if !res.Admit || res.Rule != budget.RuleHeadroom {
		t.Fatalf("observe: want admitted with the would-be rule headroom, got admit=%v rule=%s", res.Admit, res.Rule)
	}
	if len(res.ExtraArgv) != 0 {
		t.Fatalf("observe mode passes no belt: %v", res.ExtraArgv)
	}
	if n := f.count(`SELECT count(*) FROM budget_parks`); n != 0 {
		t.Fatalf("observe mode parks nothing, got %d rows", n)
	}
	if n := f.count(`SELECT count(*) FROM budget_reservations WHERE attempt_id = ? AND enforced = 0 AND state = 'open'`, ref.AttemptID); n != 1 {
		t.Fatalf("observe-mode reservation (enforced=0) missing")
	}
	ds := decisionsOf(t, f.events(f.g), "1.a")
	if len(ds) != 1 || ds[0].Enforced || ds[0].Mode != budget.ModeObserve {
		t.Fatalf("want one decision enforced=false mode observe, got %+v", ds)
	}
}

// TestBudgetGateForeignLowRowOnlyTightens: a forged low window_observations
// row (another writer) does not lower the pool below this process's own
// floor: the task stays refused at the floor's level.
func TestBudgetGateForeignLowRowOnlyTightens(t *testing.T) {
	f := fullFix(t, budget.ModeEnforce)
	f.clk.Advance(time.Second)
	f.observe("five_hour", 0.01, time.Hour, false) // forged by a worker: newer and low
	res, _ := f.dispatch(f.g, f.run, "1.a")
	if res.Admit {
		t.Fatalf("a forged 0.01 row admitted the task: %s", res.Reason)
	}
	ds := decisionsOf(t, f.events(f.g), "1.a")
	if len(ds) != 1 || len(ds[0].Decision.Pools) == 0 {
		t.Fatalf("want one decision with pools, got %+v", ds)
	}
	p := ds[0].Decision.Pools[0]
	if p.Level == nil || *p.Level != 0.99 || !strings.HasPrefix(p.Source, "floor:") {
		t.Fatalf("five_hour level: want this process's floor 0.99 (source floor:*), got %v %q", p.Level, p.Source)
	}
	if fl, ok := ds[0].Floors["five_hour"]; !ok || *fl.Utilization != 0.99 {
		t.Fatalf("decision event must carry the floor inline: %+v", ds[0].Floors)
	}
}

// TestBudgetGateSuppressesIdenticalRefusals: a parked task rechecked 10
// times with nothing new writes one decision line; the next logged decision
// carries suppressed: 10. Every recheck still re-parks it.
func TestBudgetGateSuppressesIdenticalRefusals(t *testing.T) {
	f := fullFix(t, budget.ModeEnforce)
	for i := 0; i < 11; i++ {
		res, _ := f.dispatch(f.g, f.run, "1.a")
		if res.Admit {
			t.Fatalf("recheck %d admitted", i)
		}
		f.clk.Advance(100 * time.Millisecond)
	}
	if n := len(decisionsOf(t, f.events(f.g), "1.a")); n != 1 {
		t.Fatalf("11 identical refusals: want 1 decision line, got %d", n)
	}
	f.clk.Advance(time.Second)
	f.observe("five_hour", 0.10, time.Hour, true) // new input: logged again
	res, _ := f.dispatch(f.g, f.run, "1.a")
	if !res.Admit {
		t.Fatalf("after a fresh low reading: want admit, got %s", res.Reason)
	}
	ds := decisionsOf(t, f.events(f.g), "1.a")
	if len(ds) != 2 || ds[1].Suppressed != 10 {
		t.Fatalf("want 2 decision lines, the second with suppressed 10, got %d lines %+v", len(ds), ds[len(ds)-1].Suppressed)
	}
	if r := f.g.SelfCheck(context.Background()); r != "" {
		t.Fatal(r)
	}
}

// TestBudgetGateReplayRecomputes: every decision in budget.jsonl recomputes
// to the same JSON with budget.Decide on the snapshot rebuilt from its
// budget_input lines, and a tampered decision is drift.
func TestBudgetGateReplayRecomputes(t *testing.T) {
	f := newBudgetFix(t, "(1.a || 1.b) -> 2.a", budget.ModeEnforce, "", nil, nil)
	resA, refA := f.dispatch(f.g, f.run, "1.a") // bootstrap admit
	if !resA.Admit {
		t.Fatalf("1.a: %s", resA.Reason)
	}
	f.dispatch(f.g, f.run, "1.b") // bootstrap refusal
	f.observe("five_hour", 0.50, time.Hour, true)
	f.observe("seven_day", 0.20, 72*time.Hour, true)
	f.clk.Advance(2 * time.Second)
	f.finish(f.g, refA, state.AttemptEnd{Outcome: "exited", Usage: events.Usage{Input: 10, Output: 20}, CostUSDEstimate: 0.01, HeadSHA: "h1"}, core.StatusPassed)
	f.clk.Advance(2 * time.Second)
	if res, _ := f.dispatch(f.g, f.run, "1.b"); !res.Admit {
		t.Fatalf("1.b: %s", res.Reason)
	}
	evs := f.events(f.g)
	n, err := Replay(evs)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if want := len(decisionsOf(t, evs, "")); n != want || n < 3 {
		t.Fatalf("replay recomputed %d of %d decisions", n, want)
	}
	// Tamper: flip the last decision's rule.
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == budgetlog.TypeDecision {
			evs[i].Data = json.RawMessage(strings.Replace(string(evs[i].Data), `"rule":"admit"`, `"rule":"headroom"`, 1))
			break
		}
	}
	if _, err := Replay(evs); err == nil || !strings.Contains(err.Error(), "budget drift") {
		t.Fatalf("a tampered decision must be drift, got %v", err)
	}
}

// TestBudgetGateSettle: actual tokens are the sum of the four usage classes
// of the result line the reconciler parsed (basis usage), an attempt without
// usage settles unknown, and each gets one budget_settle event.
func TestBudgetGateSettle(t *testing.T) {
	f := newBudgetFix(t, "(1.a || 1.b)", budget.ModeObserve, "", nil, nil)
	_, refA := f.dispatch(f.g, f.run, "1.a")
	_, refB := f.dispatch(f.g, f.run, "1.b")
	f.finish(f.g, refA, state.AttemptEnd{Outcome: "exited", Usage: events.Usage{Input: 1000, Output: 2000, CacheRead: 3000, CacheCreate: 4000}}, "")
	f.finish(f.g, refB, state.AttemptEnd{Outcome: "exited"}, "")
	get := func(aid int64) string {
		var s string
		if err := f.s.DB().QueryRow(`SELECT state||'|'||ifnull(actual_tokens,'-')||'|'||actual_basis FROM budget_reservations WHERE attempt_id = ?`, aid).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if g := get(refA.AttemptID); g != "settled|10000|usage" {
		t.Fatalf("1.a reservation: %s", g)
	}
	if g := get(refB.AttemptID); g != "settled|-|unknown" {
		t.Fatalf("1.b reservation: %s", g)
	}
	settles := 0
	for _, ev := range f.events(f.g) {
		if ev.Type == budgetlog.TypeSettle {
			settles++
			var s SettleEvent
			_ = json.Unmarshal(ev.Data, &s)
			if s.AttemptID == refA.AttemptID && (s.ActualTokens == nil || *s.ActualTokens != 10000 || s.ActualBasis != "usage") {
				t.Fatalf("settle event for 1.a: %+v", s)
			}
		}
	}
	if settles != 2 {
		t.Fatalf("want 2 budget_settle events, got %d", settles)
	}
}

// TestBudgetGateOrphanSettled: an attempt that ended without a Settle
// (haltLocked's best-effort finish) leaves its reservation open; the next
// dispatch settles it with actual_basis orphan and a budget_settle event.
func TestBudgetGateOrphanSettled(t *testing.T) {
	f := newBudgetFix(t, "(1.a || 1.b)", budget.ModeEnforce, "", nil, nil)
	_, refA := f.dispatch(f.g, f.run, "1.a")
	f.imm(func(tx *sql.Tx) error { // no Settle: an orphan
		return state.FinishAttempt(tx, f.clk.Now(), refA, state.AttemptEnd{Outcome: "killed"})
	})
	f.clk.Advance(time.Second)
	res, _ := f.dispatch(f.g, f.run, "1.b")
	if !res.Admit {
		t.Fatalf("1.b after the orphan was settled: want bootstrap admit, got %s", res.Reason)
	}
	var basis string
	if err := f.s.DB().QueryRow(`SELECT state||'|'||actual_basis FROM budget_reservations WHERE attempt_id = ?`, refA.AttemptID).Scan(&basis); err != nil {
		t.Fatal(err)
	}
	if basis != "settled|orphan" {
		t.Fatalf("orphan reservation: want settled|orphan, got %s", basis)
	}
	found := false
	for _, m := range bgOf(f.events(f.g), budgetlog.TypeSettle) {
		found = found || (m["actual_basis"] == "orphan" && int64(m["attempt_id"].(float64)) == refA.AttemptID)
	}
	if !found {
		t.Fatal("no budget_settle event with actual_basis orphan")
	}
}

func bgOf(evs []budgetlog.Event, typ string) []map[string]any {
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

// TestBudgetGateCrossChecks: rows this process wrote, changed behind it,
// make the next hook return an error wrapping reconcile.ErrBudgetTampered.
func TestBudgetGateCrossChecks(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(f *budgetFix)
	}{
		{"budget_tasks class", func(f *budgetFix) {
			f.exec(`UPDATE budget_tasks SET class = 'critical' WHERE task_id = '1.b'`)
		}},
		{"budget_runs mode", func(f *budgetFix) { f.exec(`UPDATE budget_runs SET mode = 'observe'`) }},
		{"budget_runs halt", func(f *budgetFix) { f.exec(`UPDATE budget_runs SET halt = 'drain', halt_reason = 'x'`) }},
		{"reservation pools", func(f *budgetFix) {
			f.exec(`UPDATE budget_reservations SET pools_json = '[{"pool":"x","reserve":0,"basis":"unit"}]'`)
		}},
		{"reservation deleted", func(f *budgetFix) { f.exec(`DELETE FROM budget_reservations`) }},
		{"reservation settled", func(f *budgetFix) {
			f.exec(`UPDATE budget_reservations SET state = 'settled', actual_basis = 'usage', settled_at = ?`, f.clk.Now().Format(time.RFC3339Nano))
		}},
		{"park changed", func(f *budgetFix) { f.exec(`UPDATE budget_parks SET rule = 'deadline'`) }},
		{"park appeared", func(f *budgetFix) {
			f.exec(`INSERT INTO budget_parks (run_id, task_id, rule, reason, parked_at, not_before, decision_eid) VALUES (?, '1.a', 'headroom', 'x', ?, ?, 1)`, f.run, f.clk.Now().Format(time.RFC3339Nano), f.clk.Now().Format(time.RFC3339Nano))
		}},
		{"own breaker appeared", func(f *budgetFix) {
			f.exec(`INSERT INTO budget_breakers (kind, scope, run_id, state, enforced) VALUES ('RETRY_STORM', 'run', ?, 'open', 1)`, f.run)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBudgetFix(t, "(1.a || 1.b || 1.c)", budget.ModeEnforce, "", nil, nil)
			if res, _ := f.dispatch(f.g, f.run, "1.a"); !res.Admit {
				t.Fatal(res.Reason)
			}
			if res, _ := f.dispatch(f.g, f.run, "1.b"); res.Admit {
				t.Fatal("1.b: want a bootstrap refusal (park row)")
			}
			// Untouched: a hook passes.
			if _, err := f.g.Tick(context.Background(), f.clk.Now()); err != nil {
				t.Fatalf("tick before tampering: %v", err)
			}
			tc.tamper(f)
			_, err := f.g.Tick(context.Background(), f.clk.Now())
			if !errors.Is(err, reconcile.ErrBudgetTampered) {
				t.Fatalf("Tick after %s: want ErrBudgetTampered, got %v", tc.name, err)
			}
			err = f.s.Immediate(context.Background(), func(tx *sql.Tx) error {
				_, _, err := f.dispatchTx(tx, f.g, f.run, "1.c")
				return err
			})
			if !errors.Is(err, reconcile.ErrBudgetTampered) {
				t.Fatalf("Admit after %s: want ErrBudgetTampered, got %v", tc.name, err)
			}
		})
	}
}

// TestBudgetGateSharedRunsRearm: two gates (two runs) on one state dir. The
// second sees the first's open reservation (bootstrap refusal naming it);
// when the first settles, its re-arm deletes the second's park row, neither
// gate reports tampering, and the second's Rearm returns its parked task.
func TestBudgetGateSharedRunsRearm(t *testing.T) {
	f := newBudgetFix(t, "1.a", budget.ModeEnforce, "", nil, nil)
	runB := f.importRun("run-b")
	ob := f.opt
	ob.Run, ob.RunDir = runB, filepath.Join(f.dir, "runs", runB)
	gb := f.newGate(ob)
	_, refA := f.dispatch(f.g, f.run, "1.a")
	res, _ := f.dispatch(gb, runB, "1.a")
	if res.Admit || res.Rule != budget.RuleBootstrap {
		t.Fatalf("run B: want a bootstrap refusal while run A holds a reservation, got %v %s", res.Admit, res.Reason)
	}
	ds := decisionsOf(t, f.events(gb), "1.a")
	sawA := false
	for _, r := range ds[0].Open {
		sawA = sawA || r.Run == f.run
	}
	if !sawA {
		t.Fatalf("run B's decision does not list run A's open reservation: %+v", ds[0].Open)
	}
	f.finish(f.g, refA, state.AttemptEnd{Outcome: "exited", Usage: events.Usage{Input: 5}}, core.StatusPassed)
	if n := f.count(`SELECT count(*) FROM budget_parks WHERE run_id = ?`, runB); n != 0 {
		t.Fatalf("run A's settle must re-arm run B's park, %d rows left", n)
	}
	for _, g := range []*Gate{f.g, gb} {
		if _, err := g.Tick(context.Background(), f.clk.Now()); err != nil {
			t.Fatalf("tick after the other run's re-arm: %v", err)
		}
	}
	var nbs map[string]time.Time
	f.imm(func(tx *sql.Tx) error {
		var err error
		nbs, err = gb.Rearm(tx, f.clk.Now())
		return err
	})
	if _, ok := nbs["1.a"]; !ok || len(nbs) != 1 {
		t.Fatalf("run B Rearm: want 1.a, got %v", nbs)
	}
	f.imm(func(tx *sql.Tx) error { // forgotten: a second Rearm returns nothing
		nbs2, err := gb.Rearm(tx, f.clk.Now())
		if len(nbs2) != 0 {
			t.Fatalf("second Rearm: %v", nbs2)
		}
		return err
	})
}

// TestBudgetGateBeltArgv: --max-budget-usd = belt (p90 cost estimate x
// factor, up to the cent) only in enforce mode with belt.usd_factor set and
// cost history.
func TestBudgetGateBeltArgv(t *testing.T) {
	for _, tc := range []struct {
		mode, cfg string
		want      []string
	}{
		{budget.ModeEnforce, "[budget.estimate]\nmin_samples = 2\n[budget.belt]\nusd_factor = 2.0\n", []string{"--max-budget-usd", "0.60"}},
		{budget.ModeEnforce, "[budget.estimate]\nmin_samples = 2\n", nil},
		{budget.ModeObserve, "[budget.estimate]\nmin_samples = 2\n[budget.belt]\nusd_factor = 2.0\n", nil},
	} {
		f := newBudgetFix(t, "1.a -> 1.b -> 1.c", tc.mode, tc.cfg, nil, nil)
		for i, id := range []string{"1.a", "1.b"} {
			res, ref := f.dispatch(f.g, f.run, id)
			if !res.Admit || len(res.ExtraArgv) != 0 {
				t.Fatalf("%s: admit=%v argv=%v (%s)", id, res.Admit, res.ExtraArgv, res.Reason)
			}
			f.clk.Advance(time.Second)
			f.finish(f.g, ref, state.AttemptEnd{Outcome: "exited", Usage: events.Usage{Input: 10}, CostUSDEstimate: 0.1 * float64(i+2), DurationMS: 5}, core.StatusPassed)
		}
		res, _ := f.dispatch(f.g, f.run, "1.c")
		if !res.Admit || strings.Join(res.ExtraArgv, " ") != strings.Join(tc.want, " ") {
			t.Fatalf("%s %q: want argv %v, got admit=%v %v (%s)", tc.mode, tc.cfg, tc.want, res.Admit, res.ExtraArgv, res.Reason)
		}
	}
}

// TestBudgetGateRetryStormIgnoresRefunded: RETRY_STORM counts retries after
// consumed attempts only; refunded (start_failed / lost) attempts, as the
// gate saw them through Settle, are neither retries nor predecessors.
func TestBudgetGateRetryStormIgnoresRefunded(t *testing.T) {
	cfg := "[budget.breaker.retry_storm]\nretries = 2\nwindow = \"1m\"\ncooldown = \"10s\"\n"
	run := func(outcome string) (opened bool) {
		f := newBudgetFix(t, "(1.a || 1.b)", budget.ModeObserve, cfg, nil, nil)
		for i := 0; i < 2; i++ {
			for _, id := range []string{"1.a", "1.b"} {
				_, ref := f.dispatch(f.g, f.run, id)
				f.clk.Advance(100 * time.Millisecond)
				f.finish(f.g, ref, state.AttemptEnd{Outcome: outcome}, core.StatusPending)
			}
		}
		if _, err := f.g.Tick(context.Background(), f.clk.Now()); err != nil {
			t.Fatal(err)
		}
		for _, m := range bgOf(f.events(f.g), budgetlog.TypeBreaker) {
			br, _ := m["breaker"].(map[string]any)
			opened = opened || (br["kind"] == budget.RetryStorm && m["action"] == "open")
		}
		return opened
	}
	if !run("exited") {
		t.Fatal("two retries of consumed attempts within 1m must open RETRY_STORM")
	}
	if run("start_failed") {
		t.Fatal("refunded start_failed attempts opened RETRY_STORM")
	}
}

// TestBudgetGateRetryStormRefusesStormMakingRetry: with retries = 2, the
// admission of the second retry is the one that reaches the threshold, so
// RETRY_STORM opens inside that Admit and the retry is refused with rule
// breaker; it does not start ahead of the breaker opening behind it.
func TestBudgetGateRetryStormRefusesStormMakingRetry(t *testing.T) {
	cfg := "[budget.breaker.retry_storm]\nretries = 2\nwindow = \"1m\"\ncooldown = \"10s\"\n"
	f := newBudgetFix(t, "1.a", budget.ModeEnforce, cfg, nil, nil)
	for i := 1; i <= 2; i++ {
		res, ref := f.dispatch(f.g, f.run, "1.a")
		if !res.Admit {
			t.Fatalf("attempt %d: want admitted (at most one retry so far), got %s: %s", i, res.Rule, res.Reason)
		}
		f.clk.Advance(100 * time.Millisecond)
		f.finish(f.g, ref, state.AttemptEnd{Outcome: "exited"}, core.StatusPending)
	}
	res, _ := f.dispatch(f.g, f.run, "1.a")
	if res.Admit || res.Rule != budget.RuleBreaker || !strings.Contains(res.Reason, budget.RetryStorm) {
		t.Fatalf("attempt 3 (the second retry): want refused by RETRY_STORM, got admit=%v %s: %s", res.Admit, res.Rule, res.Reason)
	}
	opens := 0
	for _, m := range bgOf(f.events(f.g), budgetlog.TypeBreaker) {
		br, _ := m["breaker"].(map[string]any)
		if br["kind"] == budget.RetryStorm && m["action"] == "open" {
			opens++
		}
	}
	if opens != 1 {
		t.Fatalf("want one RETRY_STORM open event, got %d", opens)
	}
	if n := f.count(`SELECT count(*) FROM budget_reservations`); n != 2 {
		t.Fatalf("the refused retry must reserve nothing: %d reservations", n)
	}
}

// TestBudgetGateSameFailure: three attempts with the same signature
// (no-commit) make Retry return the SAME_FAILURE block reason in enforce
// mode, from the facts of the hook calls; observe mode logs it only.
func TestBudgetGateSameFailure(t *testing.T) {
	for _, mode := range []string{budget.ModeEnforce, budget.ModeObserve} {
		f := newBudgetFix(t, "1.a", mode, "", nil, nil)
		var block string
		for i := 0; i < 3; i++ {
			_, ref := f.dispatch(f.g, f.run, "1.a")
			f.clk.Advance(time.Second)
			f.imm(func(tx *sql.Tx) error {
				end := state.AttemptEnd{Outcome: "exited", HeadSHA: "base0"} // no commit
				if err := state.FinishAttempt(tx, f.clk.Now(), ref, end); err != nil {
					return err
				}
				if err := f.g.Settle(tx, ref, end); err != nil {
					return err
				}
				rows, err := state.ListTasksTx(tx, f.run)
				if err != nil || len(rows) != 1 {
					t.Fatalf("tasks: %v", err)
				}
				row := rows[0]
				row.BaseSHA = "base0"
				var rerr error
				block, rerr = f.g.Retry(tx, row)
				if rerr != nil {
					return rerr
				}
				return state.Transition(tx, f.clk.Now(), f.run, "1.a", core.StatusRunning, core.StatusPending, "retry")
			})
			if i < 2 && block != "" {
				t.Fatalf("%s: blocked after %d attempts: %s", mode, i+1, block)
			}
		}
		if mode == budget.ModeEnforce && !strings.HasPrefix(block, "SAME_FAILURE: 3 attempts with no-commit") {
			t.Fatalf("enforce: want SAME_FAILURE block reason, got %q", block)
		}
		if mode == budget.ModeObserve && block != "" {
			t.Fatalf("observe: Retry must not block, got %q", block)
		}
		if n := f.count(`SELECT count(*) FROM budget_breakers WHERE kind = 'SAME_FAILURE' AND state = 'open'`); n != 1 {
			t.Fatalf("%s: want one open SAME_FAILURE row, got %d", mode, n)
		}
	}
}

// TestBudgetGateSelfCheckDetectsEdit: a decision line edited in
// budget.jsonl mid-run is caught at the end-of-run self-check.
func TestBudgetGateSelfCheckDetectsEdit(t *testing.T) {
	f := fullFix(t, budget.ModeEnforce)
	f.dispatch(f.g, f.run, "1.a")
	if r := f.g.SelfCheck(context.Background()); r != "" {
		t.Fatalf("clean log: %s", r)
	}
	b, err := os.ReadFile(f.g.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.g.logPath, []byte(strings.Replace(string(b), `"rule":"headroom"`, `"rule":"admit"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.g.SelfCheck(context.Background()); !strings.HasPrefix(r, "state_inconsistent: budget.jsonl: decision eid") {
		t.Fatalf("edited decision: want state_inconsistent, got %q", r)
	}
}

// TestBudgetGateDeadlineAndCaps: Tick stores the first halt: a USD cap
// reached by settled cost estimates drains (cap:usd_estimate); a deadline
// reached stops; budget_runs.halt holds it.
func TestBudgetGateDeadlineAndCaps(t *testing.T) {
	capUSD := 0.015
	f := newBudgetFix(t, "1.a -> 1.b", budget.ModeObserve, "", nil, func(o *Options) { o.CapUSD = &capUSD })
	_, ref := f.dispatch(f.g, f.run, "1.a")
	if h, err := f.g.Tick(context.Background(), f.clk.Now()); err != nil || h.Kind != "" {
		t.Fatalf("before the cap: %+v %v", h, err)
	}
	f.finish(f.g, ref, state.AttemptEnd{Outcome: "exited", Usage: events.Usage{Input: 1}, CostUSDEstimate: 0.02}, core.StatusPassed)
	h, err := f.g.Tick(context.Background(), f.clk.Now())
	if err != nil || h.Kind != "drain" || h.Reason != HaltCapUSD || len(h.Parked) != 1 || h.Parked[0] != "1.b" {
		t.Fatalf("cap reached: want drain cap:usd_estimate parked [1.b], got %+v %v", h, err)
	}
	if r, _ := state.GetBudgetRun(f.s.DB(), f.run); r.Halt != "drain" || r.HaltReason != HaltCapUSD {
		t.Fatalf("budget_runs halt: %+v", r)
	}

	dl := f.clk.Now().Add(5 * time.Second)
	g := newBudgetFix(t, "1.a", budget.ModeObserve, "", nil, func(o *Options) { o.Deadline = &dl })
	if h, _ := g.g.Tick(context.Background(), g.clk.Now()); h.Kind != "" {
		t.Fatalf("before the deadline: %+v", h)
	}
	g.clk.Advance(6 * time.Second)
	if h, err := g.g.Tick(context.Background(), g.clk.Now()); err != nil || h.Kind != "stop" || h.Reason != HaltDeadline {
		t.Fatalf("at the deadline: want stop deadline, got %+v %v", h, err)
	}
}
