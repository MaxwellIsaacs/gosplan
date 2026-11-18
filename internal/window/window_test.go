package window

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/state"
)

// fakeBin is the conformance suite's fake claude; fakeDir holds the
// usage.json it prints for `claude -p /usage`.
var fakeBin, fakeDir string

// usageText is a recorded /usage reply (undocumented format).
const usageText = "You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 43% used · resets Oct 27, 10:20pm (America/New_York)\nCurrent week (all models): 11% used · resets Oct 31, 5pm (America/New_York)\nCurrent week (Fable): 0% used · resets Oct 31, 4:59pm (America/New_York)\n\nWhat's contributing to your limits usage?"

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "window-fake")
	if err != nil {
		panic(err)
	}
	fakeBin, fakeDir = filepath.Join(dir, "fakeclaude"), dir
	usage, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"num_turns": 0, "local_command": "usage", "result": usageText})
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), usage, 0o644); err != nil {
		panic(err)
	}
	if out, err := exec.Command("go", "build", "-o", fakeBin, "../../conformance/fakeclaude").CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var t0 = time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC)

const realUsage = "You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 43% used · resets Oct 27, 10:20pm (America/New_York)\nCurrent week (all models): 11% used · resets Oct 31, 5pm (America/New_York)\nCurrent week (Fable): 0% used · resets Oct 31, 4:59pm (America/New_York)\n\nWhat's contributing to your limits usage?"

func byWindow(obs []state.WindowObservation) map[string]state.WindowObservation {
	m := map[string]state.WindowObservation{}
	for _, o := range obs {
		m[o.Window+"|"+o.Source] = o
	}
	return m
}

func TestFromLineRealRateLimit(t *testing.T) {
	data, err := os.ReadFile("testdata/rate_limit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	aid := int64(7)
	obs := FromLine(bytes.TrimSpace(data), &aid, t0)
	if len(obs) != 3 {
		t.Fatalf("got %d rows: %+v", len(obs), obs)
	}
	m := byWindow(obs)
	info := m["five_hour|rate_limit_event"]
	if info.Documented || info.Status != "allowed" || *info.ResetsAt != 1793154000 || info.Utilization != nil {
		t.Errorf("info row: %+v", info)
	}
	fh := m["five_hour|rate_limit_event.unifiedWindows"]
	sd := m["seven_day|rate_limit_event.unifiedWindows"]
	if fh.Documented || sd.Documented {
		t.Error("unifiedWindows rows must be documented=false")
	}
	if *fh.Utilization != 0.42 || *sd.Utilization != 0.11 || *sd.ResetsAt != 1793480400 {
		t.Errorf("unified rows: %+v %+v", fh, sd)
	}
	for _, o := range obs {
		if o.AttemptID == nil || *o.AttemptID != 7 || !o.ObservedAt.Equal(t0) || !strings.Contains(o.Raw, "unifiedWindows") {
			t.Errorf("attempt/time/raw: %+v", o)
		}
	}
}

func TestFromLineDocumentedOnly(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1793154000,"utilization":0.9}}`
	obs := FromLine([]byte(line), nil, t0)
	if len(obs) != 1 {
		t.Fatalf("got %+v", obs)
	}
	o := obs[0]
	if o.Window != "unknown" || !o.Documented || o.Status != "allowed_warning" || *o.Utilization != 0.9 || *o.ResetsAt != 1793154000 {
		t.Errorf("row: %+v", o)
	}
}

func TestFromLineAPIRetry(t *testing.T) {
	mk := func(e string) []byte {
		return []byte(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":5,"retry_delay_ms":500,"error_status":429,"error":"` + e + `"}`)
	}
	obs := FromLine(mk("rate_limit"), nil, t0)
	if len(obs) != 1 || obs[0].Window != "unknown" || obs[0].Status != "rate_limited" || obs[0].Source != "api_retry" || !obs[0].Documented {
		t.Fatalf("rate_limit: %+v", obs)
	}
	if got := FromLine(mk("overloaded"), nil, t0); got != nil {
		t.Errorf("overloaded: %+v", got)
	}
}

func TestFromLineIgnoresOthers(t *testing.T) {
	for _, l := range []string{
		`{"type":"system","subtype":"init","session_id":"x"}`,
		`{"type":"assistant","message":{}}`,
		`{"type":"result","subtype":"success","result":"ok"}`,
		`not json at all`, ``, `{"type":`,
	} {
		if got := FromLine([]byte(l), nil, t0); got != nil {
			t.Errorf("%q → %+v", l, got)
		}
	}
}

func TestFromLineTruncatesRaw(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"pad":"` + strings.Repeat("x", 20000) + `"}`
	obs := FromLine([]byte(line), nil, t0)
	if len(obs) != 1 || len(obs[0].Raw) != 8192 {
		t.Fatalf("raw len %d", len(obs[0].Raw))
	}
}

func TestParseUsageTextReal(t *testing.T) {
	obs := ParseUsageText(realUsage, t0)
	if len(obs) != 3 {
		t.Fatalf("got %+v", obs)
	}
	want := map[string]float64{"session": 0.43, "week_all": 0.11, "week_fable": 0}
	for _, o := range obs {
		w, ok := want[o.Window]
		if !ok || *o.Utilization != w || o.Documented || o.ResetsAt != nil || o.Source != "usage_text" || !strings.HasPrefix(o.Raw, "Current ") {
			t.Errorf("row %+v", o)
		}
		delete(want, o.Window)
	}
	if len(want) != 0 {
		t.Errorf("missing %v", want)
	}
}

func TestParseUsageTextChanged(t *testing.T) {
	obs := ParseUsageText("Something entirely different\nusage: lots", t0)
	if len(obs) != 1 || obs[0].Window != "_probe" || obs[0].Status != "parse_failed" {
		t.Fatalf("got %+v", obs)
	}
	obs = ParseUsageText("current   session:  7.5% used - resets later", t0)
	if len(obs) != 1 || obs[0].Window != "session" || *obs[0].Utilization != 0.075 {
		t.Fatalf("got %+v", obs)
	}
	obs = ParseUsageText("Current week (Some Model): 50% used", t0)
	if len(obs) != 1 || obs[0].Window != "week_some_model" {
		t.Fatalf("got %+v", obs)
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		12 * time.Minute: "12m", 3 * time.Hour: "3h", 50 * time.Hour: "2d", -time.Minute: "0m",
	} {
		if got := Age(t0, t0.Add(-d)); got != want {
			t.Errorf("%v → %q, want %q", d, got, want)
		}
	}
}

func newStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "gosplan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProberDue(t *testing.T) {
	now := t0
	obsAt := func(d time.Duration) state.WindowObservation {
		return state.WindowObservation{Window: "session", Source: "rate_limit_event", ObservedAt: now.Add(-d), Raw: "x"}
	}
	probeAt := func(d time.Duration) state.WindowObservation {
		return state.WindowObservation{Window: "_probe", Status: "error", Source: "usage_text", ObservedAt: now.Add(-d), Raw: "x"}
	}
	for _, tc := range []struct {
		name string
		rows []state.WindowObservation
		want bool
	}{
		{"none", nil, true},
		{"fresh observation", []state.WindowObservation{obsAt(5 * time.Minute)}, false},
		{"stale obs, recent probe", []state.WindowObservation{obsAt(2 * time.Hour), probeAt(5 * time.Minute)}, false},
		{"both stale", []state.WindowObservation{obsAt(2 * time.Hour), probeAt(time.Hour)}, true},
		{"stale obs, no probe", []state.WindowObservation{obsAt(2 * time.Hour)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			if err := s.RecordWindows(context.Background(), tc.rows); err != nil {
				t.Fatal(err)
			}
			got, err := Prober{Now: func() time.Time { return now }}.Due(context.Background(), s)
			if err != nil || got != tc.want {
				t.Errorf("Due = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestProberUsesFake(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	p := Prober{ClaudeBin: fakeBin, Env: append(os.Environ(), "CONF_FAKE_DIR="+fakeDir), Now: func() time.Time { return t0 }}
	obs, err := p.Probe(ctx, s)
	if err != nil || len(obs) != 3 {
		t.Fatalf("probe: %v %+v", err, obs)
	}
	latest, _ := s.LatestWindows(ctx)
	m := map[string]float64{}
	for _, o := range latest {
		m[o.Window] = *o.Utilization
	}
	if m["session"] != 0.43 || m["week_all"] != 0.11 {
		t.Errorf("latest %v", m)
	}

	// A binary that exits 1 records an error probe row and reports the error.
	s2 := newStore(t)
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755)
	obs, err = Prober{ClaudeBin: bad, Now: func() time.Time { return t0 }}.Probe(ctx, s2)
	if err == nil || len(obs) != 1 || obs[0].Window != "_probe" || obs[0].Status != "error" || !strings.Contains(obs[0].Raw, "boom") {
		t.Fatalf("bad: %v %+v", err, obs)
	}
	if at, _ := s2.LastProbeAt(ctx); at.IsZero() {
		t.Error("error probe not recorded")
	}
}

func TestProberTimeout(t *testing.T) {
	s := newStore(t)
	slow := filepath.Join(t.TempDir(), "slow")
	os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	start := time.Now()
	obs, err := Prober{ClaudeBin: slow, Timeout: 500 * time.Millisecond}.Probe(context.Background(), s)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %v", el)
	}
	if err == nil || len(obs) != 1 || obs[0].Window != "_probe" || obs[0].Status != "error" {
		t.Fatalf("got %v %+v", err, obs)
	}
}
