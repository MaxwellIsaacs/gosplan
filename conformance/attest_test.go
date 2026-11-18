// PROTECTED. A human reviews every change to this file
// by hand. Later operations must not edit, weaken, skip or delete any test
// in conformance/ to make it pass: the red tests are the target.
//
// attest_test.go: RED BY DESIGN. Signed verdicts: `gosplan keygen`, `gosplan attest`,
// `gosplan verify-attestation`. The envelope is read as documented DSSE JSON.
package conformance

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type dsse struct {
	PayloadType string `json:"payloadType"`
	Payload     string `json:"payload"`
	Signatures  []struct {
		KeyID string `json:"keyid"`
		Sig   string `json:"sig"`
	} `json:"signatures"`
}

type statement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	Predicate struct {
		Verifier struct {
			ID string `json:"id"`
		} `json:"verifier"`
		VerificationResult string   `json:"verificationResult"`
		VerifiedLevels     []string `json:"verifiedLevels"`
		Policy             struct {
			URI string `json:"uri"`
		} `json:"policy"`
	} `json:"predicate"`
}

// keygen makes a key outside any git work tree and returns its path.
func keygen(t *testing.T) string {
	t.Helper()
	k := filepath.Join(t.TempDir(), "keys", "ed25519.key")
	res := gosplan(t, t.TempDir(), nil, "keygen", "--out", k)
	if res.code != 0 {
		t.Fatalf("gosplan keygen: want exit 0, got %s", res)
	}
	return k
}

// signedGolden signs the `gosplan verify --json` verdict of golden fixture name.
func signedGolden(t *testing.T, name, key string) (env string, v verdict) {
	t.Helper()
	g := goldenFixture(t, name)
	v, res := g.verify(t)
	if res.code != 0 && res.code != 1 {
		t.Fatalf("gosplan verify on golden %s: %s", name, res)
	}
	dir := t.TempDir()
	vf := filepath.Join(dir, "verdict.json")
	writeFile(t, dir, "verdict.json", res.stdout)
	env = filepath.Join(dir, "verdict.dsse.json")
	if res := gosplan(t, dir, nil, "attest", "--verdict", vf, "--key", key, "--out", env, "--run", "r1", "--attempt", "1"); res.code != 0 {
		t.Fatalf("gosplan attest: want exit 0, got %s", res)
	}
	return env, v
}

func decodeEnvelope(t *testing.T, path string) (dsse, statement) {
	t.Helper()
	var e dsse
	if err := json.Unmarshal([]byte(readFile(t, path)), &e); err != nil {
		t.Fatalf("envelope is not DSSE JSON: %v", err)
	}
	p, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	var s statement
	if err := json.Unmarshal(p, &s); err != nil {
		t.Fatalf("payload is not an in-toto statement: %v", err)
	}
	return e, s
}

func verifyAttestation(t *testing.T, env, pub string, extra ...string) result {
	t.Helper()
	return gosplan(t, t.TempDir(), nil, append([]string{"verify-attestation", "--envelope", env, "--pubkey", pub}, extra...)...)
}

// TestAttestEnvelopeShape: keygen writes PKCS#8 + PKIX PEM; attest writes a
// DSSE envelope with an in-toto v1 VSA statement about the tree and commit.
// Clause: orchestrator#DY-KEYGEN
// Clause: orchestrator#DY-ATTEST-DSSE
func TestAttestEnvelopeShape(t *testing.T) {
	k := keygen(t)
	if !strings.Contains(readFile(t, k), "BEGIN PRIVATE KEY") || !strings.Contains(readFile(t, k+".pub"), "BEGIN PUBLIC KEY") {
		t.Fatalf("keygen: want PKCS#8 PRIVATE KEY and PKIX PUBLIC KEY PEM files")
	}
	env, v := signedGolden(t, "honest-pass", k)
	e, s := decodeEnvelope(t, env)
	if e.PayloadType != "application/vnd.in-toto+json" || len(e.Signatures) == 0 {
		t.Fatalf("envelope: payloadType %q, %d signatures", e.PayloadType, len(e.Signatures))
	}
	if s.Type != "https://in-toto.io/Statement/v1" || s.PredicateType != "https://slsa.dev/verification_summary/v1" {
		t.Fatalf("statement: _type %q predicateType %q", s.Type, s.PredicateType)
	}
	var tree, commit bool
	for _, sub := range s.Subject {
		tree = tree || sub.Digest["gitTree"] == v.TreeSHA
		commit = commit || sub.Digest["gitCommit"] == v.HeadSHA
	}
	if !tree || !commit {
		t.Fatalf("subjects must carry gitTree %s and gitCommit %s: %+v", v.TreeSHA, v.HeadSHA, s.Subject)
	}
	p := s.Predicate
	if p.VerificationResult != "PASSED" || p.Verifier.ID != "https://gosplan.local/gosplan-verify" {
		t.Fatalf("predicate: result %q verifier %q", p.VerificationResult, p.Verifier.ID)
	}
	has := false
	for _, l := range p.VerifiedLevels {
		if strings.HasPrefix(l, "SLSA_") {
			t.Fatalf("custom level %q must not start with SLSA_", l)
		}
		has = has || l == "GOSPLAN_GATE_cmd_test"
	}
	if !has {
		t.Fatalf("verifiedLevels lacks GOSPLAN_GATE_cmd_test: %v", p.VerifiedLevels)
	}
}

// TestAttestFailVerdictLevels: a fail verdict signs as FAILED and the failed
// gate is not a verified level.
// Clause: orchestrator#DY-ATTEST-DSSE
func TestAttestFailVerdictLevels(t *testing.T) {
	k := keygen(t)
	env, _ := signedGolden(t, "out-of-scope", k)
	_, s := decodeEnvelope(t, env)
	if s.Predicate.VerificationResult != "FAILED" {
		t.Fatalf("fail verdict: want FAILED, got %q", s.Predicate.VerificationResult)
	}
	for _, l := range s.Predicate.VerifiedLevels {
		if l == "GOSPLAN_GATE_scope" {
			t.Fatalf("failed gate scope listed as a verified level")
		}
	}
}

// TestAttestVerifyAcceptsValid: a fresh envelope verifies (exit 0).
// Clause: orchestrator#DY-ATTEST-VERIFY
func TestAttestVerifyAcceptsValid(t *testing.T) {
	k := keygen(t)
	env, v := signedGolden(t, "honest-pass", k)
	res := verifyAttestation(t, env, k+".pub", "--tree", v.TreeSHA)
	if res.code != 0 || !strings.Contains(res.stdout, "PASSED") || !strings.Contains(res.stdout, v.TreeSHA) {
		t.Fatalf("verify-attestation of a valid envelope: want exit 0 printing PASSED and the tree, got %s", res)
	}
}

// TestAttestMeaningLine: success output states the honest limit of a signature.
// Clause: orchestrator#DY-ATTEST-MEANING
func TestAttestMeaningLine(t *testing.T) {
	k := keygen(t)
	env, _ := signedGolden(t, "honest-pass", k)
	res := verifyAttestation(t, env, k+".pub")
	if res.code != 0 {
		t.Fatalf("verify-attestation: want exit 0, got %s", res)
	}
	if !strings.Contains(res.stdout+res.stderr, "does not show that the tests ran, or ran honestly") {
		t.Fatalf("verify-attestation must print the meaning line, got %s", res)
	}
}

// TestAttestTamperedPayloadRejected: flipping the result in the payload breaks the signature.
// Clause: orchestrator#DY-ATTEST-VERIFY
func TestAttestTamperedPayloadRejected(t *testing.T) {
	k := keygen(t)
	env, _ := signedGolden(t, "out-of-scope", k)
	var e map[string]any
	if err := json.Unmarshal([]byte(readFile(t, env)), &e); err != nil {
		t.Fatal(err)
	}
	ps, _ := e["payload"].(string)
	p, err := base64.StdEncoding.DecodeString(ps)
	if err != nil || !strings.Contains(string(p), "FAILED") {
		t.Fatalf("payload does not decode to a FAILED statement")
	}
	e["payload"] = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(p), "FAILED", "PASSED", 1)))
	b, _ := json.Marshal(e)
	if err := os.WriteFile(env, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := verifyAttestation(t, env, k+".pub"); res.code != 1 {
		t.Fatalf("tampered payload: want exit 1, got %s", res)
	}
}

// TestAttestWrongKeyRejected: another key's public half does not verify.
// Clause: orchestrator#DY-ATTEST-VERIFY
func TestAttestWrongKeyRejected(t *testing.T) {
	k := keygen(t)
	other := keygen(t)
	env, _ := signedGolden(t, "honest-pass", k)
	if res := verifyAttestation(t, env, other+".pub"); res.code != 1 {
		t.Fatalf("wrong key: want exit 1, got %s", res)
	}
}

// TestAttestTreeMismatchRejected: --tree must match the gitTree subject.
// Clause: orchestrator#DY-ATTEST-VERIFY
func TestAttestTreeMismatchRejected(t *testing.T) {
	k := keygen(t)
	env, _ := signedGolden(t, "honest-pass", k)
	if res := verifyAttestation(t, env, k+".pub", "--tree", strings.Repeat("0", 40)); res.code != 1 {
		t.Fatalf("tree mismatch: want exit 1, got %s", res)
	}
}

// TestKeygenRefusesGitWorkTree: a key path inside a git work tree is refused.
// Clause: orchestrator#DY-KEYGEN
func TestKeygenRefusesGitWorkTree(t *testing.T) {
	_ = keygen(t) // control: keygen works outside a repo
	r := newRepo(t, featureToml, nil)
	k := filepath.Join(r.dir, "secrets", "k")
	if res := gosplan(t, r.dir, nil, "keygen", "--out", k); res.code != 2 {
		t.Fatalf("keygen inside a git work tree: want exit 2, got %s", res)
	}
	if _, err := os.Stat(k); err == nil {
		t.Fatalf("keygen wrote %s inside the work tree", k)
	}
}

// TestRunSignsVerdicts: `gosplan run --attest-key` signs every verdict and each
// attestation in the report verifies against the tree.
// Clause: orchestrator#DY-ATTEST-RUN
func TestRunSignsVerdicts(t *testing.T) {
	k := keygen(t)
	f := newRunFix(t, "1.a -> 2.a", nil)
	mustExit(t, "gosplan run --attest-key", f.runNR("--attest-key", k), 0)
	r, _ := f.status()
	n := 0
	for _, x := range r.Tasks {
		for _, a := range x.Attempts {
			if a.Verdict == nil {
				continue
			}
			n++
			if a.AttestationPath == "" {
				t.Fatalf("task %s attempt %d: verdict not signed", x.ID, a.N)
			}
			if res := verifyAttestation(t, a.AttestationPath, k+".pub", "--tree", a.Verdict.TreeSHA); res.code != 0 {
				t.Fatalf("task %s attempt %d attestation does not verify: %s", x.ID, a.N, res)
			}
		}
	}
	if n != 2 {
		t.Fatalf("want 2 verdicts in the report, got %d", n)
	}
}
