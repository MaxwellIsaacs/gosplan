# Conformance suite

PROTECTED. Everything under `conformance/` and `testdata/` is protected. Agent
tasks must not edit, skip, delete or loosen any of it to make a test pass:
**the red tests are the target, and no test may be weakened to pass.** A human
reviews every change to these files by hand.

The suite is the definition of the Go rewrite. It is black box: it drives the
built `gosplan` binary, git, the `sqlite3` CLI on `gosplan.db`, and its own fakes
(`fakeclaude/`, a gremlins shell script). The only Go API it touches is
`internal/core`, through `coreprobe/` behind the
build tag `nrcore`. No test spends plan usage or touches the network.

Clause index: [`clauses.md`](clauses.md). Expected failures: [`RED.md`](RED.md).

## Run

```
conformance/run.sh            # vet, all tests once, RED.md check, green tests 2 more times
conformance/run.sh --repeat 1 # one pass only
```

Needs `go`, `git` and a `sqlite3` CLI (it finds one in `/nix/store`, or re-execs
under `nix shell nixpkgs#go nixpkgs#git nixpkgs#sqlite`). Exit 0: every green
test passed every time and every red test failed with its `RED.md` text.
Exit 3: the same, except some red tests now pass ("NEWLY GREEN"). Exit 1:
anything else. The last line prints the counts.

## Two kinds of test

- **Green now** (`events_test.go`, `verify_test.go`, `verify_exit_cache_test.go`,
  `meta_test.go`): the v1 event log, verifier gates (golden fixtures), exit codes
  and cancellation, cache rules, locked/protected/config defaults, and the
  suite's own rules. They must stay green forever.
- **Red by design** (`run_test.go`, `run_crash_test.go`, `store_test.go`,
  `attest_test.go`, `mutation_test.go`, `windows_test.go`, `status_test.go`,
  `core_test.go`): the orchestrator, written before it existed. Each has a row in `RED.md` with
  the failure text expected today.

## When a red test turns green

`run.sh` exits 3 and prints `NEWLY GREEN <test>`. A human reviews the work and moves
that row out of `RED.md` in a hand-reviewed commit. An operation may not do it
itself, and may not change a test's assertions.

## Every test names its clause

Each `Test*` has `// Clause: <file>#<id>` lines in its doc comment;
`TestMetaClausesAndRedTable` fails on a missing or unknown clause, a red test
without a `RED.md` row, a clause without a test, a missing protected header, or
any `t.Skip`.
