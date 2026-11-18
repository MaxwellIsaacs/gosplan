// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// events_test.go: GREEN NOW. The M1 event log, driven only through
// `gosplan verify --log` and `gosplan replay`.
package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// honestRepo is a repo whose task branch passes (unrestricted).
func honestRepo(t *testing.T) *repo {
	t.Helper()
	r := newRepo(t, featureToml, map[string]string{"README": "r"})
	writeFile(t, r.dir, "src/feature.txt", "x")
	r.commit("feature")
	return r
}

func verifyLog(t *testing.T, r *repo, log string, extra ...string) result {
	t.Helper()
	args := append([]string{"verify", "--project", r.dir, "--task", "1.a", "--base", r.base, "--head", r.head(), "--log", log}, extra...)
	return gosplan(t, r.dir, []string{"TMPDIR=" + t.TempDir()}, args...)
}

var gateOrder = []string{"object_integrity", "committed_work", "locked_paths", "protected_paths", "scope", "commands_present", "clean_checkout", "cmd:test"}

// Envelope fields, launch/gate/verdict order, and --run in the envelope.
//
// Clause: 00-event-schema.md#EV-ENVELOPE
// Clause: 00-event-schema.md#EV-WRITER-ORDER
// Clause: 00-event-schema.md#EV-WRITER-RUNID
func TestEventEnvelopeAndWriterOrder(t *testing.T) {
	r := honestRepo(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	res := verifyLog(t, r, log, "--run", "20261028-010203-abcd", "--attempt", "1", "--task-base", r.base)
	if res.code != 0 {
		t.Fatalf("gosplan verify --log: want exit 0, got %s", res)
	}
	// Strict envelope: every line has exactly the documented keys.
	for i, line := range strings.Split(strings.TrimSpace(readFile(t, log)), "\n") {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d not JSON: %v", i+1, err)
		}
		for _, k := range []string{"v", "seq", "ts", "run", "type", "data"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("line %d lacks envelope key %q: %s", i+1, k, line)
			}
		}
		if d := strings.TrimSpace(string(m["data"])); !strings.HasPrefix(d, "{") {
			t.Fatalf("line %d: data must be an object, got %s", i+1, d)
		}
	}
	evs := readEvents(t, log)
	want := append(append([]string{"launch"}, make([]string, len(gateOrder))...), "verdict")
	for i := range gateOrder {
		want[i+1] = "gate"
	}
	if len(evs) != len(want) {
		t.Fatalf("want %d events (launch, %d gates, verdict), got %d", len(want), len(gateOrder), len(evs))
	}
	for i, ev := range evs {
		if ev.V != 1 || ev.Seq != int64(i+1) || ev.Run != "20261028-010203-abcd" || ev.Task != "1.a" || ev.Type != want[i] {
			t.Fatalf("event %d: got v=%d seq=%d run=%q task=%q type=%q; want v=1 seq=%d run from --run task 1.a type %s", i, ev.V, ev.Seq, ev.Run, ev.Task, ev.Type, i+1, want[i])
		}
		ts, err := time.Parse(time.RFC3339Nano, ev.TS)
		if err != nil || ts.Location() != time.UTC || !strings.HasSuffix(ev.TS, "Z") {
			t.Fatalf("event %d: ts %q is not RFC 3339 UTC", i, ev.TS)
		}
		if ev.Type == "gate" {
			var g struct{ Name, Status string }
			_ = json.Unmarshal(ev.Data, &g)
			if g.Name != gateOrder[i-1] {
				t.Fatalf("gate %d: want %s, got %s", i, gateOrder[i-1], g.Name)
			}
		}
	}
	var l struct {
		Attempt   int    `json:"attempt"`
		BaseSHA   string `json:"base_sha"`
		Model     string `json:"model"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(evs[0].Data, &l)
	if l.Attempt != 1 || l.BaseSHA != r.base || l.Model != "" || l.SessionID != "" {
		t.Fatalf("launch: only attempt and base_sha are filled, got %s", evs[0].Data)
	}
	var v struct {
		Result      string   `json:"result"`
		FailedGates []string `json:"failed_gates"`
		TreeSHA     string   `json:"tree_sha"`
	}
	_ = json.Unmarshal(evs[len(evs)-1].Data, &v)
	if v.Result != "pass" || len(v.FailedGates) != 0 || !fullSHA.MatchString(v.TreeSHA) {
		t.Fatalf("verdict data: %s", evs[len(evs)-1].Data)
	}

	// Without --attempt/--task-base there is no launch event.
	log2 := filepath.Join(t.TempDir(), "events.jsonl")
	if res := verifyLog(t, r, log2); res.code != 0 {
		t.Fatalf("second verify: %s", res)
	}
	if evs := readEvents(t, log2); len(evs) == 0 || evs[0].Type != "gate" {
		t.Fatalf("no --attempt/--task-base: first event must be a gate, got %+v", evs)
	}
}

// Two writers one after the other continue one seq.
//
// Clause: 00-event-schema.md#EV-SEQ
// Clause: 00-event-schema.md#EV-VERDICT-ONCE
func TestEventSeqContinuesAcrossWriters(t *testing.T) {
	r := honestRepo(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	cache := t.TempDir()
	for i := 0; i < 2; i++ {
		if res := verifyLog(t, r, log, "--run", "r1", "--cache", cache); res.code != 0 {
			t.Fatalf("verify %d: %s", i, res)
		}
	}
	evs := readEvents(t, log)
	verdicts := 0
	var hits []bool
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("position %d has seq %d: seq must be 1..n across writers", i+1, ev.Seq)
		}
		if ev.Type == "verdict" {
			verdicts++
			var d struct {
				CacheHit bool `json:"cache_hit"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			hits = append(hits, d.CacheHit)
		}
	}
	if verdicts != 2 || hits[0] || !hits[1] {
		t.Fatalf("want one verdict per verification, the second a cache hit; got %d verdicts, cache_hit %v", verdicts, hits)
	}
}

// Concurrent writers on one file never duplicate or skip a seq.
//
// Clause: events-writer#EV-FLOCK
// Clause: 00-event-schema.md#EV-SEQ
func TestEventConcurrentWritersKeepSeq(t *testing.T) {
	r := honestRepo(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	const writers = 4
	var wg sync.WaitGroup
	results := make([]result, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = verifyLog(t, r, log, "--run", fmt.Sprintf("w%d", i))
		}(i)
	}
	wg.Wait()
	for i, res := range results {
		if res.code != 0 {
			t.Fatalf("writer %d: %s", i, res)
		}
	}
	evs := readEvents(t, log)
	if len(evs) != writers*len(gateOrder)+writers {
		t.Fatalf("want %d events, got %d", writers*(len(gateOrder)+1), len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("position %d has seq %d", i+1, ev.Seq)
		}
	}
	if res := gosplan(t, "", nil, "replay", "--log", log); res.code != 0 {
		t.Fatalf("gosplan replay must accept the concurrently written log: %s", res)
	}
}

// A writer waits while another process holds the flock, then appends.
//
// Clause: events-writer#EV-FLOCK
func TestEventWriterWaitsForFlock(t *testing.T) {
	r := honestRepo(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(log, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nrBin, "verify", "--project", r.dir, "--task", "1.a", "--base", r.base, "--head", r.head(), "--log", log)
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("gosplan verify finished (%v) while another process held the log's flock", err)
	case <-time.After(3 * time.Second):
	}
	if fi, _ := os.Stat(log); fi.Size() != 0 {
		t.Fatalf("log written while locked by another process")
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gosplan verify after unlock: %v", err)
		}
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("gosplan verify did not finish after the lock was released")
	}
	if evs := readEvents(t, log); len(evs) != len(gateOrder)+1 || evs[0].Seq != 1 {
		t.Fatalf("after unlock: want %d events from seq 1, got %d", len(gateOrder)+1, len(evs))
	}
}

// A torn tail is quarantined, the log truncated and appending continues.
//
// Clause: events-writer#EV-TORN
// Clause: 00-event-schema.md#EV-SEQ
func TestEventTornTailQuarantined(t *testing.T) {
	r := honestRepo(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	if res := verifyLog(t, r, log, "--run", "r1"); res.code != 0 {
		t.Fatalf("first verify: %s", res)
	}
	good := readFile(t, log)
	n := len(readEvents(t, log))
	torn := `{"v":1,"seq":99,"ts":"2026-10-28T00:00:00Z","run":"r1","ty`
	if err := os.WriteFile(log, []byte(good+torn), 0o644); err != nil {
		t.Fatal(err)
	}
	res := verifyLog(t, r, log, "--run", "r1")
	if res.code != 0 {
		t.Fatalf("verify over a torn tail: want exit 0, got %s", res)
	}
	if !strings.Contains(res.stderr, "torn") && !strings.Contains(res.stderr, "unterminated") {
		t.Fatalf("want a warning on stderr about the torn tail, got %q", res.stderr)
	}
	after := readFile(t, log)
	if !strings.HasPrefix(after, good) || strings.Contains(after, torn) || !strings.HasSuffix(after, "\n") {
		t.Fatalf("log must keep the good prefix, drop the torn bytes and end in a newline")
	}
	evs := readEvents(t, log)
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("after repair position %d has seq %d", i+1, ev.Seq)
		}
	}
	if len(evs) != 2*n {
		t.Fatalf("want %d events after repair, got %d", 2*n, len(evs))
	}
	qs, _ := filepath.Glob(log + ".torn-*")
	if len(qs) != 1 || readFile(t, qs[0]) != torn {
		t.Fatalf("want exactly one %s.torn-<ts> holding the torn bytes, got %v", filepath.Base(log), qs)
	}
}

// A complete but invalid line is refused, never "repaired".
//
// Clause: events-writer#EV-BADLINE
func TestEventCompleteBadLineRefused(t *testing.T) {
	r := honestRepo(t)
	log := filepath.Join(t.TempDir(), "events.jsonl")
	if res := verifyLog(t, r, log, "--run", "r1"); res.code != 0 {
		t.Fatalf("first verify: %s", res)
	}
	bad := readFile(t, log) + "this is not json\n"
	if err := os.WriteFile(log, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := verifyLog(t, r, log, "--run", "r1"); res.code != 2 {
		t.Fatalf("complete bad line: want exit 2, got %s", res)
	}
	if readFile(t, log) != bad {
		t.Fatal("a refused log must be left unchanged")
	}
	if qs, _ := filepath.Glob(log + ".torn-*"); len(qs) != 0 {
		t.Fatalf("a complete line is not a torn tail: %v", qs)
	}
}

// Exit 2 (missing config at base) writes no gate or verdict.
//
// Clause: 00-event-schema.md#EV-WRITER-INFRA
func TestEventInfraErrorWritesNoVerdict(t *testing.T) {
	r := newRepo(t, "", map[string]string{"README": "r"})
	writeFile(t, r.dir, "src/feature.txt", "x")
	r.commit("feature")
	log := filepath.Join(t.TempDir(), "events.jsonl")
	if res := verifyLog(t, r, log, "--run", "r1"); res.code != 2 {
		t.Fatalf("missing nightrunner.toml at base: want exit 2, got %s", res)
	}
	if _, err := os.Stat(log); err == nil {
		for _, ev := range readEvents(t, log) {
			if ev.Type == "gate" || ev.Type == "verdict" {
				t.Fatalf("exit 2 must write no gate or verdict, found %s", ev.Type)
			}
		}
	}
}

// No --log, no file; log-only flags without --log are usage errors.
//
// Clause: 00-event-schema.md#EV-WRITER-NOLOG
func TestEventNoLogFlagNoFile(t *testing.T) {
	r := honestRepo(t)
	wd := t.TempDir()
	res := run(t, 3*time.Minute, wd, []string{"TMPDIR=" + t.TempDir()}, nrBin, "verify", "--project", r.dir, "--task", "1.a", "--base", r.base, "--head", r.head())
	if res.code != 0 {
		t.Fatalf("verify without --log: %s", res)
	}
	for _, d := range []string{wd, r.dir, filepath.Join(r.dir, ".nightrunner")} {
		if m, _ := filepath.Glob(filepath.Join(d, "*.jsonl")); len(m) != 0 {
			t.Fatalf("no --log must write no log, found %v", m)
		}
	}
	for _, flag := range [][]string{{"--run", "r1"}, {"--attempt", "1"}, {"--task-base", r.base}} {
		args := append([]string{"verify", "--project", r.dir, "--task", "1.a", "--base", r.base, "--head", r.head()}, flag...)
		if res := gosplan(t, wd, nil, args...); res.code != 2 {
			t.Fatalf("%v without --log: want exit 2, got %s", flag, res)
		}
	}
}

// writeLog writes hand-made events (envelope filled in) to a file.
func writeLog(t *testing.T, evs []map[string]any) string {
	t.Helper()
	var b strings.Builder
	for i, e := range evs {
		if _, ok := e["seq"]; !ok {
			e["seq"] = i + 1
		}
		if _, ok := e["v"]; !ok {
			e["v"] = 1
		}
		e["ts"], e["run"] = "2026-10-28T00:00:00Z", "r1"
		line, _ := json.Marshal(e)
		b.Write(append(line, '\n'))
	}
	p := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, filepath.Dir(p), filepath.Base(p), b.String())
	return p
}

func ts(task, from, to string) map[string]any {
	return map[string]any{"task": task, "type": "task_state", "data": map[string]any{"from": from, "to": to}}
}

// gosplan replay --check: 0 on match, 1 and the first differing task on mismatch.
//
// Clause: 00-event-schema.md#EV-REPLAY
func TestReplayCheck(t *testing.T) {
	log := writeLog(t, []map[string]any{ts("1.a", "", "pending"), ts("2.a", "", "pending"), ts("1.a", "pending", "running"), ts("1.a", "running", "passed")})
	dir := t.TempDir()
	writeFile(t, dir, "ok.json", `{"1.a":"passed","2.a":"pending"}`)
	writeFile(t, dir, "bad.json", `{"1.a":"passed","2.a":"running"}`)
	if res := gosplan(t, "", nil, "replay", "--log", log, "--state", filepath.Join(dir, "ok.json"), "--check"); res.code != 0 {
		t.Fatalf("matching state: want exit 0, got %s", res)
	}
	res := gosplan(t, "", nil, "replay", "--log", log, "--state", filepath.Join(dir, "bad.json"), "--check")
	if res.code != 1 || !strings.Contains(res.stdout, "2.a") {
		t.Fatalf("mismatch: want exit 1 naming task 2.a, got %s", res)
	}
}

// Reading a log rejects every invalid shape listed in the schema.
//
// Clause: 00-event-schema.md#EV-VALIDATE
func TestReplayRejectsInvalidLogs(t *testing.T) {
	cases := map[string][]map[string]any{
		"unknown v":        {{"v": 2, "task": "1.a", "type": "task_state", "data": map[string]any{"from": "", "to": "pending"}}},
		"seq gap":          {ts("1.a", "", "pending"), {"seq": 3, "task": "1.a", "type": "task_state", "data": map[string]any{"from": "pending", "to": "running"}}},
		"seq repeat":       {ts("1.a", "", "pending"), {"seq": 1, "task": "1.a", "type": "task_state", "data": map[string]any{"from": "pending", "to": "running"}}},
		"unknown type":     {{"task": "1.a", "type": "telepathy", "data": map[string]any{}}},
		"broken chain":     {ts("1.a", "", "pending"), ts("1.a", "running", "passed")},
		"first with from":  {ts("1.a", "pending", "running")},
		"pass with failed": {{"task": "1.a", "type": "verdict", "data": map[string]any{"tree_sha": "x", "result": "pass", "failed_gates": []string{"scope"}}}},
	}
	for name, evs := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			log := writeLog(t, evs)
			if res := gosplan(t, "", nil, "replay", "--log", log); res.code == 0 {
				t.Fatalf("%s: gosplan replay must reject the log, got %s", name, res)
			}
		})
	}
	ok := writeLog(t, []map[string]any{ts("1.a", "", "pending"), {"task": "1.a", "type": "verdict", "data": map[string]any{"tree_sha": "x", "result": "fail", "failed_gates": []string{"scope"}}}})
	if res := gosplan(t, "", nil, "replay", "--log", ok); res.code != 0 {
		t.Fatalf("a valid log must be accepted: %s", res)
	}
}
