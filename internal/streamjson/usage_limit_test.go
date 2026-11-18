package streamjson

import (
	"os"
	"path/filepath"
	"testing"
)

// The two lines claude 2.x printed when the five-hour window refused a
// session (trimmed to the fields that matter, ids dropped).
const (
	rejectedLine = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1795039200,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":1795039200,"status":"rejected"},"seven_day":{"utilization":0.07,"resetsAt":1795294800,"status":"allowed"}}}}`
	refusedLine  = `{"type":"result","subtype":"success","is_error":true,"num_turns":1,"duration_ms":750,"api_error_status":429,"api_error_type":"rate_limit_error","api_error_origin":"first_party","api_error":"usage_limit_reached","result":"You've hit your 5-hour limit · resets 5pm (America/New_York)"}`
)

func TestUsageLimited(t *testing.T) {
	for _, c := range []struct {
		name string
		line string
		want bool
	}{
		{"usage limit", refusedLine, true},
		{"429 without api_error", `{"type":"result","is_error":true,"api_error_status":429,"api_error_type":"rate_limit_error"}`, true},
		{"other api error", `{"type":"result","is_error":true,"api_error_status":500,"api_error_type":"api_error"}`, false},
		{"not an error", `{"type":"result","is_error":false,"api_error":"usage_limit_reached"}`, false},
		{"plain success", `{"type":"result","subtype":"success","is_error":false,"result":"done"}`, false},
	} {
		l, err := Decode([]byte(c.line))
		if err != nil {
			t.Fatal(err)
		}
		r, err := ParseResult(l)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.UsageLimited(); got != c.want {
			t.Errorf("%s: UsageLimited = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLastRejectedReset(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	allowed := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1795999999}}`

	at, ok, err := LastRejectedReset(write("refused.jsonl", "not json\n"+allowed+"\n"+rejectedLine+"\n"+refusedLine+"\n"))
	if err != nil || !ok || at != 1795039200 {
		t.Errorf("refused stream: at=%d ok=%v err=%v, want 1795039200 true nil", at, ok, err)
	}
	if _, ok, err := LastRejectedReset(write("allowed.jsonl", allowed+"\n")); err != nil || ok {
		t.Errorf("allowed-only stream: ok=%v err=%v, want false nil", ok, err)
	}
	if _, _, err := LastRejectedReset(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Error("missing file: want an error")
	}
}
