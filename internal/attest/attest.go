// Package attest turns an `gosplan verify` verdict into a signed statement and
// checks such statements offline.
//
// The statement is an in-toto v1 Statement whose subjects are the verified git
// tree and commit, with a SLSA Verification Summary (VSA) predicate, wrapped
// in a DSSE envelope signed with a local ed25519 key. Nothing goes to a
// transparency log and verification needs no network.
//
// What a signature means: valid signature: the holder of this key made this
// statement about this tree. It does not show that the tests ran, or ran
// honestly.
//
// Key handling: the private key is generated outside any git work tree and is
// never put in a worker's environment. Limit: without a sandbox (D7, out of
// scope), a worker running as the same OS user can read the key file.
package attest

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	vsa "github.com/in-toto/attestation/go/predicates/vsa/v1"
	ita "github.com/in-toto/attestation/go/v1"
	"github.com/secure-systems-lab/go-securesystemslib/dsse"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/MaxwellIsaacs/gosplan/internal/verify"
)

const (
	PayloadType   = "application/vnd.in-toto+json"
	PredicateType = "https://slsa.dev/verification_summary/v1"
	VerifierID    = "https://gosplan.local/gosplan-verify"
	PolicyURI     = "https://gosplan.local/policy/m1-verify-contract/rev4"
	ExtensionKey  = "https://gosplan.local/attest/verdict/v1"
)

// Meaning is the one-line statement of what a valid signature shows.
const Meaning = "valid signature: the holder of this key made this statement about this tree. It does not show that the tests ran, or ran honestly."

const levelPrefix = "GOSPLAN_GATE_"

// Input is everything a statement is built from.
type Input struct {
	Run            string
	Attempt        int
	Verdict        verify.Verdict
	GosplanVersion string
	Time           time.Time
}

// --- keys ---

// GenerateKey writes a new ed25519 key pair: path (0600, PKCS#8 PEM) and
// path+".pub" (0644, PKIX PEM). It refuses to overwrite either file.
func GenerateKey(path string) error {
	pubPath := path + ".pub"
	for _, p := range []string{path, pubPath} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("attest: %s already exists; refusing to overwrite", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	if err := writeExcl(path, 0o600, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})); err != nil {
		return err
	}
	if err := writeExcl(pubPath, 0o644, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// writeExcl creates path with exactly mode (independent of umask) and fails
// if it exists.
func writeExcl(path string, mode os.FileMode, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// LoadPrivateKey reads a PKCS#8 PEM ed25519 key. It refuses a file that is
// readable by group or others.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("attest: private key %s has mode %04o; it must not be group or world accessible (chmod 600)", path, st.Mode().Perm())
	}
	der, err := readPEM(path, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("attest: %s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("attest: %s is not an ed25519 key", path)
	}
	return priv, nil
}

// LoadPublicKey reads a PKIX PEM ed25519 public key.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	der, err := readPEM(path, "PUBLIC KEY")
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("attest: %s: %w", path, err)
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("attest: %s is not an ed25519 key", path)
	}
	return pub, nil
}

func readPEM(path, typ string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(data)
	if blk == nil || blk.Type != typ {
		return nil, fmt.Errorf("attest: %s: no %q PEM block", path, typ)
	}
	return blk.Bytes, nil
}

// DefaultKeyPath is $XDG_CONFIG_HOME/gosplan/attest/ed25519.key, else
// ~/.config/gosplan/attest/ed25519.key.
func DefaultKeyPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "gosplan", "attest", "ed25519.key"), nil
}

// InsideGitWorkTree reports whether path (or its nearest existing parent) is
// inside a git work tree, by walking up looking for a .git entry.
func InsideGitWorkTree(path string) (bool, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	for {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false, nil
		}
		p = parent
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	for {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false, nil
		}
		p = parent
	}
}

// --- signer / verifier ---

type edSV struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func (s edSV) Sign(_ context.Context, d []byte) ([]byte, error) {
	if s.priv == nil {
		return nil, errors.New("attest: no private key")
	}
	return ed25519.Sign(s.priv, d), nil
}

func (s edSV) Verify(_ context.Context, d, sig []byte) error {
	if !ed25519.Verify(s.pub, d, sig) {
		return errors.New("bad signature")
	}
	return nil
}

func (s edSV) KeyID() (string, error)   { return keyID(s.pub), nil }
func (s edSV) Public() crypto.PublicKey { return s.pub }

func keyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// KeyID is the short identifier recorded in the envelope for pub.
func KeyID(pub ed25519.PublicKey) string { return keyID(pub) }

// --- statement ---

var nonLevelChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

// Level is the verifiedLevels entry for a passing gate.
func Level(gate string) string { return levelPrefix + nonLevelChar.ReplaceAllString(gate, "_") }

// Statement builds the in-toto statement for in and validates it.
func Statement(in Input) (*ita.Statement, error) {
	v := in.Verdict
	result := "FAILED"
	if v.Result == "pass" {
		result = "PASSED"
	}
	levels := []string{}
	gates := []any{}
	for _, g := range v.Gates {
		if g.Status == "pass" {
			levels = append(levels, Level(g.Name))
		}
		gates = append(gates, map[string]any{"name": g.Name, "status": string(g.Status)})
	}
	t := in.Time
	if t.IsZero() {
		t = time.Now()
	}
	pred := &vsa.VerificationSummary{
		Verifier: &vsa.VerificationSummary_Verifier{
			Id: VerifierID,
		},
		TimeVerified:       timestamppb.New(t.UTC()),
		ResourceUri:        fmt.Sprintf("gosplan:task/%s/%s@%s", in.Run, v.Task, v.HeadSHA),
		Policy:             &vsa.VerificationSummary_Policy{Uri: PolicyURI},
		VerificationResult: result,
		VerifiedLevels:     levels,
	}
	pj, err := protojson.Marshal(pred)
	if err != nil {
		return nil, err
	}
	ps := &structpb.Struct{}
	if err := protojson.Unmarshal(pj, ps); err != nil {
		return nil, err
	}
	// The generated VSA type has no verifier.version field; add it to the
	// struct form so the predicate still carries it.
	if vf := ps.Fields["verifier"].GetStructValue(); vf != nil {
		vf.Fields["version"] = structpb.NewStructValue(&structpb.Struct{
			Fields: map[string]*structpb.Value{"gosplan": structpb.NewStringValue(in.GosplanVersion)},
		})
	}
	failed := []any{}
	for _, f := range v.FailedGates {
		failed = append(failed, f)
	}
	ext, err := structpb.NewValue(map[string]any{
		"run":                in.Run,
		"task":               v.Task,
		"attempt":            float64(in.Attempt),
		"base_sha":           v.BaseSHA,
		"head_sha":           v.HeadSHA,
		"cache_hit":          v.CacheHit,
		"scope_unrestricted": v.Unrestricted,
		"failed_gates":       failed,
		"gates":              gates,
	})
	if err != nil {
		return nil, err
	}
	ps.Fields[ExtensionKey] = ext

	st := &ita.Statement{
		Type: ita.StatementTypeUri,
		Subject: []*ita.ResourceDescriptor{
			{Name: "tree", Digest: map[string]string{"gitTree": v.TreeSHA}},
			{Name: "commit", Digest: map[string]string{"gitCommit": v.HeadSHA}},
		},
		PredicateType: PredicateType,
		Predicate:     ps,
	}
	if err := st.Validate(); err != nil {
		return nil, fmt.Errorf("attest: statement: %w", err)
	}
	return st, nil
}

// Sign builds the statement for in and signs it into a DSSE envelope.
func Sign(ctx context.Context, key ed25519.PrivateKey, in Input) (*dsse.Envelope, error) {
	st, err := Statement(in)
	if err != nil {
		return nil, err
	}
	body, err := protojson.Marshal(st)
	if err != nil {
		return nil, err
	}
	return signBody(ctx, key, PayloadType, body)
}

func signBody(ctx context.Context, key ed25519.PrivateKey, payloadType string, body []byte) (*dsse.Envelope, error) {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("attest: bad private key")
	}
	signer, err := dsse.NewEnvelopeSigner(edSV{priv: key, pub: pub})
	if err != nil {
		return nil, err
	}
	return signer.SignPayload(ctx, payloadType, body)
}

// WriteEnvelope writes env as JSON (0644) via a temp file and rename.
func WriteEnvelope(path string, env *dsse.Envelope) error {
	return writeEnvelope(path, env)
}

// SignVerdictJSON parses verdictJSON (`gosplan verify --json` output), signs it
// as in.Verdict and writes the envelope to path. in.Verdict is ignored.
func SignVerdictJSON(ctx context.Context, key ed25519.PrivateKey, verdictJSON []byte, in Input, path string) error {
	if err := json.Unmarshal(verdictJSON, &in.Verdict); err != nil {
		return fmt.Errorf("parse verdict: %w", err)
	}
	env, err := Sign(ctx, key, in)
	if err != nil {
		return err
	}
	return writeEnvelope(path, env)
}

func writeEnvelope(path string, env *dsse.Envelope) error {
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".envelope-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// ReadEnvelope reads a JSON DSSE envelope.
func ReadEnvelope(path string) (*dsse.Envelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env dsse.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("attest: %s: %w", path, err)
	}
	return &env, nil
}

// --- verification ---

// Expect holds optional checks.
type Expect struct{ TreeSHA string }

// Summary is what a valid envelope says. See Meaning for what that is worth.
type Summary struct {
	Result       string // PASSED | FAILED
	TreeSHA      string
	CommitSHA    string
	VerifierID   string
	TimeVerified time.Time
	Levels       []string
	Extension    map[string]any
	KeyID        string
}

// Verify checks the signature with pub, then payloadType, statement type,
// predicateType, the gitTree subject, and want. Any failure is an error.
func Verify(ctx context.Context, pub ed25519.PublicKey, env *dsse.Envelope, want Expect) (Summary, error) {
	var sum Summary
	if env == nil {
		return sum, errors.New("attest: nil envelope")
	}
	if len(pub) != ed25519.PublicKeySize {
		return sum, errors.New("attest: bad public key")
	}
	ev, err := dsse.NewEnvelopeVerifier(edSV{pub: pub})
	if err != nil {
		return sum, err
	}
	accepted, body, err := ev.VerifyAndDecode(ctx, env)
	if err != nil {
		return sum, fmt.Errorf("signature check failed: %w", err)
	}
	if len(accepted) == 0 {
		return sum, errors.New("signature check failed: no accepted signature")
	}
	sum.KeyID = accepted[0].KeyID
	// payloadType is covered by the signature (it is in the PAE), but a valid
	// signature over some other payload type is still not one of ours.
	if env.PayloadType != PayloadType {
		return sum, fmt.Errorf("wrong payloadType %q (want %q)", env.PayloadType, PayloadType)
	}
	var st ita.Statement
	if err := protojson.Unmarshal(body, &st); err != nil {
		return sum, fmt.Errorf("statement does not parse: %w", err)
	}
	if st.Type != ita.StatementTypeUri {
		return sum, fmt.Errorf("wrong statement type %q", st.Type)
	}
	if st.PredicateType != PredicateType {
		return sum, fmt.Errorf("wrong predicateType %q (want %q)", st.PredicateType, PredicateType)
	}
	if err := st.Validate(); err != nil {
		return sum, fmt.Errorf("statement invalid: %w", err)
	}
	for _, s := range st.Subject {
		switch {
		case s.Name == "tree" && s.Digest["gitTree"] != "":
			if sum.TreeSHA != "" {
				return sum, errors.New("more than one gitTree subject")
			}
			sum.TreeSHA = s.Digest["gitTree"]
		case s.Name == "commit" && s.Digest["gitCommit"] != "":
			sum.CommitSHA = s.Digest["gitCommit"]
		}
	}
	if sum.TreeSHA == "" {
		return sum, errors.New("no gitTree subject")
	}
	if want.TreeSHA != "" && !strings.EqualFold(want.TreeSHA, sum.TreeSHA) {
		return sum, fmt.Errorf("tree mismatch: statement is about %s, expected %s", sum.TreeSHA, want.TreeSHA)
	}
	if st.Predicate == nil {
		return sum, errors.New("no predicate")
	}
	pj, err := protojson.Marshal(st.Predicate)
	if err != nil {
		return sum, err
	}
	var p vsa.VerificationSummary
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(pj, &p); err != nil {
		return sum, fmt.Errorf("predicate does not parse: %w", err)
	}
	if p.VerificationResult != "PASSED" && p.VerificationResult != "FAILED" {
		return sum, fmt.Errorf("unexpected verificationResult %q", p.VerificationResult)
	}
	sum.Result = p.VerificationResult
	sum.VerifierID = p.GetVerifier().GetId()
	if p.TimeVerified != nil {
		sum.TimeVerified = p.TimeVerified.AsTime()
	}
	sum.Levels = p.VerifiedLevels
	if m, ok := st.Predicate.AsMap()[ExtensionKey].(map[string]any); ok {
		sum.Extension = m
	}
	return sum, nil
}
