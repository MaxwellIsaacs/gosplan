package runner

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/attest"
	"github.com/MaxwellIsaacs/gosplan/internal/spec"
)

func TestWorkerArgv(t *testing.T) {
	key := "/home/u/.config/gosplan/attest/ed25519.key"
	argv := workerArgv("claude", "do the task", "11111111-2222-4333-8444-555555555555", "opus", 7)
	want := []string{"claude", "-p", "do the task",
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		"--session-id", "11111111-2222-4333-8444-555555555555",
		"--model", "opus", "--max-turns", "7"}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv:\n got %q\nwant %q", argv, want)
	}
	if strings.Contains(strings.Join(argv, " "), key) || strings.Contains(strings.Join(argv, " "), "attest") {
		t.Fatalf("argv mentions the attest key: %q", argv)
	}
}

func TestWorkerEnvFilter(t *testing.T) {
	base := []string{
		"HOME=/home/u", "PATH=/bin",
		"GOSPLAN_ATTEST_KEY=/k", "GOSPLAN_ATTEST=x", "GOSPLAN_ATTESTATION_DIR=/d",
		"NIGHTRUNNER_X=1", "NIGHTRUNNER_STATE=/s",
		"GOSPLAN_TASK=stale", "GOSPLAN_RUN=stale", "GOSPLAN_ATTEMPT=9",
		"GOSPLAN_OTHER=keep", "MY_NIGHTRUNNER_X=keep",
	}
	got := workerEnv(base, "r1", "2.b", 3)
	want := []string{"HOME=/home/u", "PATH=/bin", "GOSPLAN_OTHER=keep", "MY_NIGHTRUNNER_X=keep",
		"GOSPLAN_RUN=r1", "GOSPLAN_TASK=2.b", "GOSPLAN_ATTEMPT=3"}
	if !slices.Equal(got, want) {
		t.Fatalf("worker env:\n got %q\nwant %q", got, want)
	}
	if f := filterEnv(base); slices.ContainsFunc(f, func(kv string) bool {
		return strings.HasPrefix(kv, "GOSPLAN_ATTEST") || strings.HasPrefix(kv, "NIGHTRUNNER_")
	}) {
		t.Fatalf("filterEnv kept a secret: %q", f)
	}
}

func TestRetryDelay(t *testing.T) {
	if got := RetryDelay(100 * time.Millisecond)(3); got != 400*time.Millisecond {
		t.Fatalf("RetryDelay(100ms)(3) = %v, want 400ms", got)
	}
	if got := RetryDelay(10 * time.Minute)(1); got != 5*time.Minute {
		t.Fatalf("a base above the cap must be capped: %v", got)
	}
}

func TestRunLoadKeyRules(t *testing.T) {
	var errb bytes.Buffer
	missing := filepath.Join(t.TempDir(), "no-key")

	key, notes, err := loadKey(Options{Stderr: &errb})
	if err != nil || key != nil || !slices.Contains(notes, noteUnsigned) {
		t.Fatalf("no key: want unsigned with note, got key %v notes %v err %v", key != nil, notes, err)
	}
	key, notes, err = loadKey(Options{AttestKey: missing, Stderr: &errb})
	if err != nil || key != nil || !slices.Contains(notes, noteUnsigned) || !strings.Contains(errb.String(), "does not exist") {
		t.Fatalf("missing key: want unsigned + warning, got key %v notes %v err %v stderr %q", key != nil, notes, err, errb.String())
	}
	if _, _, err := loadKey(Options{AttestKey: missing, RequireAttest: true, Stderr: &errb}); err == nil {
		t.Fatalf("--require-attest with a missing key must fail")
	}
	if _, _, err := loadKey(Options{RequireAttest: true, Stderr: &errb}); err == nil {
		t.Fatalf("--require-attest without --attest-key must fail")
	}
	real := filepath.Join(t.TempDir(), "k")
	if err := attest.GenerateKey(real); err != nil {
		t.Fatal(err)
	}
	key, notes, err = loadKey(Options{AttestKey: real, RequireAttest: true, Stderr: &errb})
	if err != nil || key == nil || len(notes) != 0 {
		t.Fatalf("real key: want loaded, no notes; got key %v notes %v err %v", key != nil, notes, err)
	}
}

func TestRunReadyWaves(t *testing.T) {
	sp := &spec.Spec{Tasks: []spec.Task{
		{ID: "1.a", Tier: 1},
		{ID: "2.a", Tier: 2, Deps: []string{"1.a"}},
		{ID: "2.c", Tier: 2},
		{ID: "3.a", Tier: 3, Deps: []string{"2.a", "2.c"}},
	}}
	got := readyWaves(sp)
	want := [][]string{{"1.a", "2.c"}, {"2.a"}, {"3.a"}}
	if len(got) != len(want) {
		t.Fatalf("waves: got %v want %v", got, want)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("waves: got %v want %v", got, want)
		}
	}
}
