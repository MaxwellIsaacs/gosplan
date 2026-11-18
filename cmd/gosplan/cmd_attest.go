package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MaxwellIsaacs/gosplan/internal/attest"
)

// cmdKeygen implements `gosplan keygen`.
func cmdKeygen(args []string) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "private key path (default $XDG_CONFIG_HOME/gosplan/attest/ed25519.key)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	path := *out
	if path == "" {
		p, err := attest.DefaultKeyPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, "gosplan keygen:", err)
			return exitError
		}
		path = p
	}
	if _, err := os.Lstat(path); err == nil {
		fmt.Fprintf(os.Stderr, "gosplan keygen: %s already exists; refusing to overwrite\n", path)
		return exitError
	}
	inside, err := attest.InsideGitWorkTree(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan keygen:", err)
		return exitError
	}
	if inside {
		fmt.Fprintf(os.Stderr, "gosplan keygen: %s is inside a git work tree; keep the private key outside every repo and worktree\n", path)
		return exitError
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "gosplan keygen:", err)
		return exitError
	}
	if err := attest.GenerateKey(path); err != nil {
		fmt.Fprintln(os.Stderr, "gosplan keygen:", err)
		return exitError
	}
	pub, err := attest.LoadPublicKey(path + ".pub")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan keygen:", err)
		return exitError
	}
	fmt.Printf("private key: %s\npublic key:  %s.pub\nkey id:      %s\n", path, path, attest.KeyID(pub))
	fmt.Println("note: without a sandbox (D7), a worker running as the same OS user can read the private key file.")
	return exitPass
}

// cmdAttest implements `gosplan attest`.
func cmdAttest(args []string) int {
	fs := flag.NewFlagSet("attest", flag.ContinueOnError)
	verdictPath := fs.String("verdict", "", "verdict file (output of `gosplan verify --json`)")
	keyPath := fs.String("key", "", "private key file")
	out := fs.String("out", "", "envelope file to write")
	run := fs.String("run", "", "run id recorded in the statement")
	attempt := fs.Int("attempt", 0, "attempt number recorded in the statement")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *verdictPath == "" || *keyPath == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "gosplan attest: --verdict, --key and --out are required")
		return exitError
	}
	data, err := os.ReadFile(*verdictPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan attest:", err)
		return exitError
	}
	key, err := attest.LoadPrivateKey(*keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan attest:", err)
		return exitError
	}
	in := attest.Input{Run: *run, Attempt: *attempt, GosplanVersion: version, Time: time.Now()}
	if err := attest.SignVerdictJSON(context.Background(), key, data, in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "gosplan attest: %s: %v\n", *verdictPath, err)
		return exitError
	}
	fmt.Printf("wrote %s\n", *out)
	return exitPass
}

// cmdVerifyAttestation implements `gosplan verify-attestation`. Offline only.
func cmdVerifyAttestation(args []string) int {
	fs := flag.NewFlagSet("verify-attestation", flag.ContinueOnError)
	envPath := fs.String("envelope", "", "envelope file")
	pubPath := fs.String("pubkey", "", "public key file")
	tree := fs.String("tree", "", "require the statement to be about this git tree SHA")
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *envPath == "" || *pubPath == "" {
		fmt.Fprintln(os.Stderr, "gosplan verify-attestation: --envelope and --pubkey are required")
		return exitError
	}
	pub, err := attest.LoadPublicKey(*pubPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan verify-attestation:", err)
		return exitError
	}
	env, err := attest.ReadEnvelope(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gosplan verify-attestation:", err)
		return exitError
	}
	sum, err := attest.Verify(context.Background(), pub, env, attest.Expect{TreeSHA: *tree})
	if err != nil {
		fmt.Printf("INVALID: %v\n", err)
		return exitFail
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"valid": true, "result": sum.Result, "tree_sha": sum.TreeSHA, "commit_sha": sum.CommitSHA,
			"verifier": sum.VerifierID, "time_verified": sum.TimeVerified.Format(time.RFC3339),
			"levels": sum.Levels, "extension": sum.Extension, "key_id": sum.KeyID,
			"meaning": attest.Meaning,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "gosplan verify-attestation: encode:", err)
			return exitError
		}
		return exitPass
	}
	fmt.Printf("result:   %s\ntree:     %s\ncommit:   %s\nverifier: %s\ntime:     %s\nkey id:   %s\nlevels:\n",
		sum.Result, sum.TreeSHA, sum.CommitSHA, sum.VerifierID, sum.TimeVerified.Format(time.RFC3339), sum.KeyID)
	for _, l := range sum.Levels {
		fmt.Printf("  %s\n", l)
	}
	fmt.Println(attest.Meaning)
	return exitPass
}
