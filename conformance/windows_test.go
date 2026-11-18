// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// windows_test.go: RED BY DESIGN. Observed rate-limit windows
// from the recorded claude 2.1.305 samples in
// testdata/samples.
// Rows are read from gosplan.db's window_observations (documented DDL).
package conformance

import (
	"strings"
	"testing"
)

// winRows returns "window|source|documented|utilization|resets_at|status"
// rows, NULLs as "-", sorted.
func winRows(t *testing.T, f *runFix) string {
	t.Helper()
	return sql(t, f.db(), "SELECT window||'|'||source||'|'||documented||'|'||ifnull(utilization,'-')||'|'||ifnull(resets_at,'-')||'|'||ifnull(status,'-') FROM window_observations ORDER BY 1;")
}

func hasRow(rows, prefix string) bool {
	for _, l := range strings.Split(rows, "\n") {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// TestWindowsFromRateLimitEvent: the recorded rate_limit_event yields the
// rate_limit_info row and one undocumented row per unifiedWindows entry.
// Clause: orchestrator#DY-WINDOW-STREAM
func TestWindowsFromRateLimitEvent(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.sample("rate_limit_event.jsonl", "rl.jsonl")
	f.scenario(map[string][]map[string]any{"1.a": {{"op": "emit_file", "path": "rl.jsonl"}, {"op": "commit"}}})
	mustExit(t, "gosplan run (worker emits rate_limit_event)", f.runNR(), 0)
	rows := winRows(t, f)
	for _, want := range []string{
		"five_hour|rate_limit_event|0|-|1793154000|allowed",
		"five_hour|rate_limit_event.unifiedWindows|0|0.42|1793154000|",
		"seven_day|rate_limit_event.unifiedWindows|0|0.11|1793480400|",
	} {
		if !hasRow(rows, want) {
			t.Fatalf("window_observations lacks %q; rows:\n%s", want, rows)
		}
	}
}

// TestWindowsFallbackFields: missing fields degrade (unknown window,
// documented), api_retry is recorded, junk lines are ignored, the task passes.
// Clause: orchestrator#DY-WINDOW-FALLBACK
func TestWindowsFallbackFields(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	writeFile(t, f.fake, "lines.jsonl", strings.Join([]string{
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1793154000}}`,
		`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":429,"error":"rate_limit"}`,
		`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":500,"error":"server_error"}`,
		`{"type":"rate_limit_event","rate_limit_info":"garbage"}`,
		`not json at all`,
	}, "\n")+"\n")
	f.scenario(map[string][]map[string]any{"1.a": {{"op": "emit_file", "path": "lines.jsonl"}, {"op": "commit"}}})
	mustExit(t, "gosplan run (worker emits partial/junk lines)", f.runNR(), 0)
	r, _ := f.status()
	if s := r.task(t, "1.a").Status; s != "passed" {
		t.Fatalf("1.a: junk stream lines must not fail the task, got %s", s)
	}
	rows := winRows(t, f)
	for _, want := range []string{
		"unknown|rate_limit_event|1|-|1793154000|allowed_warning",
		"unknown|api_retry|1|-|-|rate_limited",
	} {
		if !hasRow(rows, want) {
			t.Fatalf("window_observations lacks %q; rows:\n%s", want, rows)
		}
	}
	if n := strings.Count(rows, "|api_retry|"); n != 1 {
		t.Fatalf("only error=rate_limit api_retry lines are windows; got %d api_retry rows:\n%s", n, rows)
	}
}

// TestWindowsUsageProbe: the $0 /usage fallback parses the recorded text.
// Clause: orchestrator#DY-WINDOW-USAGE
func TestWindowsUsageProbe(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.sample("usage.json", "usage.json")
	mustExit(t, "gosplan run --usage-probe=true", f.runNR("--usage-probe=true"), 0)
	probed := false
	for _, r := range f.ledger() {
		probed = probed || r.Event == "usage"
	}
	if !probed {
		t.Fatalf("--usage-probe=true on an empty store never called claude -p /usage")
	}
	rows := winRows(t, f)
	for _, want := range []string{
		"session|usage_text|0|0.43|-|",
		"week_all|usage_text|0|0.11|-|",
		"week_fable|usage_text|0|0.0|-|",
	} {
		if !hasRow(rows, want) {
			t.Fatalf("window_observations lacks %q; rows:\n%s", want, rows)
		}
	}
}

// TestWindowsUsageParseFailed: /usage text with no known line records one
// _probe row with status parse_failed.
// Clause: orchestrator#DY-WINDOW-USAGE
func TestWindowsUsageParseFailed(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	writeFile(t, f.fake, "usage.json", `{"type":"result","subtype":"success","is_error":false,"result":"Usage is shown on the web now."}`)
	mustExit(t, "gosplan run --usage-probe=true", f.runNR("--usage-probe=true"), 0)
	rows := winRows(t, f)
	if !hasRow(rows, "_probe|") || !strings.Contains(rows, "|parse_failed") {
		t.Fatalf("want a _probe row with status parse_failed; rows:\n%s", rows)
	}
}

// TestWindowsProbeOff: --usage-probe=false never runs /usage.
// Clause: orchestrator#DY-WINDOW-USAGE
func TestWindowsProbeOff(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	f.sample("usage.json", "usage.json")
	mustExit(t, "gosplan run --usage-probe=false", f.runNR("--usage-probe=false"), 0)
	for _, r := range f.ledger() {
		if r.Event == "usage" {
			t.Fatalf("--usage-probe=false still called claude -p /usage")
		}
	}
	if n := sql(t, f.db(), "SELECT count(*) FROM window_observations WHERE source='usage_text' OR window='_probe';"); n != "0" {
		t.Fatalf("--usage-probe=false recorded %s usage rows", n)
	}
}
