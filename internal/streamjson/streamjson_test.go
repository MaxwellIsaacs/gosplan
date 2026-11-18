package streamjson

import (
	"os"
	"path/filepath"
	"testing"
)

func fixtureLine(t *testing.T, name string) Line {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	l, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestParseResultReal(t *testing.T) {
	r, err := ParseResult(fixtureLine(t, "result.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Subtype != "success" || r.Result != "ok" || r.IsError || r.NumTurns != 1 ||
		r.DurationMS != 1484 || r.StopReason != "end_turn" || r.TerminalReason != "completed" ||
		r.SessionID != "28a383f5-ec08-4962-a59e-88f9b61e14b7" || r.PermissionDenials != 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if r.CostUSDEstimate != 0.0030338700000000006 {
		t.Fatalf("cost = %v", r.CostUSDEstimate)
	}
	u := r.Usage
	if u.Input != 2 || u.Output != 4 || u.CacheRead != 11227 || u.CacheCreate != 14597 {
		t.Fatalf("usage = %+v", u)
	}
	if _, err := ParseResult(Line{Type: "system", Raw: []byte(`{}`)}); err == nil {
		t.Fatal("expected error for non-result line")
	}
	r2, err := ParseResult(Line{Type: "result", Raw: []byte(`{"type":"result","permission_denials":[{"a":1},{"b":2}]}`)})
	if err != nil || r2.PermissionDenials != 2 {
		t.Fatalf("denials = %d, %v", r2.PermissionDenials, err)
	}
}

func TestParseRateLimitReal(t *testing.T) {
	rl, err := ParseRateLimit(fixtureLine(t, "rate_limit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if rl.Status != "allowed" {
		t.Fatalf("status = %q", rl.Status)
	}
	if rl.ResetsAt == nil || *rl.ResetsAt != 1793154000 {
		t.Fatalf("resetsAt = %v", rl.ResetsAt)
	}
	if rl.Utilization != nil {
		t.Fatalf("utilization = %v, want nil", *rl.Utilization)
	}
	if rl.UndocumentedType != "five_hour" {
		t.Fatalf("type = %q", rl.UndocumentedType)
	}
	w := rl.UndocumentedWindows["seven_day"]
	if w.Utilization == nil || *w.Utilization != 0.11 || w.ResetsAt == nil || *w.ResetsAt != 1793480400 {
		t.Fatalf("seven_day = %+v", w)
	}
}

func TestParseRateLimitTopLevelFields(t *testing.T) {
	raw := `{"type":"rate_limit_event","status":"allowed_warning","resetsAt":1793154000,"utilization":0.9,"rateLimitType":"seven_day","unifiedWindows":{"seven_day":{"utilization":0.11,"resetsAt":5}}}`
	l, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	rl, err := ParseRateLimit(l)
	if err != nil {
		t.Fatal(err)
	}
	if rl.Status != "allowed_warning" || rl.ResetsAt == nil || *rl.ResetsAt != 1793154000 ||
		rl.Utilization == nil || *rl.Utilization != 0.9 || rl.UndocumentedType != "seven_day" ||
		*rl.UndocumentedWindows["seven_day"].Utilization != 0.11 {
		t.Fatalf("unexpected: %+v", rl)
	}
}

func TestParseAPIRetry(t *testing.T) {
	l, err := Decode([]byte(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":30000,"error_status":429,"error":"rate_limit"}`))
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := ParseAPIRetry(l)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := APIRetry{Attempt: 1, MaxRetries: 10, RetryDelayMS: 30000, ErrorStatus: 429, Error: "rate_limit"}
	if a != want {
		t.Fatalf("got %+v", a)
	}
	init, _ := Decode([]byte(`{"type":"system","subtype":"init","session_id":"x"}`))
	if _, ok, err := ParseAPIRetry(init); ok || err != nil {
		t.Fatalf("init: ok=%v err=%v", ok, err)
	}
}

func TestDecodeIgnoresUnknown(t *testing.T) {
	l, err := Decode([]byte(`{"type":"brand_new","subtype":"x","whatever":{"a":[1,2]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if l.Type != "brand_new" || l.Subtype != "x" || len(l.Raw) == 0 {
		t.Fatalf("got %+v", l)
	}
	if _, err := Decode([]byte("not json")); err == nil {
		t.Fatal("expected error for non-JSON")
	}
}

func TestBlockedRequest(t *testing.T) {
	cases := []struct {
		in     string
		reason string
		ok     bool
	}{
		{"did stuff\nGOSPLAN_BLOCKED: spec contradicts tests", "spec contradicts tests", true},
		{"I wrote GOSPLAN_BLOCKED: nothing here", "", false},
		{"GOSPLAN_BLOCKED: first\nGOSPLAN_BLOCKED:   second  \n", "second", true},
		{"  GOSPLAN_BLOCKED: indented", "indented", true},
		{"", "", false},
		{"done\nGOSPLAN_BLOCKED: none — task complete.", "", false},
		{"done\nGOSPLAN_BLOCKED: none — completed", "", false},
		{"done\nGOSPLAN_BLOCKED: None", "", false},
		{"done\nGOSPLAN_BLOCKED: n/a", "", false},
		{"done\nGOSPLAN_BLOCKED:", "", false},
		{"done\nGOSPLAN_BLOCKED: no blockers.", "", false},
		{"done\nGOSPLAN_BLOCKED: (none)", "", false},
		{"done\nGOSPLAN_BLOCKED: none…", "", false},
		{"done\nGOSPLAN_BLOCKED: `none`", "", false},
		{"done\nGOSPLAN_BLOCKED: none (task complete)", "", false},
		{"done\nGOSPLAN_BLOCKED: None\u00a0— task complete", "", false},
		{"done\nGOSPLAN_BLOCKED: none ‒ done", "", false},
		{"done\nGOSPLAN_BLOCKED: nil", "", false},
		{"done\nGOSPLAN_BLOCKED: task complete", "", false},
		{"done\nGOSPLAN_BLOCKED: no issues", "", false},
		{"GOSPLAN_BLOCKED: spec missing\nmore text\nGOSPLAN_BLOCKED: none", "spec missing", true},
		{"GOSPLAN_BLOCKED: no blockers, but the spec is contradictory so I stopped", "no blockers, but the spec is contradictory so I stopped", true},
		{"GOSPLAN_BLOCKED: none; however tests cannot run", "none; however tests cannot run", true},
		{"GOSPLAN_BLOCKED: none: spec missing file X", "none: spec missing file X", true},
		{"GOSPLAN_BLOCKED: nothing: cannot continue", "nothing: cannot continue", true},
		{"GOSPLAN_BLOCKED: n/a - cannot run tests", "n/a - cannot run tests", true},
		{"GOSPLAN_BLOCKED: nonexistent dependency", "nonexistent dependency", true},
		{"GOSPLAN_BLOCKED: none of the tests can run", "none of the tests can run", true},
		{"GOSPLAN_BLOCKED: nothing to build against, spec missing", "nothing to build against, spec missing", true},
	}
	for _, c := range cases {
		got, ok := Result{Result: c.in}.BlockedRequest()
		if got != c.reason || ok != c.ok {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", c.in, got, ok, c.reason, c.ok)
		}
	}
}

func TestLastResult(t *testing.T) {
	r, ok, err := LastResult(filepath.Join("testdata", "stream.jsonl"))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Result != "second" || r.NumTurns != 2 || r.IsError {
		t.Fatalf("got %+v", r)
	}
	empty := filepath.Join(t.TempDir(), "e.jsonl")
	if err := os.WriteFile(empty, []byte(`{"type":"system","subtype":"init"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LastResult(empty); ok || err != nil {
		t.Fatalf("no-result file: ok=%v err=%v", ok, err)
	}
}
