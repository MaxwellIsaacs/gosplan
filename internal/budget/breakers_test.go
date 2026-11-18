package budget

import (
	"strings"
	"testing"
	"time"
)

func bInput() BreakerInput {
	return BreakerInput{Now: tNow, Mode: ModeEnforce, Config: tCfg(), Run: "r1"}
}

// attempt is one consumed attempt of task started `ago` before tNow.
func attempt(id int64, task string, n int, ago time.Duration) AttemptFact {
	return AttemptFact{AttemptID: id, Task: task, N: n, StartedAt: tNow.Add(-ago), BaseSHA: "base", HeadSHA: "head", Verdict: "fail", FailedGates: []string{"test"}}
}

func only(t *testing.T, ch []BreakerChange, kind string) []BreakerChange {
	t.Helper()
	var out []BreakerChange
	for _, c := range ch {
		if c.Breaker.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func TestBudgetBreakersRetryStorm(t *testing.T) {
	// storm: 4 tasks × 3 attempts each within the window = 8 retries.
	storm := func() []AttemptFact {
		var as []AttemptFact
		id := int64(1)
		for _, task := range []string{"a", "b", "c", "d"} {
			for n := 1; n <= 3; n++ {
				as = append(as, attempt(id, task, n, time.Duration(13-id)*time.Minute))
				id++
			}
		}
		return as
	}
	open := Breaker{ID: 3, Kind: RetryStorm, Scope: ScopeRun, Run: "r1", State: "open", Count: 8, OpenedAt: tp(tNow.Add(-5 * time.Minute)), ClosesAt: tp(tNow.Add(10 * time.Minute))}
	cases := []struct {
		name    string
		mod     func(in *BreakerInput)
		action  string // "" = no change
		closes  time.Time
		count   int
		reasons []string
	}{
		{name: "opens at retries", mod: func(in *BreakerInput) { in.Attempts = storm() },
			action: "open", closes: tNow.Add(15 * time.Minute), count: 8,
			reasons: []string{"8 retries", ">= 6", "window 15m0s", "cooldown 15m0s", "2026-11-05T12:15:00Z"}},
		{name: "below the threshold stays closed", mod: func(in *BreakerInput) { in.Attempts = storm()[:6] }}, // a1-3, b1-3: 4 retries
		{name: "first attempts never count", mod: func(in *BreakerInput) {
			for i := int64(1); i <= 12; i++ {
				in.Attempts = append(in.Attempts, attempt(i, string(rune('a'+i)), 1, time.Minute))
			}
		}},
		{name: "retries outside the window do not count", mod: func(in *BreakerInput) {
			in.Attempts = storm()
			for i := range in.Attempts {
				in.Attempts[i].StartedAt = in.Attempts[i].StartedAt.Add(-time.Hour)
			}
		}},
		{name: "refunded attempts are neither retries nor predecessors", mod: func(in *BreakerInput) {
			in.Attempts = storm()
			for i := range in.Attempts {
				if in.Attempts[i].N == 1 || in.Attempts[i].N == 3 {
					in.Attempts[i].Refunded = true // only n=2 is consumed: no retry at all
				}
			}
		}},
		{name: "N >= 2 is not the test", mod: func(in *BreakerInput) {
			// every task: refunded start_failed n=1, consumed n=2: zero retries.
			for i := int64(0); i < 8; i++ {
				task := string(rune('a' + i))
				r := attempt(2*i+1, task, 1, 10*time.Minute)
				r.Refunded = true
				in.Attempts = append(in.Attempts, r, attempt(2*i+2, task, 2, 5*time.Minute))
			}
		}},
		{name: "open stays open before closes_at", mod: func(in *BreakerInput) { in.Attempts = storm(); in.Current = []Breaker{open} }},
		{name: "open closes at closes_at", mod: func(in *BreakerInput) {
			in.Attempts = storm()
			in.Current = []Breaker{open}
			in.Now = *open.ClosesAt
		}, action: "close", reasons: []string{"RETRY_STORM closed", "closes_at 2026-11-05T12:10:00Z reached"}},
		{name: "after a close only later retries count", mod: func(in *BreakerInput) {
			in.Attempts = storm()
			c := open
			c.State, c.ClosedAt = "closed", tp(tNow.Add(-time.Second))
			in.Current = []Breaker{c}
		}},
		{name: "another run's breaker is not this run's", mod: func(in *BreakerInput) {
			in.Attempts = storm()
			o := open
			o.Run = "r2"
			in.Current = []Breaker{o}
		}, action: "open", closes: tNow.Add(15 * time.Minute), count: 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := bInput()
			c.mod(&in)
			ch := only(t, EvalBreakers(in), RetryStorm)
			if c.action == "" {
				if len(ch) != 0 {
					t.Fatalf("changes %+v, want none", ch)
				}
				return
			}
			if len(ch) != 1 || ch[0].Action != c.action {
				t.Fatalf("changes %+v, want one %s", ch, c.action)
			}
			b := ch[0].Breaker
			if b.Scope != ScopeRun || b.Run != "r1" || !b.Enforced {
				t.Fatalf("breaker %+v", b)
			}
			if c.action == "open" && (b.State != "open" || b.ClosesAt == nil || !b.ClosesAt.Equal(c.closes) || b.Count != c.count) {
				t.Fatalf("open %+v, want closes %s count %d", b, c.closes, c.count)
			}
			if c.action == "close" && (b.State != "closed" || b.ClosedAt == nil || !b.ClosedAt.Equal(in.Now)) {
				t.Fatalf("close %+v", b)
			}
			for _, s := range c.reasons {
				if !strings.Contains(b.Reason, s) {
					t.Errorf("reason %q lacks %q", b.Reason, s)
				}
			}
		})
	}
	// Observe mode: evaluated and recorded unenforced.
	in := bInput()
	in.Mode, in.Attempts = ModeObserve, storm()
	if ch := only(t, EvalBreakers(in), RetryStorm); len(ch) != 1 || ch[0].Breaker.Enforced {
		t.Fatalf("observe: %+v", ch)
	}
}

func TestBudgetBreakersSameFailure(t *testing.T) {
	fail := func(id int64, gates ...string) AttemptFact {
		a := attempt(id, "t1", int(id), time.Duration(10-id)*time.Minute)
		a.FailedGates = gates
		return a
	}
	cases := []struct {
		name     string
		attempts []AttemptFact
		current  []Breaker
		task     string
		open     string // signature in the reason, "" = no change
	}{
		{name: "three equal gate failures open", attempts: []AttemptFact{fail(1, "test", "lint"), fail(2, "lint", "test"), fail(3, "test", "lint")}, open: "gates:lint,test"},
		{name: "two are not enough", attempts: []AttemptFact{fail(1, "test"), fail(2, "test")}},
		{name: "different gates stay closed", attempts: []AttemptFact{fail(1, "test"), fail(2, "lint"), fail(3, "test")}},
		{name: "only the last three count", attempts: []AttemptFact{fail(1, "lint"), fail(2, "test"), fail(3, "test"), fail(4, "test")}, open: "gates:test"},
		{name: "no-commit thrice opens", attempts: func() []AttemptFact {
			var as []AttemptFact
			for i := int64(1); i <= 3; i++ {
				a := fail(i)
				a.HeadSHA = map[bool]string{true: "", false: "base"}[i%2 == 0]
				as = append(as, a)
			}
			return as
		}(), open: "no-commit"},
		{name: "a refunded attempt never matches", attempts: func() []AttemptFact {
			as := []AttemptFact{fail(1, "test"), fail(2, "test"), fail(3, "test")}
			as[1].Refunded = true
			return as
		}()},
		{name: "other signatures never match", attempts: func() []AttemptFact {
			as := []AttemptFact{fail(1), fail(2), fail(3)}
			for i := range as {
				as[i].Verdict = "error"
			}
			return as
		}()},
		{name: "other tasks' attempts are ignored", attempts: func() []AttemptFact {
			as := []AttemptFact{fail(1, "test"), fail(2, "test"), fail(3, "test")}
			as[2].Task = "t2"
			return as
		}()},
		{name: "already open stays open, no new change", attempts: []AttemptFact{fail(1, "test"), fail(2, "test"), fail(3, "test")},
			current: []Breaker{{ID: 4, Kind: SameFailure, Scope: ScopeTask, Run: "r1", Task: "t1", State: "open"}}},
		{name: "not evaluated without a task", task: "-", attempts: []AttemptFact{fail(1, "test"), fail(2, "test"), fail(3, "test")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := bInput()
			in.Task, in.Attempts, in.Current = "t1", c.attempts, c.current
			if c.task == "-" {
				in.Task = ""
			}
			ch := only(t, EvalBreakers(in), SameFailure)
			if c.open == "" {
				if len(ch) != 0 {
					t.Fatalf("changes %+v, want none", ch)
				}
				return
			}
			if len(ch) != 1 || ch[0].Action != "open" {
				t.Fatalf("changes %+v, want one open", ch)
			}
			b := ch[0].Breaker
			if b.Scope != ScopeTask || b.Task != "t1" || b.Run != "r1" || b.ClosesAt != nil || b.State != "open" {
				t.Fatalf("breaker %+v", b)
			}
			if want := "SAME_FAILURE: 3 attempts with " + c.open; !strings.HasPrefix(b.Reason, want) {
				t.Fatalf("reason %q, want prefix %q", b.Reason, want)
			}
		})
	}
	// Never closes on its own, however late.
	in := bInput()
	in.Task, in.Now = "t1", tNow.Add(1000*time.Hour)
	in.Current = []Breaker{{ID: 4, Kind: SameFailure, Scope: ScopeTask, Run: "r1", Task: "t1", State: "open"}}
	if ch := only(t, EvalBreakers(in), SameFailure); len(ch) != 0 {
		t.Fatalf("SAME_FAILURE changed on its own: %+v", ch)
	}
}

func TestBudgetBreakersRateLimited(t *testing.T) {
	row := func(id int64, status string, ago time.Duration, resets *int64) Obs {
		return Obs{ID: id, Pool: "five_hour", Status: status, ObservedAt: tNow.Add(-ago), ResetsAt: resets, Source: "rate_limit_event"}
	}
	reset := tNow.Add(40 * time.Minute)
	j := Jitter("RATE_LIMITED|2|"+fmtTime(reset), tCfg().Jitter)
	closed := Breaker{ID: 5, Kind: RateLimited, Scope: ScopeGlobal, State: "closed", Count: 2, OpenedAt: tp(tNow.Add(-30 * time.Minute)), ClosedAt: tp(tNow.Add(-10 * time.Minute))}
	cases := []struct {
		name    string
		status  []Obs
		current []Breaker
		now     time.Time
		action  string
		closes  time.Time
		count   int
		reasons []string
	}{
		{name: "rejected with resets_at opens until the reset + jitter",
			status: []Obs{row(1, "allowed", 3*time.Minute, nil), row(2, "rejected", time.Minute, ip(reset.Unix()))},
			action: "open", closes: reset.Add(j), count: 1, reasons: []string{"observation 2", "status rejected", "resets_at 2026-11-05T12:40:00Z + jitter"}},
		{name: "api_retry rate_limited without a reset uses the base cooldown",
			status: []Obs{{ID: 3, Pool: "unknown", Status: "rate_limited", ObservedAt: tNow.Add(-time.Second), Source: "api_retry"}},
			action: "open", closes: tNow.Add(5 * time.Minute), count: 1, reasons: []string{"no resets_at known", "cooldown 5m0s"}},
		{name: "the pool's newest known reset is used when the row has none",
			status: []Obs{row(1, "allowed", 3*time.Minute, ip(reset.Unix())), {ID: 2, Pool: "five_hour", Status: "rejected", ObservedAt: tNow.Add(-time.Minute)}},
			action: "open", closes: reset.Add(j), count: 1},
		{name: "consecutive openings double the cooldown",
			status:  []Obs{row(7, "rate_limited", time.Minute, nil)},
			current: []Breaker{closed},
			action:  "open", closes: tNow.Add(20 * time.Minute), count: 3, reasons: []string{"opening 3", "base 5m0s x 2^2"}},
		{name: "the cooldown is capped at cooldown_max",
			status:  []Obs{row(7, "rate_limited", time.Minute, nil)},
			current: []Breaker{func() Breaker { b := closed; b.Count = 9; return b }()},
			action:  "open", closes: tNow.Add(time.Hour), count: 10},
		{name: "an allowed row in between resets the count",
			status:  []Obs{row(6, "allowed", 5*time.Minute, nil), row(7, "rate_limited", time.Minute, nil)},
			current: []Breaker{closed},
			action:  "open", closes: tNow.Add(5 * time.Minute), count: 1},
		{name: "a row older than the last close does not reopen",
			status:  []Obs{row(4, "rejected", 20*time.Minute, nil)},
			current: []Breaker{closed}},
		{name: "allowed rows never open", status: []Obs{row(1, "allowed", time.Minute, nil), row(2, "allowed_warning", 0, nil)}},
		{name: "open stays open before closes_at",
			status:  []Obs{row(2, "rejected", time.Minute, nil)},
			current: []Breaker{{ID: 5, Kind: RateLimited, Scope: ScopeGlobal, State: "open", Count: 1, OpenedAt: tp(tNow.Add(-time.Minute)), ClosesAt: tp(tNow.Add(time.Minute))}}},
		{name: "open closes at closes_at",
			current: []Breaker{{ID: 5, Kind: RateLimited, Scope: ScopeGlobal, State: "open", Count: 1, OpenedAt: tp(tNow.Add(-time.Minute)), ClosesAt: tp(tNow.Add(time.Minute))}},
			now:     tNow.Add(time.Minute), action: "close", reasons: []string{"RATE_LIMITED closed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := bInput()
			in.Status, in.Current = c.status, c.current
			if !c.now.IsZero() {
				in.Now = c.now
			}
			ch := only(t, EvalBreakers(in), RateLimited)
			if c.action == "" {
				if len(ch) != 0 {
					t.Fatalf("changes %+v, want none", ch)
				}
				return
			}
			if len(ch) != 1 || ch[0].Action != c.action {
				t.Fatalf("changes %+v, want one %s", ch, c.action)
			}
			b := ch[0].Breaker
			if b.Scope != ScopeGlobal {
				t.Fatalf("scope %q", b.Scope)
			}
			if c.action == "open" && (b.ClosesAt == nil || !b.ClosesAt.Equal(c.closes) || b.Count != c.count) {
				t.Fatalf("open closes %v count %d, want %s / %d (%s)", b.ClosesAt, b.Count, c.closes, c.count, b.Reason)
			}
			if c.action == "close" && (b.State != "closed" || !b.ClosedAt.Equal(in.Now)) {
				t.Fatalf("close %+v", b)
			}
			for _, s := range c.reasons {
				if !strings.Contains(b.Reason, s) {
					t.Errorf("reason %q lacks %q", b.Reason, s)
				}
			}
		})
	}
}

func TestBudgetBreakersSortedAndPure(t *testing.T) {
	in := bInput()
	in.Task = "a"
	for i := int64(1); i <= 7; i++ {
		in.Attempts = append(in.Attempts, attempt(i, "a", int(i), time.Duration(8-i)*time.Minute))
	}
	in.Status = []Obs{{ID: 1, Pool: "unknown", Status: "rate_limited", ObservedAt: tNow}}
	ch := EvalBreakers(in)
	var kinds []string
	for _, c := range ch {
		kinds = append(kinds, c.Breaker.Kind)
	}
	if strings.Join(kinds, ",") != "RATE_LIMITED,RETRY_STORM,SAME_FAILURE" {
		t.Fatalf("kinds %v, want sorted, all three", kinds)
	}
	if len(in.Attempts) != 7 || in.Attempts[0].AttemptID != 1 {
		t.Fatal("EvalBreakers mutated its input")
	}
}

func TestBudgetSignature(t *testing.T) {
	cases := []struct {
		name string
		a    AttemptFact
		want string
	}{
		{"no head is no-commit", AttemptFact{AttemptID: 1, BaseSHA: "b", Verdict: "fail"}, "no-commit"},
		{"head = base is no-commit", AttemptFact{AttemptID: 2, BaseSHA: "b", HeadSHA: "b", Verdict: "fail", FailedGates: []string{"x"}}, "no-commit"},
		{"gates sorted", AttemptFact{AttemptID: 3, BaseSHA: "b", HeadSHA: "h", Verdict: "fail", FailedGates: []string{"test", "custom", "lint"}}, "gates:custom,lint,test"},
		{"pass is other", AttemptFact{AttemptID: 4, BaseSHA: "b", HeadSHA: "h", Verdict: "pass"}, "other:4"},
		{"error is other", AttemptFact{AttemptID: 5, BaseSHA: "b", HeadSHA: "h", Verdict: "error"}, "other:5"},
		{"refunded is other even without a commit", AttemptFact{AttemptID: 6, Refunded: true}, "other:6"},
		{"refunded fail is other", AttemptFact{AttemptID: 7, Refunded: true, BaseSHA: "b", HeadSHA: "h", Verdict: "fail", FailedGates: []string{"test"}}, "other:7"},
	}
	for _, c := range cases {
		if got := Signature(c.a); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if Signature(AttemptFact{AttemptID: 8, HeadSHA: "h", BaseSHA: "b"}) == Signature(AttemptFact{AttemptID: 9, HeadSHA: "h", BaseSHA: "b"}) {
		t.Error("other signatures must be unique")
	}
	in := AttemptFact{AttemptID: 3, BaseSHA: "b", HeadSHA: "h", Verdict: "fail", FailedGates: []string{"z", "a"}}
	Signature(in)
	if in.FailedGates[0] != "z" {
		t.Error("Signature sorted its input slice")
	}
}
