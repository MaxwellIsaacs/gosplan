// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// mutation_test.go: RED BY DESIGN. The advisory gremlins run.
// gremlins is a fake shell script.
package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeGremlins writes a script that logs its argv, writes an all-lived
// report to the -o path and exits 10 (below threshold).
func fakeGremlins(t *testing.T) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "gremlins")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + argvLog + `'
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
[ -n "$out" ] && cat > "$out" <<'EOF'
{"go_module":"fixture","test_efficacy":0,"mutations_coverage":100,"mutants_total":6,"mutants_killed":0,"mutants_lived":6,"mutants_not_viable":0,"mutants_not_covered":0,"elapsed_time":1,"files":[{"file_name":"a.go","mutations":[{"type":"CONDITIONALS_NEGATION","status":"LIVED","line":1,"column":1}]}]}
EOF
exit 10
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

// TestMutationAdvisoryNeverDecides: gremlins exiting 10 with zero efficacy
// changes no status: the run completes and mutation is marked advisory.
// Clause: orchestrator#DY-MUTATION-ADVISORY
func TestMutationAdvisoryNeverDecides(t *testing.T) {
	bin, _ := fakeGremlins(t)
	f := newRunFix(t, "1.a -> 2.a", nil)
	mustExit(t, "gosplan run --mutation advisory", f.runNR("--mutation", "advisory", "--gremlins", bin), 0)
	r, _ := f.status()
	for _, x := range r.Tasks {
		if x.Status != "passed" {
			t.Fatalf("task %s: mutation must not change status, got %s", x.ID, x.Status)
		}
		if len(x.Attempts) == 0 {
			t.Fatalf("task %s: no attempts in the report", x.ID)
		}
		last := x.Attempts[len(x.Attempts)-1]
		if last.Mutation == nil || last.Mutation["advisory"] != true {
			t.Fatalf("task %s: passed attempt must carry mutation with advisory: true, got %+v", x.ID, last.Mutation)
		}
	}
}

// TestMutationArgvNoThresholdFlags: gremlins is called with --diff <base>,
// --config and -o, never a --threshold-* flag.
// Clause: orchestrator#DY-MUTATION-ARGV
func TestMutationArgvNoThresholdFlags(t *testing.T) {
	bin, argvLog := fakeGremlins(t)
	f := newRunFix(t, "1.a", nil)
	mustExit(t, "gosplan run --mutation advisory", f.runNR("--mutation", "advisory", "--gremlins", bin), 0)
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("gremlins was never called after a pass")
	}
	argv := string(b)
	if !regexp.MustCompile(`(^|\s)unleash\s`).MatchString(argv) || !regexp.MustCompile(`--diff[ =][0-9a-f]{40}`).MatchString(argv) ||
		!strings.Contains(argv, "--config") || !strings.Contains(argv, "-o ") {
		t.Fatalf("gremlins argv lacks unleash/--diff <sha>/--config/-o: %s", argv)
	}
	if strings.Contains(argv, "--threshold") {
		t.Fatalf("gremlins argv must not carry --threshold-* flags: %s", argv)
	}
}

// TestMutationUnavailableBinary: a missing gremlins is status unavailable and
// the run still completes.
// Clause: orchestrator#DY-MUTATION-ARGV
// Clause: orchestrator#DY-MUTATION-ADVISORY
func TestMutationUnavailableBinary(t *testing.T) {
	f := newRunFix(t, "1.a", nil)
	mustExit(t, "gosplan run --gremlins <missing>", f.runNR("--mutation", "advisory", "--gremlins", filepath.Join(t.TempDir(), "no-gremlins")), 0)
	r, _ := f.status()
	x := r.task(t, "1.a")
	if x.Status != "passed" || len(x.Attempts) == 0 || x.Attempts[0].Mutation == nil || x.Attempts[0].Mutation["status"] != "unavailable" {
		t.Fatalf("1.a: want passed with mutation status unavailable, got %s %+v", x.Status, x.Attempts)
	}
}
