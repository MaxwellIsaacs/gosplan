// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// budget_admit_test.go: RED BY DESIGN. Pools, projection, admission,
// reservations, parking, classes, shared accounting and routing.
package conformance

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBudgetObserveByDefault: with no budget config the run is in observe
// mode: it says so, nothing is enforced (two workers run at once), and every
// decision is still computed and logged with enforced=false, including the
// refusal enforce mode would have made.
// Clause: budget#BG-MODE
// Clause: budget#BG-EVENTS
func TestBudgetObserveByDefault(t *testing.T) {
	f := newRunFix(t, "(1.a || 1.b)", nil)
	work := []map[string]any{{"op": "sleep", "seconds": 3, "heartbeat": true}, {"op": "commit"}}
	f.scenario(map[string][]map[string]any{"1.a": work, "1.b": work})
	res := f.runNR()
	mustExit(t, "gosplan run (no budget config)", res, 0)
	if !strings.Contains(res.stderr, "observe mode") {
		t.Fatalf("gosplan run: no observe-mode notice on stderr:\n%s", clip(res.stderr))
	}
	if n := bgMaxOverlap(bgSpans(f)); n < 2 {
		t.Fatalf("observe mode must not enforce: two independent tasks never ran at once (max %d)", n)
	}
	ds := bgDecisions(t, bgLog(t, f, f.onlyRun()), "")
	if len(ds) < 2 {
		t.Fatalf("want a budget_decision per dispatch, got %d", len(ds))
	}
	refused := false
	for _, d := range ds {
		if d.Enforced || d.Mode != "observe" {
			t.Fatalf("observe-mode decision has enforced=%v mode=%q", d.Enforced, d.Mode)
		}
		refused = refused || (!d.Decision.Admit && d.Decision.Rule == "bootstrap")
	}
	if !refused {
		t.Fatalf("observe mode must still record the bootstrap refusal enforce would make; decisions: %+v", ds)
	}
	_, raw := f.status()
	b, _ := raw["budget"].(map[string]any)
	if b == nil || b["mode"] != "observe" {
		t.Fatalf("report budget.mode: want observe, got %v", raw["budget"])
	}
}

// TestBudgetBootstrapOneAtATime: enforce mode on a state dir with no
// observation at all admits exactly one attempt at a time and never deadlocks.
// Clause: budget#BG-BOOTSTRAP
func TestBudgetBootstrapOneAtATime(t *testing.T) {
	f := newRunFix(t, "(1.a || 1.b || 1.c)", nil)
	work := []map[string]any{{"op": "sleep", "seconds": 2, "heartbeat": true}, {"op": "commit"}}
	f.scenario(map[string][]map[string]any{"1.a": work, "1.b": work, "1.c": work})
	cfg := bgConfig(t, "", "")
	mustExit(t, "gosplan run --budget-config (enforce, no observations)", f.runNR("--budget-config", cfg), 0)
	if n := bgMaxOverlap(bgSpans(f)); n != 1 {
		t.Fatalf("bootstrap rule: want exactly 1 worker at a time, saw %d", n)
	}
	ds := bgDecisions(t, bgLog(t, f, f.onlyRun()), "")
	admits, refusals := 0, 0
	for _, d := range ds {
		if d.Decision.Rule != "bootstrap" {
			t.Fatalf("with no observation every decision must use rule bootstrap, got %q (%s)", d.Decision.Rule, d.Decision.Reason)
		}
		if !d.Enforced {
			t.Fatalf("enforce-mode decision recorded enforced=false")
		}
		if d.Decision.Admit {
			admits++
		} else {
			refusals++
		}
	}
	if admits != 3 || refusals == 0 {
		t.Fatalf("want 3 bootstrap admits and at least one bootstrap refusal, got %d / %d", admits, refusals)
	}
}

// TestBudgetParksUntilReset: a pool at 99% with one 5% unit reservation to
// make parks the dependent task with every input number, sets not_before at
// the observed reset, and starts it only after the reset.
// Clause: budget#BG-ADMIT
// Clause: budget#BG-PARK
// Clause: budget#BG-POOLS
func TestBudgetParksUntilReset(t *testing.T) {
	f, cfg := bgParkFix(t, 20)
	mustExit(t, "gosplan run --budget-config (five_hour at 99%)", f.runNR("--budget-config", cfg), 0)
	ds := bgDecisions(t, bgLog(t, f, f.onlyRun()), "2.a")
	if len(ds) < 2 {
		t.Fatalf("2.a: want a refusal and a later admit, got %d decisions", len(ds))
	}
	first := ds[0]
	if first.Decision.Admit || first.Decision.Rule != "headroom" || !strings.Contains(first.Decision.Reason, "five_hour") {
		t.Fatalf("2.a first decision: want refusal rule headroom naming five_hour, got admit=%v rule=%q reason=%q",
			first.Decision.Admit, first.Decision.Rule, first.Decision.Reason)
	}
	p, ok := first.pool("five_hour")
	if !ok {
		t.Fatalf("refusal lacks the five_hour pool numbers: %+v", first.Decision.Pools)
	}
	switch {
	case p.Level == nil || *p.Level != 0.99:
		t.Fatalf("five_hour level: want 0.99, got %v", p.Level)
	case p.Headroom == nil || *p.Headroom >= 0.05 || *p.Headroom < 0:
		t.Fatalf("five_hour headroom: want ~0.01, got %v", p.Headroom)
	case p.Reserve == nil || *p.Reserve != 0.05 || p.ReserveBasis != "unit":
		t.Fatalf("five_hour reserve: want 0.05 unit, got %v %q", p.Reserve, p.ReserveBasis)
	case p.Fits == nil || *p.Fits:
		t.Fatalf("five_hour fits: want false")
	case p.Documented == nil || *p.Documented:
		t.Fatalf("five_hour from rateLimitType must be documented=false")
	case p.ResetsAt == nil || p.ObservedAt == nil || p.AgeSeconds == nil || p.Source == "" || p.Projected == nil || p.OpenReserved == nil || p.Hold == nil:
		t.Fatalf("pool numbers incomplete: %+v", p)
	}
	if sd, ok := first.pool("seven_day"); !ok || sd.Fits == nil || !*sd.Fits {
		t.Fatalf("seven_day at 10%% must be checked and fit: %+v", sd)
	}
	if first.Decision.NotBefore == nil || first.Decision.AdmitAt == nil {
		t.Fatalf("a refusal must carry admit_at and not_before")
	}
	reset := *p.ResetsAt
	if nb := bgParseTS(t, *first.Decision.NotBefore); nb.Unix() > reset {
		t.Fatalf("not_before %s is after the reset %d (recheck_max is 2s)", nb, reset)
	}
	last := ds[len(ds)-1]
	if !last.Decision.Admit {
		t.Fatalf("2.a was never admitted")
	}
	if last.Decision.Rule != "stale" || !strings.Contains(strings.Join(last.Decision.StalePools, ","), "five_hour") {
		t.Fatalf("after the reset five_hour is stale (past_reset): want rule stale naming it, got %q %v", last.Decision.Rule, last.Decision.StalePools)
	}
	start, ok := bgFirstStart(bgSpans(f), "2.a")
	if !ok {
		t.Fatalf("2.a never started")
	}
	if start < reset*1000 {
		t.Fatalf("2.a started at %d ms, before the observed reset at %d s", start, reset)
	}
}

// TestBudgetSettlesActualTokens: the reservation is settled from the
// result's usage when the attempt ends.
// Clause: budget#BG-SETTLE
func TestBudgetSettlesActualTokens(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.scenario(map[string][]map[string]any{"1.a": {
		{"op": "usage", "input": 1000, "output": 2000, "cache_read": 3000, "cache_create": 4000}, {"op": "commit"}}})
	mustExit(t, "gosplan run --budget-config", f.runNR("--budget-config", bgConfig(t, "", "")), 0)
	got := sql(t, f.db(), "SELECT state||'|'||ifnull(actual_tokens,'-')||'|'||ifnull(actual_basis,'-') FROM budget_reservations;")
	if got != "settled|10000|usage" {
		t.Fatalf("budget_reservations: want settled|10000|usage, got %q", got)
	}
	settles := bgOfType(bgLog(t, f, f.onlyRun()), "budget_settle")
	if len(settles) != 1 {
		t.Fatalf("want one budget_settle event, got %d", len(settles))
	}
	if v, _ := bgNum(settles[0], "actual_tokens"); v != 10000 {
		t.Fatalf("budget_settle actual_tokens: want 10000, got %v", settles[0]["actual_tokens"])
	}
}

// TestBudgetEventsComplete: budget.jsonl has the M1 envelope with contiguous
// seq, only budget_* types, every input row a decision names (config,
// observations, spans, samples) recorded once as budget_input before it, a
// settle per attempt; events.jsonl carries no budget type.
// Clause: budget#BG-EVENTS
func TestBudgetEventsComplete(t *testing.T) {
	f, cfg := bgParkFix(t, 20)
	mustExit(t, "gosplan run --budget-config", f.runNR("--budget-config", cfg), 0)
	run := f.onlyRun()
	evs := bgLog(t, f, run)
	inputs := map[string]bool{} // kind/id
	settles := 0
	idKey := func(kind string, id any) string {
		b, _ := json.Marshal(id)
		return kind + "/" + strings.Trim(string(b), `"`)
	}
	for i, ev := range evs {
		if ev.V != 1 || ev.Seq != int64(i+1) || ev.Run != run || ev.TS == "" {
			t.Fatalf("budget.jsonl line %d: bad envelope v=%d seq=%d run=%q ts=%q", i+1, ev.V, ev.Seq, ev.Run, ev.TS)
		}
		if !bgBudgetType.MatchString(ev.Type) {
			t.Fatalf("budget.jsonl line %d: unknown type %q", i+1, ev.Type)
		}
		if !strings.Contains(string(ev.Data), `"eid"`) {
			t.Fatalf("budget.jsonl line %d: data lacks eid", i+1)
		}
		switch ev.Type {
		case "budget_input":
			m := bgOfType([]event{ev}, ev.Type)[0]
			kind, _ := m["kind"].(string)
			if miss := keysMissing(m, "kind", "id", "row"); len(miss) > 0 || !map[string]bool{"config": true, "obs": true, "span": true, "sample": true}[kind] {
				t.Fatalf("budget_input seq %d: want kind config|obs|span|sample with id and row, got %v", ev.Seq, m)
			}
			k := idKey(kind, m["id"])
			if inputs[k] {
				t.Fatalf("budget_input %s logged twice (seq %d): each input row is logged once", k, ev.Seq)
			}
			inputs[k] = true
		case "budget_decision":
			m := bgOfType([]event{ev}, ev.Type)[0]
			if miss := keysMissing(m, "now", "mode", "enforced", "config_sha256", "obs_ids", "span_ids", "sample_ids", "open", "breakers", "candidates", "runs", "for", "decision"); len(miss) > 0 {
				t.Fatalf("budget_decision seq %d lacks %v", ev.Seq, miss)
			}
			d := bgDecisions(t, []event{ev}, "")[0]
			if !inputs[idKey("config", d.ConfigSHA)] {
				t.Fatalf("decision seq %d names config %q not recorded before it", ev.Seq, d.ConfigSHA)
			}
			for kind, ids := range map[string][]int64{"obs": d.ObsIDs, "span": d.SpanIDs, "sample": d.SampleIDs} {
				for _, id := range ids {
					if !inputs[idKey(kind, id)] {
						t.Fatalf("decision seq %d names %s %d, not recorded as budget_input before it", ev.Seq, kind, id)
					}
				}
			}
			// 1.a is decided in a fresh state dir, before any pool was
			// observed; 2.a is decided after 1.a reported both pools.
			if d.Decision.For.Task == "2.a" && len(d.ObsIDs) == 0 {
				t.Fatalf("decision seq %d for 2.a names no observation; 1.a reported two pools", ev.Seq)
			}
		case "budget_settle":
			settles++
		}
	}
	if settles != 2 {
		t.Fatalf("want 2 budget_settle events (1.a, 2.a), got %d", settles)
	}
	for _, ev := range readEvents(t, filepath.Join(f.state, "runs", run, "events.jsonl")) {
		if strings.HasPrefix(ev.Type, "budget_") {
			t.Fatalf("events.jsonl must not carry %s (the M1 schema does not know it)", ev.Type)
		}
	}
}

// TestBudgetClassHold: a best_effort task cannot use headroom held for
// critical; the critical task, ready at the same time, starts first. 2.b
// reports five_hour again before it ends, so 1.a's reservation (settled after
// the first observation) stops counting and 2.a fits once 2.b
// is done.
// Clause: budget#BG-CLASS
func TestBudgetClassHold(t *testing.T) {
	f := newRunFix(t, "1.a -> (2.a || 2.b)", nil)
	bgSidecar(t, f.spec, "2.a", "class = \"best_effort\"\n")
	bgSidecar(t, f.spec, "2.b", "class = \"critical\"\n")
	f.scenario(map[string][]map[string]any{
		"1.a": {{"op": "rate_limit", "window": "five_hour", "utilization": 0.70, "resets_in": 3600}, {"op": "commit"}},
		"2.b": {{"op": "sleep", "seconds": 2, "heartbeat": true},
			{"op": "rate_limit", "window": "five_hour", "utilization": 0.70, "resets_in": 3600}, {"op": "commit"}},
	})
	cfg := bgConfig(t, "", "[budget.cold]\nunit_fraction = 0.05\n[budget.class.critical]\nreserve_fraction = 0.6\n[budget.class.normal]\nreserve_fraction = 0.0\n")
	mustExit(t, "gosplan run --budget-config (classes)", f.runNR("--budget-config", cfg), 0)
	spans := bgSpans(f)
	a, okA := bgFirstStart(spans, "2.a")
	b, okB := bgFirstStart(spans, "2.b")
	if !okA || !okB || b >= a {
		t.Fatalf("critical 2.b must start before best_effort 2.a: 2.b=%d 2.a=%d", b, a)
	}
	held := false
	for _, d := range bgDecisions(t, bgLog(t, f, f.onlyRun()), "2.a") {
		held = held || (!d.Decision.Admit && d.Decision.Rule == "class_hold")
	}
	if !held {
		t.Fatalf("2.a (best_effort) must be refused with rule class_hold while critical's share is held")
	}
}

// TestBudgetSharedAcrossRuns: two gosplan run processes on one state dir share
// pools and reservations: the second run sees the first run's open
// reservations and parks instead of claiming the same headroom. After run A
// ends, its reservations settled after the newest observation still count,
// so run B starts only once five_hour goes stale (30 s,
// no probe) under the stale rule.
// Clause: budget#BG-SHARED
// Clause: budget#BG-ADMIT
func TestBudgetSharedAcrossRuns(t *testing.T) {
	f := newRunFix(t, "(1.a || 1.b)", nil)
	work := []map[string]any{{"op": "rate_limit", "window": "five_hour", "utilization": 0.88, "resets_in": 3600},
		{"op": "sleep", "seconds": 20, "heartbeat": true}, {"op": "commit"}}
	f.scenario(map[string][]map[string]any{"1.a": work, "1.b": work})
	cfg := bgConfig(t, "stale_after = \"30s\"\n", "[budget.cold]\nunit_fraction = 0.05\n[budget.class.critical]\nreserve_fraction = 0.0\n[budget.class.normal]\nreserve_fraction = 0.0\n")
	cmdA, doneA := bgStartWatched(f, "--budget-config", cfg)
	if !eventually(40*time.Second, func() bool { return bgExited(doneA) || len(f.starts("")) >= 2 }) || len(f.starts("")) < 2 {
		t.Fatalf("run A exited or never started both workers (starts: %d)", len(f.starts("")))
	}
	runA := f.onlyRun()

	g := newRunFix(t, "1.c", nil)
	args := []string{nrBin, "run", "--spec", g.spec, "--project", g.project, "--state-dir", f.state,
		"--claude", fakeClaude, "--gosplan-bin", nrBin, "--max-parallel", "3", "--max-attempts", "2",
		"--stall-timeout", "3s", "--retry-base", "1s", "--mutation", "off", "--usage-probe=false",
		"--attest-key", filepath.Join(f.state, "no-such-key"), "--budget-config", cfg}
	resB := run(t, 3*time.Minute, g.project, g.env(), args...)
	mustExit(t, "gosplan run B (shared state dir)", resB, 0)
	if !eventually(60*time.Second, func() bool { return bgExited(doneA) }) {
		bgStop(cmdA)
		t.Fatalf("run A did not finish")
	}
	runB := bgOtherRun(t, f, runA)
	evs := bgLog(t, f, runB)
	var refusal *bgDecision
	for _, d := range bgDecisions(t, evs, "1.c") {
		if !d.Decision.Admit && d.Decision.Rule == "headroom" {
			d := d
			refusal = &d
			break
		}
	}
	if refusal == nil {
		t.Fatalf("run B's 1.c was never refused for headroom while run A held two reservations")
	}
	p, _ := refusal.pool("five_hour")
	if p.OpenReserved == nil || *p.OpenReserved < 0.0999 {
		t.Fatalf("run B must count run A's two open 5%% reservations, open_reserved=%v", p.OpenReserved)
	}
	sawA := false
	for _, r := range refusal.Open {
		sawA = sawA || r["run"] == runA
	}
	if !sawA {
		t.Fatalf("run B's refusal does not list run A's (%s) open reservations", runA)
	}
	if n := bgMaxOverlap(append(bgSpans(f), bgSpans(g)...)); n > 2 {
		t.Fatalf("headroom for two attempts, but %d ran at once across both runs", n)
	}
}

// TestBudgetRoutingOptIn: with routing enabled, a task's prefer list picks
// the first model whose pools admit (week opus full -> sonnet); with routing
// off the list is ignored with a warning.
// Clause: budget#BG-ROUTING
// Clause: budget#BG-POOLS
func TestBudgetRoutingOptIn(t *testing.T) {
	for _, on := range []bool{true, false} {
		f := newRunFix(t, "1.a -> 2.a", nil)
		bgSidecar(t, f.spec, "2.a", "prefer = [\"opus\", \"sonnet\"]\n")
		reset := 3600
		if !on {
			reset = 5
		}
		f.scenario(map[string][]map[string]any{"1.a": {{"op": "rate_limit", "window": "five_hour", "utilization": 0.10, "resets_in": 3600,
			"unified": map[string]any{"seven_day_opus": map[string]any{"utilization": 0.99, "resets_in": reset}}}, {"op": "commit"}}})
		tables := "[budget.cold]\nunit_fraction = 0.05\n"
		if on {
			tables += "[budget.routing]\nenabled = true\n"
		}
		res := f.runNR("--budget-config", bgConfig(t, "", tables), "--model", "opus")
		mustExit(t, "gosplan run --budget-config (routing "+map[bool]string{true: "on", false: "off"}[on]+")", res, 0)
		st := f.starts("2.a")
		if len(st) == 0 {
			t.Fatalf("2.a never started")
		}
		want := "sonnet"
		if !on {
			want = "opus"
			if !strings.Contains(res.stderr, "prefer ignored") {
				t.Fatalf("routing off: want a 'prefer ignored' warning on stderr:\n%s", clip(res.stderr))
			}
		}
		if st[0].Model != want {
			t.Fatalf("routing on=%v: 2.a model want %s, got %s", on, want, st[0].Model)
		}
	}
}

// TestBudgetStaleTriggersProbe: a pool older than stale_after makes the gate
// run the $0 /usage probe during the run, and a still-stale pool is handled by
// the stated stale rule (one open attempt), not by a guess.
// Clause: budget#BG-STALE
func TestBudgetStaleTriggersProbe(t *testing.T) {
	f := newRunFix(t, "1.a -> 2.a", nil)
	f.sample("usage.json", "usage.json")
	f.scenario(map[string][]map[string]any{"1.a": {
		{"op": "rate_limit", "window": "five_hour", "utilization": 0.10, "resets_in": 3600},
		{"op": "sleep", "seconds": 4, "heartbeat": true}, {"op": "commit"}}})
	cfg := bgConfig(t, "stale_after = \"2s\"\nprobe_min_interval = \"1s\"\n", "")
	mustExit(t, "gosplan run --budget-config --usage-probe=true", f.runNR("--budget-config", cfg, "--usage-probe=true"), 0)
	start, ok := bgFirstStart(bgSpans(f), "1.a")
	if !ok {
		t.Fatalf("1.a never started")
	}
	probedLater := false
	for _, r := range f.ledger() {
		probedLater = probedLater || (r.Event == "usage" && r.TMS > start)
	}
	if !probedLater {
		t.Fatalf("five_hour went stale during 1.a but no /usage probe ran after 1.a started")
	}
	evs := bgLog(t, f, f.onlyRun())
	ran := false
	for _, p := range bgOfType(evs, "budget_probe") {
		ran = ran || p["ran"] == true
	}
	if !ran {
		t.Fatalf("no budget_probe event with ran=true")
	}
	ds := bgDecisions(t, evs, "2.a")
	if len(ds) == 0 {
		t.Fatalf("no decision for 2.a")
	}
	d := ds[len(ds)-1]
	if !d.Decision.Admit || d.Decision.Rule != "stale" || !strings.Contains(strings.Join(d.Decision.StalePools, ","), "five_hour") {
		t.Fatalf("2.a: want admit under rule stale naming five_hour, got admit=%v rule=%q stale=%v", d.Decision.Admit, d.Decision.Rule, d.Decision.StalePools)
	}
}
