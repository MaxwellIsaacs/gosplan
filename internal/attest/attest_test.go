package attest

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ita "github.com/in-toto/attestation/go/v1"
	"github.com/secure-systems-lab/go-securesystemslib/dsse"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/MaxwellIsaacs/gosplan/internal/events"
	"github.com/MaxwellIsaacs/gosplan/internal/verify"
)

const (
	attestTree = "cccccccccccccccccccccccccccccccccccccccc"
	attestHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	attestBase = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func attestInput(result string) Input {
	v := verify.Verdict{
		Task: "1.a", BaseSHA: attestBase, HeadSHA: attestHead, TreeSHA: attestTree,
		Gates: []events.GateData{
			{Name: "committed_work", Status: events.GatePass},
			{Name: "cmd:test", Status: events.GatePass},
		},
		Result: result, FailedGates: []string{},
	}
	if result == "fail" {
		v.Gates[1].Status = events.GateFail
		v.FailedGates = []string{"cmd:test"}
	}
	return Input{Run: "r1", Attempt: 2, Verdict: v, GosplanVersion: "test", Time: time.Unix(1700000000, 0)}
}

func attestKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "k")
	if err := GenerateKey(p); err != nil {
		t.Fatal(err)
	}
	priv, err := LoadPrivateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := LoadPublicKey(p + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func attestSign(t *testing.T, priv ed25519.PrivateKey, in Input) *dsse.Envelope {
	t.Helper()
	env, err := Sign(context.Background(), priv, in)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestAttestRoundTrip(t *testing.T) {
	priv, pub := attestKey(t)
	env := attestSign(t, priv, attestInput("pass"))
	s, err := Verify(context.Background(), pub, env, Expect{TreeSHA: attestTree})
	if err != nil {
		t.Fatal(err)
	}
	if s.Result != "PASSED" || s.TreeSHA != attestTree || s.CommitSHA != attestHead || s.VerifierID != VerifierID {
		t.Fatalf("summary: %+v", s)
	}
	found := false
	for _, l := range s.Levels {
		found = found || l == "GOSPLAN_GATE_cmd_test"
	}
	if !found {
		t.Fatalf("levels %v", s.Levels)
	}
	if !s.TimeVerified.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("time %v", s.TimeVerified)
	}
	if s.Extension["run"] != "r1" || s.Extension["attempt"] != float64(2) {
		t.Fatalf("extension %v", s.Extension)
	}
}

func TestAttestFailVerdict(t *testing.T) {
	priv, pub := attestKey(t)
	env := attestSign(t, priv, attestInput("fail"))
	s, err := Verify(context.Background(), pub, env, Expect{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Result != "FAILED" {
		t.Fatalf("result %s", s.Result)
	}
	for _, l := range s.Levels {
		if l == "GOSPLAN_GATE_cmd_test" {
			t.Fatalf("failed gate in levels %v", s.Levels)
		}
	}
	fg, _ := s.Extension["failed_gates"].([]any)
	if len(fg) != 1 || fg[0] != "cmd:test" {
		t.Fatalf("failed_gates %v", s.Extension["failed_gates"])
	}
}

func TestAttestTamperedPayloadRejected(t *testing.T) {
	priv, pub := attestKey(t)
	env := attestSign(t, priv, attestInput("fail"))
	body, err := env.DecodeB64Payload()
	if err != nil {
		t.Fatal(err)
	}
	// Flip FAILED -> PASSED's first letter: any single byte change must break it.
	i := strings.Index(string(body), "FAILED")
	body[i] = 'f'
	env.Payload = base64.StdEncoding.EncodeToString(body)
	if _, err := Verify(context.Background(), pub, env, Expect{}); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

func TestAttestWrongKeyRejected(t *testing.T) {
	priv, _ := attestKey(t)
	_, other := attestKey(t)
	env := attestSign(t, priv, attestInput("pass"))
	if _, err := Verify(context.Background(), other, env, Expect{}); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestAttestWrongPayloadTypeRejected(t *testing.T) {
	priv, pub := attestKey(t)
	st, err := Statement(attestInput("pass"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := protojson.Marshal(st)
	env, err := signBody(context.Background(), priv, "text/plain", body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), pub, env, Expect{}); err == nil || !strings.Contains(err.Error(), "payloadType") {
		t.Fatalf("want payloadType error, got %v", err)
	}
}

func TestAttestWrongPredicateTypeRejected(t *testing.T) {
	priv, pub := attestKey(t)
	st, err := Statement(attestInput("pass"))
	if err != nil {
		t.Fatal(err)
	}
	st.PredicateType = "https://example.com/other/v1"
	body, _ := protojson.Marshal(st)
	env, err := signBody(context.Background(), priv, PayloadType, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), pub, env, Expect{}); err == nil || !strings.Contains(err.Error(), "predicateType") {
		t.Fatalf("want predicateType error, got %v", err)
	}
}

func TestAttestTreeMismatch(t *testing.T) {
	priv, pub := attestKey(t)
	env := attestSign(t, priv, attestInput("pass"))
	_, err := Verify(context.Background(), pub, env, Expect{TreeSHA: strings.Repeat("d", 40)})
	if err == nil || !strings.Contains(err.Error(), "tree mismatch") {
		t.Fatalf("want tree mismatch, got %v", err)
	}
}

func TestAttestNoSLSAPrefixLevels(t *testing.T) {
	in := attestInput("pass")
	in.Verdict.Gates = append(in.Verdict.Gates, events.GateData{Name: "SLSA_BUILD_LEVEL_3", Status: events.GatePass})
	priv, pub := attestKey(t)
	s, err := Verify(context.Background(), pub, attestSign(t, priv, in), Expect{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Levels) != 3 {
		t.Fatalf("levels %v", s.Levels)
	}
	for _, l := range s.Levels {
		if strings.HasPrefix(l, "SLSA_") || !strings.HasPrefix(l, "GOSPLAN_GATE_") {
			t.Fatalf("bad level %q", l)
		}
	}
}

func TestAttestStatementValidates(t *testing.T) {
	st, err := Statement(attestInput("pass"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range st.Subject {
		for k, v := range s.Digest {
			got[k] = v
		}
	}
	if got["gitTree"] != attestTree || got["gitCommit"] != attestHead {
		t.Fatalf("subjects %v", got)
	}
	if st.Type != ita.StatementTypeUri || st.PredicateType != PredicateType {
		t.Fatalf("types %s %s", st.Type, st.PredicateType)
	}
	p := st.Predicate.AsMap()
	if p["resourceUri"] != "gosplan:task/r1/1.a@"+attestHead {
		t.Fatalf("resourceUri %v", p["resourceUri"])
	}
	if v, _ := p["verifier"].(map[string]any); v["id"] != VerifierID || v["version"].(map[string]any)["gosplan"] != "test" {
		t.Fatalf("verifier %v", p["verifier"])
	}
	if _, err := Statement(Input{Verdict: verify.Verdict{Result: "pass"}}); err == nil {
		t.Fatal("statement without a tree validated")
	}
}

func TestAttestKeyFilePerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	if err := GenerateKey(p); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v", st.Mode().Perm())
	}
	if ps, _ := os.Stat(p + ".pub"); ps.Mode().Perm() != 0o644 {
		t.Fatalf("public key mode %v", ps.Mode().Perm())
	}
	if err := GenerateKey(p); err == nil {
		t.Fatal("GenerateKey overwrote an existing key")
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(p); err == nil {
		t.Fatal("0644 private key accepted")
	}
}

func TestAttestInsideGitWorkTree(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	in, err := InsideGitWorkTree(filepath.Join(repo, "not", "yet", "key"))
	if err != nil || !in {
		t.Fatalf("inside repo: %v %v", in, err)
	}
	out, err := InsideGitWorkTree(filepath.Join(t.TempDir(), "key"))
	if err != nil || out {
		t.Fatalf("outside repo: %v %v", out, err)
	}
}
