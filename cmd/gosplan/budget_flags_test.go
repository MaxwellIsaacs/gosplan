package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBudgetRunHelp: `gosplan run -h` lists the seven budget flags, says the
// belt is a list-price estimate that does not cap plan usage, and that
// budget defaults are policy.
func TestBudgetRunHelp(t *testing.T) {
	f := &runFix{t: t, bins: runBinaries(t)}
	res := f.exec(nil, "run", "-h")
	out := res.stdout + res.stderr
	for _, s := range []string{"-budget ", "-budget-config", "-deadline", "-cap-window", "-cap-usd-estimate", "-max-verify-errors", "-stall-quiet-max",
		"--max-budget-usd is a client-side list-price estimate; it does not cap plan usage.", "policy"} {
		if !strings.Contains(out, s) {
			t.Fatalf("gosplan run -h lacks %q:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "(default 3)") || !strings.Contains(out, "(default 15m0s)") {
		t.Fatalf("gosplan run -h: --max-verify-errors 3 and --stall-quiet-max 15m defaults missing:\n%s", out)
	}
}

// TestBudgetRunUsageErrors: malformed budget flags and configs are usage
// errors (exit 2) before any task runs.
func TestBudgetRunUsageErrors(t *testing.T) {
	f := bgFix(t, "1.a", nil)
	noMode := filepath.Join(t.TempDir(), "budget.toml")
	write(t, filepath.Dir(noMode), "budget.toml", "[budget]\njitter = \"0s\"\n")
	noUnit := filepath.Join(t.TempDir(), "budget.toml")
	write(t, filepath.Dir(noUnit), "budget.toml", "[budget]\nmode = \"enforce\"\n")
	observeFile := filepath.Join(t.TempDir(), "budget.toml")
	write(t, filepath.Dir(observeFile), "budget.toml", "[budget]\nmode = \"observe\"\n")
	offFile := filepath.Join(t.TempDir(), "budget.toml")
	write(t, filepath.Dir(offFile), "budget.toml", "[budget]\nmode = \"off\"\n")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--deadline", "25:99"}, "deadline"},
		{[]string{"--deadline", "tomorrow"}, "deadline"},
		{[]string{"--cap-window", "five_hour=2"}, "cap-window"},
		{[]string{"--cap-window", "five_hour"}, "cap-window"},
		{[]string{"--cap-usd-estimate", "-1"}, "cap-usd-estimate"},
		{[]string{"--budget", "strict"}, "budget"},
		{[]string{"--budget-config", noMode}, "budget.mode must be set"},
		{[]string{"--budget-config", noUnit}, "unit_fraction"},
		{[]string{"--budget-config", observeFile, "--budget", "enforce"}, "unit_fraction"},
		{[]string{"--budget", "off", "--cap-usd-estimate", "1"}, "not off"},
		{[]string{"--budget", "off", "--deadline", "23:59"}, "not off"},
		{[]string{"--budget", "off", "--cap-window", "five_hour=0.5"}, "not off"},
		{[]string{"--budget-config", offFile, "--cap-usd-estimate", "1"}, "not off"},
	} {
		res := f.brun(tc.args...)
		if res.code != 2 || !strings.Contains(res.stderr, tc.want) {
			t.Fatalf("gosplan run %v: want exit 2 naming %q, got %d\n%s", tc.args, tc.want, res.code, res.stderr)
		}
	}
	if spans := bgSpans(t, f); len(spans) != 0 {
		t.Fatalf("a usage error started %d workers", len(spans))
	}
}

// TestBudgetParseDeadline: RFC 3339, or HH:MM as the next such local time.
func TestBudgetParseDeadline(t *testing.T) {
	now := time.Date(2026, 11, 5, 22, 30, 0, 0, time.Local)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"23:15", time.Date(2026, 11, 5, 23, 15, 0, 0, time.Local)},
		{"06:00", time.Date(2026, 11, 6, 6, 0, 0, 0, time.Local)},
		{"22:30", time.Date(2026, 11, 6, 22, 30, 0, 0, time.Local)},
		{"2026-11-06T01:02:03Z", time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)},
	} {
		got, err := parseDeadline(tc.in, now)
		if err != nil || !got.Equal(tc.want) {
			t.Fatalf("parseDeadline(%q): got %v %v, want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"25:99", "24:00", "7:5", "12:60", "", "noon"} {
		if _, err := parseDeadline(bad, now); err == nil {
			t.Fatalf("parseDeadline(%q): want an error", bad)
		}
	}
}
