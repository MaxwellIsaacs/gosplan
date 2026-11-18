# Golden verifier fixtures

Four small repos that pin what `gosplan verify` must say. No repo or other
binary is committed: everything here is text. At test time
`internal/verify` runs `build.sh` once into a temp dir, checks every
fixture's base and head SHA against `shas.txt` (any drift fails the tests),
then `TestGoldenFixtures` verifies each built repo against its
`expect.json`.

| file | what it is |
|---|---|
| `build.sh` | builds every fixture repo into `OUTDIR/<name>` (branch `main` = base commit, `task` = the worker's commit). Fixed dates, identity and config, so SHAs are stable. |
| `shas.txt` | expected `name base head` (full SHAs) per fixture, in the format `build.sh` prints. |

Each `<name>/` has:

| file | what it is |
|---|---|
| `expect.json` | hand-written: the `--scope` to pass (`null` = unrestricted), the expected `result`, the exact `failed_gates`, and optionally `detail_contains` (gate name → text its detail must contain). |
| `uncommitted/` | optional: files copied into the worker tree after checkout and **not** committed. |

Every base commit has the same `nightrunner.toml`:

```toml
[verify]
test = "test -f src/feature.txt"
```

## Fixtures

| name | base (`main`) | task commit | scope | expected |
|---|---|---|---|---|
| `tampered-protected-test` | `src/feature.txt`, `pkg/a_test.go` | edits `pkg/a_test.go` ("weakened to pass") | none | **fail**: `protected_paths` |
| `uncommitted-only-pass` | `README` | edits `README` only; `src/feature.txt` exists only in `uncommitted/` | none | **fail**: `cmd:test` (the clean checkout has no feature file) |
| `out-of-scope` | `README` | adds `src/feature.txt` and `docs/notes.md` | `src/**` | **fail**: `scope` (names `docs/notes.md`) |
| `honest-pass` | `README` | adds `src/feature.txt` | `src/**` | **pass** |

Commit SHAs (fixed dates and identity, so they are stable) are in
`shas.txt`.

## Changing a fixture

1. Edit `build.sh`, then run `testdata/golden/build.sh /tmp/golden` and
   review the repos it builds (`git -C /tmp/golden/<name> log --stat main..task`).
2. Copy the printed `name base head` lines into `shas.txt`, and update
   `expect.json` and this README by hand.
3. The golden tests fail if `build.sh` no longer produces the SHAs in
   `shas.txt`, if a fixture lacks a line there, or if a `*.bundle` is
   committed here.

Note: in a project verified by gosplan, `**/testdata/**` is protected by
default, so a task branch cannot edit these files.
