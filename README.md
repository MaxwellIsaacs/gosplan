<p align="center">
  <img src="assets/gosplan-building.jpg" alt="The Gosplan building on Okhotny Ryad, Moscow, 1930s postcard" width="600">
</p>

<h1><img src="assets/logo.svg" alt="" width="40" align="top"> Gosplan</h1>

Central planning for coding agents, written in Go.

Gosplan takes a spec that has been broken into tasks, runs one `claude`
worker per task, and accepts a task only when its own verifier passes it.
It schedules the work against the one resource that is actually scarce: the
plan's usage window.

The original Gosplan failed because the factories padded their figures and
nobody could check. This one checks.

> Formerly nightrunner, a bash harness that ran specs like this for about
> a year. The project config file keeps the name `nightrunner.toml`.

## What it does

- **Plans.** Reads a spec directory (`spec.md`, `prompts/`,
  `prompts/sequence.txt`) and schedules tasks by declared dependency, not
  by tier barrier.
- **Supervises.** One worker per task in its own git worktree, with a lease
  and heartbeat. State lives in SQLite plus an append-only `events.jsonl`
  that can be replayed.
- **Inspects.** Every task is verified in a clean export of its commits:
  file scope, protected and locked paths, then the project's own test, lint
  and build commands. The worker's word counts for nothing.
- **Rations.** Observes the five-hour and weekly usage windows, reserves
  headroom before launching a task, and parks the run until the window
  resets. Observe-only by default.
- **Never merges to your main branch.** Passed tasks land on an integration
  branch. A human merges.

## Status

| Part | State |
|---|---|
| Verifier (`gosplan verify`, `gosplan replay`, attestations) | On `main` |
| Orchestrator (`gosplan run`, `gosplan status`) | On `main` |
| Budget governor (`gosplan budget`, `--budget`) | On `main`; observe-only by default |
| Nix flake and module | Not started |
| Worker sandbox | Not started; workers share the orchestrator's uid, see [docs/trust-until-d7.md](docs/trust-until-d7.md) |

First large real run (2026-11-10, a 21-task web app spec): 13 tasks passed
and integrated, 12 of them on the first attempt, then one task failed three
attempts and the run halted.

## Commands

| command | what it does |
|---|---|
| `gosplan run` | preflight the environment, schedule a spec's tasks by dependency, supervise the workers, verify each task; `--continue` finishes a halted run |
| `gosplan status` | read-only view of a run: tasks, attempts, verdicts, observed windows |
| `gosplan verify` | verify a task's `base..head` in a clean export. Exit 0 pass, 1 fail, 2 could not verify |
| `gosplan replay` | fold an `events.jsonl` into task statuses; `--check` compares them with a `state.json` |
| `gosplan budget show`, `explain`, `calibrate` | inspect budget decisions and fit estimates from past runs |
| `gosplan keygen`, `gosplan attest`, `gosplan verify-attestation` | ed25519 DSSE envelopes over verdicts and their offline check |
| `gosplan version` | print the version |

Operator guides: [docs/gosplan-run.md](docs/gosplan-run.md),
[docs/gosplan-budget.md](docs/gosplan-budget.md).

## Workflow

Two Claude Code skills cover the parts the binary does not:

- [skills/gosplan-spec](skills/gosplan-spec/SKILL.md) turns a spec document
  into a spec directory: tasks, dependencies, scopes, and verify commands a
  worker can pass honestly.
- [skills/gosplan-drive](skills/gosplan-drive/SKILL.md) preflights,
  launches, watches, debugs, re-runs and ships a run.

Symlink them into `~/.claude/skills/`. Machine-specific notes (which host
runs gosplan, resource policy) go in `~/.config/gosplan/operator.md`, which
gosplan-drive reads first.

## Build and test

```
nix shell nixpkgs#go nixpkgs#gcc nixpkgs#git -c bash -c 'gofmt -l . ; go vet ./... && go test -race -count=1 ./...'
CGO_ENABLED=0 go build -o gosplan ./cmd/gosplan
```

The SQLite store uses `modernc.org/sqlite`, so the binary builds with
`CGO_ENABLED=0`.

## Trust boundary

Workers run as the same uid as `gosplan run` until the sandbox is built.
[docs/trust-until-d7.md](docs/trust-until-d7.md) lists what is defended,
what is detected and what is still open.

`internal/verify`, `internal/gitx` and `testdata/golden` change only by
human-reviewed commits.

## Roadmap

1. ~~Merge the budget governor.~~ Done.
2. Nix: one flake, one pinned `claude`, the project's dev shell as the
   worker and verifier environment.
3. Sandbox (D7): systemd scope, bwrap, credentials and keys out of the worker's
   reach.
4. Bisecting integration, fresh-context retries, a stall watchdog, an
   offline evidence bundle.
5. `gosplan verify` packaged for use on its own, outside `gosplan run`.

## Glossary

| Term | Here |
|---|---|
| Five-Year Plan | the spec |
| OTK (factory quality control) | the verifier |
| Gossnab (state supply) | the budget governor |
| Brigade | tasks that run in parallel |
| Norm | a task's token estimate |
| Pripiski (padded output figures) | what the verifier is for |
| Perestroika | restructuring a codebase to fit the plan |

## Image

The header is a 1930s postcard of the building on Okhotny Ryad in Moscow
that housed Gosplan (now the State Duma), from an open archive. The logo in
`assets/logo.svg` is drawn from it.

## License

Apache-2.0. See [LICENSE](LICENSE).
