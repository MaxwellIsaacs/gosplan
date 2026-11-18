package state

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/budget"
	"github.com/MaxwellIsaacs/gosplan/internal/core"
	"github.com/MaxwellIsaacs/gosplan/internal/events"
)

var bsNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func bsExec(t *testing.T, s *Store, q string, args ...any) sql.Result {
	t.Helper()
	res, err := s.DB().Exec(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res
}

// bsRun registers a budget_runs row for run (which storeSeedTask created).
func bsRun(t *testing.T, s *Store, run string, heartbeat time.Time, maxParallel int, halt string) {
	t.Helper()
	tx := bsTx(t, s)
	r := BudgetRunRow{Run: run, Owner: "host:1:x", Mode: budget.ModeEnforce, MaxParallel: maxParallel,
		ConfigSHA256: "sha", ConfigJSON: "{}", HeartbeatAt: heartbeat}
	if err := UpsertBudgetRun(tx, r); err != nil {
		t.Fatal(err)
	}
	if halt != "" {
		if err := SetHalt(tx, run, halt, "cap:usd_estimate"); err != nil {
			t.Fatal(err)
		}
	}
	bsCommit(t, tx)
}

func bsTx(t *testing.T, s *Store) *sql.Tx {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func bsCommit(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// bsAttempt inserts an attempt row; ended nil = still running.
func bsAttempt(t *testing.T, s *Store, run, task string, n int, model string, started time.Time, ended *time.Time, outcome, usage string) core.AttemptRef {
	t.Helper()
	var end, out, use any
	if ended != nil {
		end, out, use = timestamp(*ended), outcome, usage
	}
	res := bsExec(t, s, `INSERT INTO attempts (run_id, task_id, n, session_id, model, started_at, ended_at, outcome, usage_json, cost_usd_estimate)
		VALUES (?, ?, ?, 'sess', ?, ?, ?, ?, ?, 0.5)`, run, task, n, model, timestamp(started), end, out, use)
	id, _ := res.LastInsertId()
	return core.AttemptRef{Run: run, Task: task, Attempt: n, AttemptID: id}
}

func bsReserve(t *testing.T, s *Store, ref core.AttemptRef, model string, created time.Time) {
	t.Helper()
	tx := bsTx(t, s)
	est := 1000.0
	d := budget.Decision{Model: model, Reserve: []budget.PoolReserve{{Pool: "five_hour", Reserve: 0.02, Basis: "ratio"}},
		Estimate: budget.Estimate{Tokens: &est, TokensBasis: "p90"}}
	if err := InsertReservation(tx, ref, d, true, 7, created); err != nil {
		t.Fatal(err)
	}
	bsCommit(t, tx)
}

func bsUtil(v float64) *float64 { return &v }

func bsObs(t *testing.T, s *Store, window string, util *float64, status string, at time.Time) {
	t.Helper()
	bsExec(t, s, `INSERT INTO window_observations (window, utilization, status, source, documented, observed_at, raw_json)
		VALUES (?, ?, ?, 'rate_limit_event', 1, ?, '{}')`, window, util, status, timestamp(at))
}

func bsConfig() budget.Config {
	c := budget.DefaultConfig()
	c.Alias = map[string]string{"session": "five_hour"}
	return c
}

func bsReq(run, task string) SnapshotRequest {
	return SnapshotRequest{Run: run, Task: task, Attempt: 1, Model: "sonnet", Mode: budget.ModeEnforce, Config: bsConfig(), Now: bsNow}
}

func bsLoad(t *testing.T, s *Store, req SnapshotRequest) budget.Snapshot {
	t.Helper()
	snap, err := LoadSnapshot(s.DB(), req)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func bsCandKeys(snap budget.Snapshot) []string {
	var out []string
	for _, c := range snap.Candidates {
		out = append(out, c.Run+"/"+c.Task)
	}
	sort.Strings(out)
	return out
}

// Two live runs, a dead one and a halted one share the state dir.
func TestBudgetStoreSnapshotAcrossRuns(t *testing.T) {
	s := newTestStore(t)
	for _, x := range [][3]string{
		{"A", "a1", "pending"}, {"A", "a2", "pending"}, {"B", "b1", "running"}, {"B", "b2", "pending"},
		{"D", "d1", "pending"}, {"H", "h1", "pending"},
	} {
		storeSeedTask(t, s, x[0], x[1], x[2])
	}
	// a2 is parked: not_before in the future. It is still a candidate.
	bsExec(t, s, `UPDATE tasks SET not_before = ? WHERE run_id = 'A' AND id = 'a2'`, timestamp(bsNow.Add(10*time.Minute)))
	bsRun(t, s, "A", bsNow, 2, "")
	bsRun(t, s, "B", bsNow.Add(-10*time.Second), 2, "")
	bsRun(t, s, "D", bsNow.Add(-2*time.Hour), 2, "") // dead: heartbeat past run_ttl
	bsRun(t, s, "H", bsNow, 2, "drain")              // halted
	ref := bsAttempt(t, s, "B", "b1", 1, "opus", bsNow.Add(-time.Minute), nil, "", "")
	bsReserve(t, s, ref, "opus", bsNow.Add(-time.Minute))
	// A dead run's open reservation is not live.
	storeSeedTask(t, s, "D", "d2", "running")
	dref := bsAttempt(t, s, "D", "d2", 1, "opus", bsNow.Add(-3*time.Hour), nil, "", "")
	bsReserve(t, s, dref, "opus", bsNow.Add(-3*time.Hour))

	snap := bsLoad(t, s, bsReq("A", "a1"))
	if got, want := bsCandKeys(snap), []string{"A/a1", "A/a2", "B/b2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v (dead run D and halted run H contribute none)", got, want)
	}
	if len(snap.Open) != 1 || snap.Open[0].Run != "B" || snap.Open[0].AttemptID != ref.AttemptID {
		t.Fatalf("Open = %+v, want only B's live reservation", snap.Open)
	}
	if snap.Open[0].Class != budget.ClassNormal || len(snap.Open[0].Pools) != 1 || snap.Open[0].Pools[0].Reserve != 0.02 {
		t.Fatalf("Open[0] = %+v", snap.Open[0])
	}
	var runs []string
	for _, r := range snap.Runs {
		runs = append(runs, r.Run)
		if r.Run == "H" && r.Halt != "drain" {
			t.Fatalf("H halt = %q", r.Halt)
		}
		if r.Run == "B" && (r.Running != 1 || !reflect.DeepEqual(r.Pending, []string{"b2"})) {
			t.Fatalf("B state = %+v", r)
		}
	}
	if !reflect.DeepEqual(runs, []string{"A", "B", "H"}) {
		t.Fatalf("runs = %v, want A B H", runs)
	}
	if snap.For != (budget.Key{Run: "A", Task: "a1", Attempt: 1}) || snap.ConfigSHA256 != bsConfig().SHA256() {
		t.Fatalf("For/sha = %+v %s", snap.For, snap.ConfigSHA256)
	}
	for _, c := range snap.Candidates {
		if c.Run == "A" && c.Task == "a1" && !reflect.DeepEqual(c.Models, []string{"sonnet"}) {
			t.Fatalf("own candidate models = %v", c.Models)
		}
		if c.Run == "B" && !reflect.DeepEqual(c.Models, []string{"opus"}) {
			t.Fatalf("B candidate models = %v, want its newest attempt's model", c.Models)
		}
	}
}

// A run with no free slot contributes no candidates.
func TestBudgetStoreSnapshotFullRunNoCandidates(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "pending")
	storeSeedTask(t, s, "B", "b1", "running")
	storeSeedTask(t, s, "B", "b2", "pending")
	bsRun(t, s, "A", bsNow, 2, "")
	bsRun(t, s, "B", bsNow, 1, "")
	snap := bsLoad(t, s, bsReq("A", "a1"))
	if got := bsCandKeys(snap); !reflect.DeepEqual(got, []string{"A/a1"}) {
		t.Fatalf("candidates = %v, want only A/a1", got)
	}
}

func TestBudgetStoreOrphans(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "pending")
	storeSeedTask(t, s, "A", "a2", "running")
	bsRun(t, s, "A", bsNow, 2, "")
	ended := bsNow.Add(-time.Minute)
	gone := bsAttempt(t, s, "A", "a1", 1, "sonnet", bsNow.Add(-5*time.Minute), &ended, "killed", "{}")
	bsReserve(t, s, gone, "sonnet", bsNow.Add(-5*time.Minute))
	live := bsAttempt(t, s, "A", "a2", 1, "sonnet", bsNow.Add(-time.Minute), nil, "", "")
	bsReserve(t, s, live, "sonnet", bsNow.Add(-time.Minute))

	snap := bsLoad(t, s, bsReq("A", "a1"))
	if len(snap.Open) != 1 || snap.Open[0].AttemptID != live.AttemptID {
		t.Fatalf("Open = %+v, want only the live reservation", snap.Open)
	}
	tx := bsTx(t, s)
	orph, err := SettleOrphans(tx, bsNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	bsCommit(t, tx)
	if len(orph) != 1 || orph[0].AttemptID != gone.AttemptID || orph[0].SettledAt == nil {
		t.Fatalf("orphans = %+v", orph)
	}
	rows, err := RunReservations(s.DB(), "A")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		switch r.AttemptID {
		case gone.AttemptID:
			if r.State != "settled" || r.ActualBasis != "orphan" || r.ActualTokens != nil {
				t.Fatalf("orphan row = %+v", r)
			}
		case live.AttemptID:
			if r.State != "open" {
				t.Fatalf("live row settled: %+v", r)
			}
		}
	}
	// An orphan never counts in Recent.
	if snap := bsLoad(t, s, bsReq("A", "a1")); len(snap.Recent) != 0 {
		t.Fatalf("Recent = %+v, want no orphan", snap.Recent)
	}
	// Run dead past run_ttl: its open reservation is an orphan too.
	tx = bsTx(t, s)
	orph, err = SettleOrphans(tx, bsNow.Add(5*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	bsCommit(t, tx)
	if len(orph) != 1 || orph[0].AttemptID != live.AttemptID {
		t.Fatalf("dead-run orphans = %+v", orph)
	}
}

func TestBudgetStoreRecentWithinStaleAfter(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "pending")
	storeSeedTask(t, s, "A", "a2", "passed")
	storeSeedTask(t, s, "A", "a3", "passed")
	bsRun(t, s, "A", bsNow, 2, "")
	usage := &events.Usage{Input: 1}
	for i, age := range []time.Duration{5 * time.Minute, 30 * time.Minute} {
		task := fmt.Sprintf("a%d", i+2)
		end := bsNow.Add(-age)
		ref := bsAttempt(t, s, "A", task, 1, "sonnet", end.Add(-time.Minute), &end, "exited", `{"input":1}`)
		bsReserve(t, s, ref, "sonnet", end.Add(-time.Minute))
		tx := bsTx(t, s)
		if _, _, ok, err := SettleReservation(tx, ref.AttemptID, usage, end); err != nil || !ok {
			t.Fatal(ok, err)
		}
		bsCommit(t, tx)
	}
	snap := bsLoad(t, s, bsReq("A", "a1")) // stale_after 20m
	if len(snap.Recent) != 1 || snap.Recent[0].Task != "a2" || snap.Recent[0].ActualTokens == nil || *snap.Recent[0].ActualTokens != 1 {
		t.Fatalf("Recent = %+v, want only a2 (settled 5m ago)", snap.Recent)
	}
	if len(snap.Open) != 0 {
		t.Fatalf("Open = %+v", snap.Open)
	}
}

func TestBudgetStoreAliasAndStatus(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "pending")
	bsRun(t, s, "A", bsNow, 2, "")
	bsObs(t, s, "five_hour", bsUtil(0.30), "allowed", bsNow.Add(-10*time.Minute))
	bsObs(t, s, "session", bsUtil(0.35), "allowed", bsNow.Add(-5*time.Minute))
	bsObs(t, s, "seven_day", bsUtil(0.5), "allowed", bsNow.Add(-5*time.Minute))
	bsObs(t, s, "unknown", nil, "rejected", bsNow.Add(-time.Minute))
	bsObs(t, s, "_probe", nil, "allowed", bsNow.Add(-time.Minute))
	bsObs(t, s, "seven_day", bsUtil(0.1), "allowed", bsNow.Add(-200*time.Hour)) // outside fit.lookback

	snap := bsLoad(t, s, bsReq("A", "a1"))
	var pools []string
	for p := range snap.Obs {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	if !reflect.DeepEqual(pools, []string{"five_hour", "seven_day"}) {
		t.Fatalf("pools = %v, want five_hour (session aliased) and seven_day", pools)
	}
	fh := snap.Obs["five_hour"]
	if len(fh) != 2 || *fh[0].Utilization != 0.30 || *fh[1].Utilization != 0.35 || fh[1].Pool != "five_hour" {
		t.Fatalf("five_hour rows = %+v", fh)
	}
	if len(snap.Obs["seven_day"]) != 1 {
		t.Fatalf("seven_day rows = %+v, want the old row outside the lookback dropped", snap.Obs["seven_day"])
	}
	var status []string
	for _, o := range snap.Status {
		status = append(status, o.Pool+":"+o.Status)
	}
	sort.Strings(status)
	if strings.Join(status, " ") != "five_hour:allowed five_hour:allowed seven_day:allowed unknown:rejected" {
		t.Fatalf("status = %v", status)
	}
}

func TestBudgetStoreSettleSumsFourClasses(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "running")
	bsRun(t, s, "A", bsNow, 2, "")
	ref := bsAttempt(t, s, "A", "a1", 1, "sonnet", bsNow.Add(-time.Minute), nil, "", "")
	bsReserve(t, s, ref, "sonnet", bsNow.Add(-time.Minute))
	tx := bsTx(t, s)
	res, actual, ok, err := SettleReservation(tx, ref.AttemptID, &events.Usage{Input: 1, Output: 20, CacheRead: 300, CacheCreate: 4000}, bsNow)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	bsCommit(t, tx)
	if actual == nil || *actual != 4321 || res.ActualTokens == nil || *res.ActualTokens != 4321 {
		t.Fatalf("actual = %v, res = %+v", actual, res)
	}
	rows, _ := RunReservations(s.DB(), "A")
	if len(rows) != 1 || rows[0].ActualBasis != "usage" || rows[0].Actual == nil || rows[0].Actual.CacheCreate != 4000 || rows[0].State != "settled" {
		t.Fatalf("row = %+v", rows)
	}
	tx = bsTx(t, s)
	if _, _, ok, err := SettleReservation(tx, ref.AttemptID, nil, bsNow); err != nil || ok {
		t.Fatalf("second settle: ok=%v err=%v, want ok=false", ok, err)
	}
	bsCommit(t, tx)
}

func TestBudgetStoreSettleUnknownUsage(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "running")
	bsRun(t, s, "A", bsNow, 2, "")
	ref := bsAttempt(t, s, "A", "a1", 1, "sonnet", bsNow.Add(-time.Minute), nil, "", "")
	bsReserve(t, s, ref, "sonnet", bsNow.Add(-time.Minute))
	tx := bsTx(t, s)
	_, actual, ok, err := SettleReservation(tx, ref.AttemptID, nil, bsNow)
	if err != nil || !ok || actual != nil {
		t.Fatal(ok, err, actual)
	}
	bsCommit(t, tx)
	var basis string
	var tokens sql.NullInt64
	if err := s.DB().QueryRow(`SELECT actual_basis, actual_tokens FROM budget_reservations`).Scan(&basis, &tokens); err != nil {
		t.Fatal(err)
	}
	if basis != "unknown" || tokens.Valid {
		t.Fatalf("basis %q tokens %v, want unknown/NULL", basis, tokens)
	}
}

// bsTasksDump is every column of every tasks row, as text.
func bsTasksDump(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.DB().Query(`SELECT * FROM tasks ORDER BY run_id, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
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
		fmt.Fprintf(&b, "%v\n", vals)
	}
	return b.String()
}

func TestBudgetStoreRearmParksRulesOnly(t *testing.T) {
	s := newTestStore(t)
	rules := []string{budget.RuleHeadroom, budget.RuleClassHold, budget.RuleFairness, budget.RuleBootstrap, budget.RuleStale,
		budget.RuleCap, budget.RuleBreaker, budget.RuleDeadline, budget.RuleHalt}
	for _, run := range []string{"A", "B"} {
		for i, rule := range rules {
			task := fmt.Sprintf("t%d", i)
			storeSeedTask(t, s, run, task, "pending")
			bsExec(t, s, `UPDATE tasks SET not_before = ? WHERE run_id = ? AND id = ?`, timestamp(bsNow.Add(time.Hour)), run, task)
			nb := bsNow.Add(time.Hour)
			tx := bsTx(t, s)
			if err := Park(tx, run, task, budget.Decision{Rule: rule, Reason: "r", NotBefore: &nb}, 3, bsNow); err != nil {
				t.Fatal(err)
			}
			bsCommit(t, tx)
		}
	}
	before := bsTasksDump(t, s)
	tx := bsTx(t, s)
	n, err := RearmParks(tx)
	if err != nil {
		t.Fatal(err)
	}
	bsCommit(t, tx)
	if n != 10 {
		t.Fatalf("RearmParks deleted %d rows, want 10 (5 rules x 2 runs)", n)
	}
	if after := bsTasksDump(t, s); after != before {
		t.Fatalf("RearmParks changed tasks:\nbefore %s\nafter  %s", before, after)
	}
	parks, err := Parks(s.DB(), "")
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, p := range parks {
		left = append(left, p.Run+":"+p.Rule)
	}
	want := []string{"A:cap", "A:breaker", "A:deadline", "A:halt", "B:cap", "B:breaker", "B:deadline", "B:halt"}
	if !reflect.DeepEqual(left, want) {
		t.Fatalf("parks left = %v, want %v", left, want)
	}
	gone, err := ParkedWithoutRow(s.DB(), "A", []string{"t0", "t5", "t4"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gone, []string{"t0", "t4"}) {
		t.Fatalf("ParkedWithoutRow = %v, want t0 t4", gone)
	}
}

func TestBudgetStoreParkUpsertAndHaltFirstWins(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "pending")
	bsRun(t, s, "A", bsNow, 2, "")
	nb1, nb2, at := bsNow.Add(time.Minute), bsNow.Add(2*time.Minute), bsNow.Add(90*time.Second)
	tx := bsTx(t, s)
	if err := Park(tx, "A", "a1", budget.Decision{Rule: budget.RuleHeadroom, Reason: "x", NotBefore: &nb1}, 1, bsNow); err != nil {
		t.Fatal(err)
	}
	if err := Park(tx, "A", "a1", budget.Decision{Rule: budget.RuleFairness, Reason: "y", NotBefore: &nb2, AdmitAt: &at}, 2, bsNow); err != nil {
		t.Fatal(err)
	}
	if err := Park(tx, "A", "a1", budget.Decision{Rule: budget.RuleFairness}, 2, bsNow); err == nil {
		t.Fatal("Park without not_before succeeded")
	}
	if err := SetHalt(tx, "A", "drain", "cap:usd_estimate"); err != nil {
		t.Fatal(err)
	}
	if err := SetHalt(tx, "A", "stop", "deadline"); err != nil {
		t.Fatal(err)
	}
	if err := SetHalt(tx, "A", "bogus", "x"); err == nil {
		t.Fatal("SetHalt accepted kind bogus")
	}
	bsCommit(t, tx)
	parks, _ := Parks(s.DB(), "A")
	if len(parks) != 1 || parks[0].Rule != budget.RuleFairness || !parks[0].NotBefore.Equal(nb2) || parks[0].AdmitAt == nil || parks[0].DecisionEID != 2 {
		t.Fatalf("parks = %+v", parks)
	}
	br, err := GetBudgetRun(s.DB(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if br.Halt != "drain" || br.HaltReason != "cap:usd_estimate" {
		t.Fatalf("halt = %q %q, want the first (drain)", br.Halt, br.HaltReason)
	}
	tx = bsTx(t, s)
	if err := HeartbeatBudgetRun(tx, "A", bsNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := HeartbeatBudgetRun(tx, "nope", bsNow); err == nil {
		t.Fatal("heartbeat of a missing run succeeded")
	}
	bsCommit(t, tx)
	if br, _ := GetBudgetRun(s.DB(), "A"); !br.HeartbeatAt.Equal(bsNow.Add(time.Minute)) {
		t.Fatalf("heartbeat = %v", br.HeartbeatAt)
	}
}

func TestBudgetStoreBreakersAndFacts(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "pending")
	bsRun(t, s, "A", bsNow, 2, "")
	closes := bsNow.Add(15 * time.Minute)
	tx := bsTx(t, s)
	open := budget.BreakerChange{Action: "open", Breaker: budget.Breaker{Kind: budget.RetryStorm, Scope: budget.ScopeRun, Run: "A",
		State: "open", ClosesAt: &closes, Reason: "6 retries", Enforced: true}}
	if err := ApplyBreakerChange(tx, open, []byte(`{"run":"A"}`), bsNow); err != nil {
		t.Fatal(err)
	}
	bsCommit(t, tx)
	bs, err := OpenBreakers(s.DB())
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].Kind != budget.RetryStorm || bs[0].OpenedAt == nil || !bs[0].ClosesAt.Equal(closes) || !bs[0].Enforced {
		t.Fatalf("open breakers = %+v", bs)
	}
	tx = bsTx(t, s)
	closeCh := budget.BreakerChange{Action: "close", Breaker: budget.Breaker{Kind: budget.RetryStorm, Scope: budget.ScopeRun, Run: "A"}}
	if err := ApplyBreakerChange(tx, closeCh, nil, bsNow.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ApplyBreakerChange(tx, budget.BreakerChange{Action: "count", Breaker: budget.Breaker{Kind: budget.VerifierError, Scope: budget.ScopeRun, Run: "A", Count: 2}}, nil, bsNow); err != nil {
		t.Fatal(err)
	}
	if err := ApplyBreakerChange(tx, budget.BreakerChange{Action: "explode"}, nil, bsNow); err == nil {
		t.Fatal("unknown action accepted")
	}
	bsCommit(t, tx)
	if bs, _ := OpenBreakers(s.DB()); len(bs) != 0 {
		t.Fatalf("still open: %+v", bs)
	}
	var cnt int
	var st string
	if err := s.DB().QueryRow(`SELECT count, state FROM budget_breakers WHERE kind = 'VERIFIER_ERROR'`).Scan(&cnt, &st); err != nil || cnt != 2 || st != "closed" {
		t.Fatalf("VERIFIER_ERROR row: count %d state %q err %v", cnt, st, err)
	}

	end := bsNow
	bsExec(t, s, `UPDATE tasks SET base_sha = 'base1' WHERE run_id = 'A' AND id = 'a1'`)
	bsAttempt(t, s, "A", "a1", 1, "sonnet", bsNow.Add(-3*time.Minute), &end, "start_failed", "{}")
	bsAttempt(t, s, "A", "a1", 2, "sonnet", bsNow.Add(-2*time.Minute), &end, "exited", "{}")
	bsAttempt(t, s, "A", "a1", 3, "sonnet", bsNow.Add(-time.Minute), nil, "", "")
	bsExec(t, s, `UPDATE attempts SET head_sha = 'h2', verdict_result = 'fail', failed_gates_json = '["test"]' WHERE n = 2`)
	facts, err := RunAttemptFacts(s.DB(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || !facts[0].Refunded || facts[1].Refunded || facts[1].HeadSHA != "h2" || facts[1].Verdict != "fail" ||
		!reflect.DeepEqual(facts[1].FailedGates, []string{"test"}) || facts[1].BaseSHA != "base1" {
		t.Fatalf("facts = %+v", facts)
	}
}

// Nothing in the store writes tasks: a whole cycle leaves the table as it was.
func TestBudgetStoreNeverWritesTasks(t *testing.T) {
	s := newTestStore(t)
	storeSeedTask(t, s, "A", "a1", "running")
	storeSeedTask(t, s, "A", "a2", "pending")
	before := bsTasksDump(t, s)
	bsRun(t, s, "A", bsNow, 2, "")
	ref := bsAttempt(t, s, "A", "a1", 1, "sonnet", bsNow.Add(-time.Minute), nil, "", "")
	bsReserve(t, s, ref, "sonnet", bsNow)
	nb := bsNow.Add(time.Minute)
	tx := bsTx(t, s)
	for _, err := range []error{
		Park(tx, "A", "a2", budget.Decision{Rule: budget.RuleHeadroom, NotBefore: &nb}, 1, bsNow),
		func() error {
			_, _, _, err := SettleReservation(tx, ref.AttemptID, &events.Usage{Input: 3}, bsNow)
			return err
		}(),
		func() error { _, err := SettleOrphans(tx, bsNow, time.Minute); return err }(),
		func() error { _, err := RearmParks(tx); return err }(),
		HeartbeatBudgetRun(tx, "A", bsNow),
		SetHalt(tx, "A", "stop", "deadline"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	bsCommit(t, tx)
	if _, err := LoadSnapshot(s.DB(), bsReq("A", "a2")); err != nil {
		t.Fatal(err)
	}
	if after := bsTasksDump(t, s); after != before {
		t.Fatalf("tasks changed:\n%s\n%s", before, after)
	}
}
