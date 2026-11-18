package events

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedClock(t *testing.T) func() time.Time {
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time { n++; return base.Add(time.Duration(n) * time.Second) }
}

func openTemp(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	l.now = fixedClock(t)
	t.Cleanup(func() { l.Close() })
	return l, path
}

func state(t *testing.T, l *Log, task, from, to string) {
	t.Helper()
	if _, err := l.Append(task, TaskState, TaskStateData{From: from, To: to}); err != nil {
		t.Fatal(err)
	}
}

func TestAppendReadRoundTrip(t *testing.T) {
	l, path := openTemp(t)
	state(t, l, "1.a", "", "running")
	state(t, l, "1.a", "running", "passed")
	if _, err := l.Append("1.a", Gate, GateData{Name: "committed_work", Status: GatePass}); err != nil {
		t.Fatal(err)
	}
	evs, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) || ev.V != SchemaVersion || ev.Run != "run-1" {
			t.Errorf("event %d envelope wrong: %+v", i, ev)
		}
	}
}

func TestReopenContinuesSeq(t *testing.T) {
	l, path := openTemp(t)
	state(t, l, "1.a", "", "running")
	l.Close()
	l2, err := Open(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	ev, err := l2.Append("1.a", TaskState, TaskStateData{From: "running", To: "passed"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 2 {
		t.Fatalf("seq after reopen = %d, want 2", ev.Seq)
	}
}

func TestOpenRefusesBrokenSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	bad := `{"v":1,"seq":1,"ts":"2026-10-26T12:00:00Z","run":"r","type":"run_started","data":{}}
{"v":1,"seq":3,"ts":"2026-10-26T12:00:01Z","run":"r","type":"run_started","data":{}}
`
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "r"); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("want ErrSeqGap, got %v", err)
	}
}

func TestRejectsUnknownType(t *testing.T) {
	l, _ := openTemp(t)
	if _, err := l.Append("", Type("bogus"), struct{}{}); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestValidateRejectsBrokenChain(t *testing.T) {
	l, path := openTemp(t)
	state(t, l, "1.a", "", "running")
	state(t, l, "1.a", "pending", "passed") // from does not match prior "running"
	_, err := ReadFile(path)
	if err == nil || !strings.Contains(err.Error(), "prior state was") {
		t.Fatalf("want chain error, got %v", err)
	}
}

func TestValidateRejectsFirstStateWithFrom(t *testing.T) {
	l, path := openTemp(t)
	state(t, l, "1.a", "running", "passed")
	if _, err := ReadFile(path); err == nil {
		t.Fatal("expected error: first state must have empty from")
	}
}

func TestReplayMatchesAndDiffs(t *testing.T) {
	l, path := openTemp(t)
	state(t, l, "1.a", "", "running")
	state(t, l, "1.a", "running", "passed")
	state(t, l, "1.b", "", "failed")
	evs, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := TaskStatuses(evs)
	if err != nil {
		t.Fatal(err)
	}
	recorded := map[string]string{"1.a": "passed", "1.b": "failed"}
	if d := Diff(got, recorded); d != "" {
		t.Fatalf("unexpected diff: %s", d)
	}
	recorded["1.b"] = "passed"
	if d := Diff(got, recorded); !strings.Contains(d, "1.b") {
		t.Fatalf("want diff naming 1.b, got %q", d)
	}
	recorded["1.b"] = "failed"
	recorded["9.z"] = "passed"
	if d := Diff(got, recorded); !strings.Contains(d, "9.z") {
		t.Fatalf("want diff naming 9.z, got %q", d)
	}
}

func TestValidateRejectsPassWithFailedGates(t *testing.T) {
	l, path := openTemp(t)
	if _, err := l.Append("1.a", Verdict, VerdictData{Result: "pass", FailedGates: []string{"scope"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path); err == nil || !strings.Contains(err.Error(), "failed gates") {
		t.Fatalf("want pass-with-failed-gates error, got %v", err)
	}
}

// quietWarnings captures repair warnings for the test's duration.
func quietWarnings(t *testing.T) *strings.Builder {
	t.Helper()
	var sb strings.Builder
	old := warnOut
	warnOut = &sb
	t.Cleanup(func() { warnOut = old })
	return &sb
}

func tornFiles(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(path + ".torn-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A write cut short by a crash leaves an unterminated last line. Open moves
// it aside, truncates to the last newline, and the log carries on.
func TestOpenQuarantinesTornTail(t *testing.T) {
	warn := quietWarnings(t)
	l, path := openTemp(t)
	state(t, l, "1.a", "", "running")
	state(t, l, "1.a", "running", "verifying")
	l.Close()
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	torn := `{"v":1,"seq":3,"ts":"2026-10-26T12:00:03Z","run":"run-1","ty`
	if err := os.WriteFile(path, append(append([]byte{}, good...), torn...), 0o644); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(path, "run-1")
	if err != nil {
		t.Fatalf("Open after torn write: %v", err)
	}
	defer l2.Close()
	if got, _ := os.ReadFile(path); string(got) != string(good) {
		t.Fatalf("log not truncated to last newline:\n%s", got)
	}
	q := tornFiles(t, path)
	if len(q) != 1 {
		t.Fatalf("want one quarantine file, got %v", q)
	}
	if got, _ := os.ReadFile(q[0]); string(got) != torn {
		t.Fatalf("quarantine holds %q, want %q", got, torn)
	}
	if !strings.Contains(warn.String(), q[0]) {
		t.Fatalf("warning does not name the quarantine file: %q", warn.String())
	}
	ev, err := l2.Append("1.a", TaskState, TaskStateData{From: "verifying", To: "passed"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 3 {
		t.Fatalf("seq after repair = %d, want 3", ev.Seq)
	}
	if _, err := ReadFile(path); err != nil {
		t.Fatalf("repaired log does not validate: %v", err)
	}
}

// A file with no newline at all is one torn line: it is quarantined whole.
// A second torn write gets its own quarantine file; neither is overwritten.
func TestOpenQuarantinesWholeTornFileTwice(t *testing.T) {
	quietWarnings(t)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	for i, torn := range []string{`{"v":1,"se`, `{"v":1,"seq":1,"ts"`} {
		if err := os.WriteFile(path, []byte(torn), 0o644); err != nil {
			t.Fatal(err)
		}
		l, err := Open(path, "r")
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		if fi, _ := os.Stat(path); fi.Size() != 0 {
			t.Fatalf("round %d: log size %d, want 0", i, fi.Size())
		}
		if q := tornFiles(t, path); len(q) != i+1 {
			t.Fatalf("round %d: quarantine files %v", i, q)
		}
	}
	var got []string
	for _, q := range tornFiles(t, path) {
		b, _ := os.ReadFile(q)
		got = append(got, string(b))
	}
	if j := strings.Join(got, "|"); !strings.Contains(j, `{"v":1,"se`) || !strings.Contains(j, `{"v":1,"seq":1,"ts"`) {
		t.Fatalf("quarantine contents = %v", got)
	}
}

// Only an unterminated tail is repaired. A complete line that is not valid
// JSON is corruption, not a torn write, and Open still refuses it.
func TestOpenRefusesCompleteBadLine(t *testing.T) {
	quietWarnings(t)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "r"); err == nil {
		t.Fatal("expected Open to refuse a complete invalid line")
	}
	if q := tornFiles(t, path); len(q) != 0 {
		t.Fatalf("complete line was quarantined: %v", q)
	}
}

// Two Log values on one file (an orchestrator and `gosplan verify --log`) used
// to keep seq in memory and write duplicates. Interleaved and concurrent
// appends must leave a file that validates with seq 1..n.
func TestTwoWritersKeepSeqContiguous(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	a, err := Open(path, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path, "run-b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Strict interleave: each writer appends after the other has.
	var want int64
	for i := 0; i < 10; i++ {
		l := a
		if i%2 == 1 {
			l = b
		}
		ev, err := l.Append("1.a", Gate, GateData{Name: "scope", Status: GatePass})
		if err != nil {
			t.Fatal(err)
		}
		want++
		if ev.Seq != want {
			t.Fatalf("append %d got seq %d, want %d", i, ev.Seq, want)
		}
	}

	// Concurrent: both writers race from their own goroutine.
	const per = 50
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, l := range []*Log{a, b} {
		wg.Add(1)
		go func(l *Log) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := l.Append("1.a", Gate, GateData{Name: "scope", Status: GatePass}); err != nil {
					errs <- err
					return
				}
			}
		}(l)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	evs, err := ReadFile(path)
	if err != nil {
		t.Fatalf("log does not validate: %v", err)
	}
	if len(evs) != 10+2*per {
		t.Fatalf("events = %d, want %d", len(evs), 10+2*per)
	}
}

// A writer that crashed mid-append leaves a torn tail; the other writer's
// next Append quarantines it rather than writing after the torn bytes.
func TestAppendRepairsTornTailFromOtherWriter(t *testing.T) {
	quietWarnings(t)
	l, path := openTemp(t)
	state(t, l, "1.a", "", "running")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"v":1,"seq":2,"ts"`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ev, err := l.Append("1.a", TaskState, TaskStateData{From: "running", To: "passed"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 2 {
		t.Fatalf("seq = %d, want 2", ev.Seq)
	}
	if _, err := ReadFile(path); err != nil {
		t.Fatalf("log does not validate: %v", err)
	}
	if q := tornFiles(t, path); len(q) != 1 {
		t.Fatalf("want one quarantine file, got %v", q)
	}
}
