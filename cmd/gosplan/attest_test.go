package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attestCapture runs f with stdout redirected and returns what it printed.
func attestCapture(t *testing.T, f func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		io.Copy(&b, r)
		done <- b.String()
	}()
	code := f()
	w.Close()
	os.Stdout = old
	return code, <-done
}

// attestFixture keygens into a temp dir, verifies a tiny repo with the real
// cmdVerify --json, attests the verdict and returns key, envelope paths.
func attestFixture(t *testing.T) (key, env string) {
	t.Helper()
	dir := t.TempDir()
	key = filepath.Join(dir, "k")
	if code := cmdKeygen([]string{"--out", key}); code != 0 {
		t.Fatalf("keygen exit %d", code)
	}
	proj, base, head := project(t, "src/feature.txt")
	code, out := attestCapture(t, func() int { return cmdVerify(append(verifyArgs(t, proj, base, head), "--json")) })
	if code != 0 {
		t.Fatalf("verify exit %d: %s", code, out)
	}
	vpath := filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(vpath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	env = filepath.Join(dir, "env.json")
	if code := cmdAttest([]string{"--verdict", vpath, "--key", key, "--out", env, "--run", "r1", "--attempt", "1"}); code != 0 {
		t.Fatalf("attest exit %d", code)
	}
	return key, env
}

func TestAttestCLIRoundTrip(t *testing.T) {
	key, env := attestFixture(t)
	code, out := attestCapture(t, func() int {
		return cmdVerifyAttestation([]string{"--envelope", env, "--pubkey", key + ".pub"})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"PASSED", "does not show that the tests ran", "GOSPLAN_GATE_committed_work"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if code, _ := attestCapture(t, func() int {
		return cmdVerifyAttestation([]string{"--envelope", env, "--pubkey", key + ".pub", "--tree", strings.Repeat("0", 40)})
	}); code != 1 {
		t.Fatalf("tree mismatch exit %d, want 1", code)
	}
}

func TestAttestCLITamperedExit1(t *testing.T) {
	key, env := attestFixture(t)
	data, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the first base64 character of the payload.
	i := bytes.Index(data, []byte(`"payload": "`)) + len(`"payload": "`)
	if data[i] == 'A' {
		data[i] = 'B'
	} else {
		data[i] = 'A'
	}
	if err := os.WriteFile(env, data, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := attestCapture(t, func() int {
		return cmdVerifyAttestation([]string{"--envelope", env, "--pubkey", key + ".pub"})
	})
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
}

func TestAttestCLIKeygenRefusesRepoPath(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	p := filepath.Join(repo, "keys", "k")
	if code := cmdKeygen([]string{"--out", p}); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	for _, f := range []string{p, p + ".pub"} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s was written", f)
		}
	}
}
