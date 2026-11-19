package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWhy(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(
		`{"type":"run_started","data":{}}`+"\n"+
			`{"type":"run_finished","data":{"status":"halted","reason":"environment: task 1.a: verify gate test exited 127 (command not found): nix"}}`+"\n"), 0o600)
	logDir := filepath.Join(dir, "verify-logs", "1.a", "2", "1")
	os.MkdirAll(logDir, 0o700)
	os.WriteFile(filepath.Join(logDir, "1.a.test.log"), []byte("running\nbash: line 1: nix: command not found\n\n"), 0o600)
	r := Report{Run: "R", Status: "halted", SpecDir: "/spec", Project: "/proj", Tasks: []Task{
		{ID: "1.a", Status: "failed", Attempts: []Attempt{
			{N: 1, Verdict: &Verdict{Result: "fail", FailedGates: []string{"scope"}}},
			{N: 2, Verdict: &Verdict{Result: "fail", FailedGates: []string{"cmd:test"}}},
		}},
		{ID: "2.a", Status: "passed"},
	}}
	got := Why(r, dir, map[string]string{"1.a": "attempt 2 failed verification.\nfailed gates: cmd:test"})
	for _, want := range []string{
		"halted: environment: task 1.a: verify gate test exited 127",
		"--continue R",
		"1.a failed: attempt 2 failed verification.",
		"last log line: bash: line 1: nix: command not found",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Why lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "2.a") {
		t.Errorf("Why mentions a passed task:\n%s", got)
	}
	if Why(Report{Status: "complete"}, dir, nil) != "" {
		t.Error("Why says something about a complete run")
	}
}
