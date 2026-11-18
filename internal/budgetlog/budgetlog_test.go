package budgetlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func budgetlogPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "budget.jsonl")
}

// TestBudgetlogEnvelopeAndSharedSeq: two Logs on one file keep seq
// contiguous; every line has v=1, ts, run, task, type and an object data.
func TestBudgetlogEnvelopeAndSharedSeq(t *testing.T) {
	p := budgetlogPath(t)
	a, err := Open(p, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(p, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for i, l := range []*Log{a, b, a, b} {
		ev, err := l.Append("1.a", TypeDecision, map[string]any{"eid": i + 1})
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("append %d: seq %d", i, ev.Seq)
		}
	}
	evs, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("want 4 events, got %d", len(evs))
	}
	for _, ev := range evs {
		if ev.V != 1 || ev.Run != "r1" || ev.Task != "1.a" || ev.Type != TypeDecision || ev.TS.IsZero() {
			t.Fatalf("bad envelope: %+v", ev)
		}
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
}

// TestBudgetlogRejectsBadAppend: unknown types (including M1 types) and
// non-object data are refused and nothing is written.
func TestBudgetlogRejectsBadAppend(t *testing.T) {
	p := budgetlogPath(t)
	l, err := Open(p, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append("", "task_state", map[string]any{}); err == nil {
		t.Error("accepted M1 type task_state")
	}
	if _, err := l.Append("", "budget_snapshot", map[string]any{}); err == nil {
		t.Error("accepted budget_snapshot (not a budget event type)")
	}
	for _, d := range []any{[]int{1}, 3, "x", nil} {
		if _, err := l.Append("", TypeInput, d); err == nil {
			t.Errorf("accepted non-object data %v", d)
		}
	}
	if b, _ := os.ReadFile(p); len(b) != 0 {
		t.Fatalf("refused appends wrote %q", b)
	}
	for _, typ := range []string{TypeInput, TypeDecision, TypeSettle, TypeBreaker, TypeFit, TypeProbe, TypeHalt} {
		if !KnownType(typ) || !strings.HasPrefix(typ, "budget_") {
			t.Errorf("type %q", typ)
		}
	}
}

// TestBudgetlogTornTail: an unterminated last line is quarantined and the
// next append continues the seq from the last complete line.
func TestBudgetlogTornTail(t *testing.T) {
	p := budgetlogPath(t)
	l, err := Open(p, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append("", TypeProbe, map[string]any{"ran": true}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"v":1,"seq":2,"ty`)
	f.Close()
	var warn bytes.Buffer
	old := warnOut
	warnOut = &warn
	defer func() { warnOut = old }()
	l, err = Open(p, "r1")
	if err != nil {
		t.Fatalf("Open with torn tail: %v", err)
	}
	defer l.Close()
	ev, err := l.Append("", TypeHalt, map[string]any{"kind": "stop"})
	if err != nil || ev.Seq != 2 {
		t.Fatalf("append after repair: seq %d err %v", ev.Seq, err)
	}
	if !strings.Contains(warn.String(), "unterminated") {
		t.Errorf("no warning: %q", warn.String())
	}
	q, _ := filepath.Glob(p + ".torn-*")
	if len(q) != 1 {
		t.Fatalf("quarantine files: %v", q)
	}
	if b, _ := os.ReadFile(q[0]); string(b) != `{"v":1,"seq":2,"ty` {
		t.Fatalf("quarantined %q", b)
	}
	if _, err := ReadFile(p); err != nil {
		t.Fatal(err)
	}
}

// TestBudgetlogValidate: version, seq gaps, unknown types and non-object
// data are each rejected.
func TestBudgetlogValidate(t *testing.T) {
	ok := func(seq int64) Event {
		return Event{V: 1, Seq: seq, Run: "r", Type: TypeFit, Data: json.RawMessage(`{"pool":"five_hour"}`)}
	}
	if err := Validate([]Event{ok(1), ok(2)}); err != nil {
		t.Fatalf("valid log: %v", err)
	}
	gap := []Event{ok(1), ok(3)}
	if err := Validate(gap); !errors.Is(err, ErrSeqGap) {
		t.Errorf("gap: %v", err)
	}
	v2 := ok(1)
	v2.V = 2
	typ := ok(1)
	typ.Type = "verdict"
	arr := ok(1)
	arr.Data = json.RawMessage(`[1]`)
	null := ok(1)
	null.Data = json.RawMessage(`null`)
	for name, ev := range map[string]Event{"version": v2, "type": typ, "array data": arr, "null data": null} {
		if err := Validate([]Event{ev}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Open refuses a file whose envelope is already broken.
	p := budgetlogPath(t)
	os.WriteFile(p, []byte(`{"v":1,"seq":1,"ts":"2026-11-05T00:00:00Z","run":"r","type":"verdict","data":{}}`+"\n"), 0o600)
	if l, err := Open(p, "r"); err == nil {
		l.Close()
		t.Fatal("Open accepted a log with an unknown type")
	}
}
