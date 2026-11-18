# `gosplan run`: operator guide

`gosplan run` is the Go orchestrator. It imports a gosplan spec directory,
schedules its tasks by dependency, runs one `claude` worker per task as a
supervised subprocess, verifies each finished task with `gosplan verify` (the
verifier), optionally signs each verdict, records an advisory mutation result
and the rate-limit windows it observed, merges the passed work into an
integration branch and writes a run report.

**Status: the launcher for real runs.** `gosplan run` replaced the bash
orchestrator. It is tested end to end with fake `claude` workers (the
conformance suite's `conformance/fakeclaude`, also used by
`cmd/gosplan/run_test.go`, and the adversarial shell fakes in
`cmd/gosplan/tamper_test.go`) and has run real specs since 2026-11-10. Until
D7, workers share this user's uid: read
[trust-until-d7.md](trust-until-d7.md) for what that leaves open.

## What it does

1. Resolves `--base` (default `HEAD`) in the project to a full SHA, once.
2. Loads and validates the spec (`prompts/sequence.txt`, `prompts/<id>.md`,
   optional `prompts/<id>.task.toml`). An invalid spec is refused before
   anything is written.
3. Opens `<state>/gosplan.db` (SQLite, WAL) and imports the run, or resumes
   `--run ID` after checking that the spec content hash is unchanged
   ("spec changed since import" otherwise, exit 2).
4. If `--usage-probe` is on and the newest window observation is stale, runs
   the $0 `claude -p /usage` probe (failures are warnings).
5. Runs the reconciler: dispatches ready tasks up to `--max-parallel`,
   launches workers, kills stalled ones, verifies finished ones, retries with
   backoff, abandons descendants of failed/blocked tasks. Decisions read the
   orchestrator's in-memory copy of the run; every DB transaction first
   checks that `gosplan.db` still says what this process wrote
   ([trust-until-d7.md](trust-until-d7.md)).
6. When every task is terminal, merges the passed tasks' verified head SHAs
   in (tier, id) order into `gosplan/<run>/integration` from the run base, then
   finishes the run. A conflict blocks the task and halts the run.
7. Runs the `gosplan replay --check` logic in-process (DB, log and integration
   tip must agree with memory), waits for in-flight mutation runs, flushes
   `events.jsonl`, writes `state.json` and `report.json`, prints the report
   text.

Exit codes: **0** run `complete`: every task passed **and** was merged into
the integration branch. **1** run `halted`: some task failed, blocked or was
abandoned, a passed task could not be integrated, or the orchestrator halted
the run for one of the reasons below. **2** usage/setup error or run
`aborted` (SIGINT/SIGTERM).

### Halt reasons

When the orchestrator itself stops a run, the run ends `halted` (exit 1)
and `gosplan run` prints `halted (<reason>: <detail>)`; the same text is the
`run_finished` event's reason.

| reason | meaning |
|---|---|
| `state_tampered` | `gosplan.db` (a `tasks` or `runs` row) differs from what this process wrote, or on resume the DB disagrees with the spec (task set, static task columns, spec hash), `events.jsonl` (run base, spec hash, statuses, integration tip), the integration branch, or a re-verify. The detail names table, row and fields. |
| `state_inconsistent` | the end-of-run self-check failed (log replay, integration tip), a recomputed base differs, or a recorded pass could not be re-verified on resume. |
| `state_locked` | `gosplan.db` stayed locked by another process (`SQLITE_BUSY`) for over a minute. The clock starts at the first busy result; every 2s poll then probes the write lock (an empty `BEGIN IMMEDIATE`), and a free lock clears it, so only a lock that stays held halts. The run row may still say `running`. |
| `state_write_failed` | one step's write kept failing for a reason other than a busy lock: 10 failures in a row, or 60s since the first (`reconcile.Config.MaxWriteFailures`, `WriteFailTimeout`; no flag). The detail is `<step>: <error>`, where step is the work-queue key (`task/<id>`, `run`) or `journal` (writing `events.jsonl` / `state.json`). Examples: the attempt row the exit must close was edited, or the run dir turned read-only. |
| `integration_conflict` | a passed task's head does not merge into the integration branch. That task becomes `blocked` with the conflicting paths (and a `blocker` event); the branch keeps the last good merge; the detail lists the tasks not integrated. |
| `integration_failed` | git failed while writing the integration branch. |
| `infrastructure` | 10 infrastructure failures in a row (worker never started or lost) with no launch in between. No task is failed for them. |
| `verifier_error` | `--max-verify-errors` (3) consecutive could-not-verify results across the run. On in every budget mode, with or without a gate (see Budget). |
| `deadline` | the budget gate's stop halt at `--deadline`: live workers are killed, their attempts refunded, and the pending tasks stay `pending` (parked). The detail is `parked <ids>`. |
| `cap:usd_estimate`, `cap:window:<pool>` | the budget gate's drain halt at a per-run cap: no new attempt starts, running ones finish and are verified, then the run ends with `parked <ids>`. |
| `no_progress` | a task was ready but never dispatched, running with no worker (or with an exit that could not be recorded), or verifying with no verify (or with a verdict that could not be recorded), for two minutes. The detail names the tasks. |

`gosplan run` never writes `main`, `master` or the project's checked-out branch.
Task branches are `gosplan/<run>/<task>`; the integration branch is
`gosplan/<run>/integration`.

A run's `run_finished` reason ends with `integration_sha=<tip>` when the
integration branch was written; resume cross-checks it against `gosplan.db`.

## Flags

```
gosplan run --spec DIR --project DIR [flags]
  --state-dir DIR        default $XDG_STATE_HOME/gosplan/<project>-<sha256(path)[:12]>
  --base REF             base of a new run (default HEAD), resolved once
  --run ID               resume this run
  --max-parallel N       tasks running or verifying at once (3)
  --max-attempts N       attempts per task (3)
  --stall-timeout D      kill a worker silent on stdout this long (5m)
  --retry-base D         backoff base: delay = min(base*2^(n-1), 5m) (10s)
  --claude BIN           worker binary (claude)
  --model M              model for attempt 1 (sonnet)
  --retry-model M        model for later attempts (opus)
  --max-turns N          claude --max-turns (200)
  --gosplan-bin PATH          gosplan binary for `gosplan verify` (default: this binary)
  --verify-timeout D     stop one `gosplan verify` after this long (2h)
  --attest-key PATH      sign verdicts with this ed25519 key (default: unsigned)
  --require-attest       fail before any task runs if no key is usable
  --mutation advisory|off  run gremlins after each pass (advisory)
  --gremlins BIN         gremlins binary (gremlins)
  --usage-probe=true|false  allow the /usage probe (true; on/off accepted)
  --budget off|observe|enforce  budget gate mode (default: budget.mode of the config; observe without one)
  --budget-config FILE   budget config TOML (default <state>/budget.toml)
  --deadline TIME        no attempt starts after TIME; live workers stop at it (RFC 3339 or HH:MM)
  --cap-window POOL=F    this run may use at most fraction F of POOL's window (repeatable)
  --cap-usd-estimate X   drain once the run's client-side USD estimate reaches X
  --max-verify-errors N  halt verifier_error after N consecutive could-not-verify results (3; every mode)
  --stall-quiet-max D    most silence one api_retry line may add to --stall-timeout (15m; every mode)
  --dry-run              validate the spec, print tasks and ready order; touch nothing
```

`--dry-run` prints every task with its tier, deps and deps source, then the
waves in which tasks become ready if every task passes. It opens no
database and creates no worktree.

## State directory

```
<state>/gosplan.db                                    SQLite state (one writer process)
<state>/worktrees/<run>/<task>/                  git worktrees (branch gosplan/<run>/<task>)
<state>/runs/<run>/lock                          flock held by the gosplan run driving the run
<state>/runs/<run>/events.jsonl                  v1 event log (orchestrator outbox + gosplan verify --log)
<state>/runs/<run>/state.json                    flat {task: status}, for gosplan replay --check
<state>/runs/<run>/report.json                   run report, gosplan.run-report/v1
<state>/runs/<run>/attempts/<task>.<n>.stream.jsonl   raw stream-json from the worker
<state>/runs/<run>/attempts/<task>.<n>.stderr
<state>/runs/<run>/attempts/<task>.<n>.argv.json      the worker argv (no environment)
<state>/runs/<run>/verify/<task>.<n>.<seq>/verdict.json | verdict.stderr | verdict.exit
<state>/runs/<run>/verify-logs/<task>/<n>/<seq>/          (<n>-resume/<seq>/ for a resume re-verify)
<state>/runs/<run>/attest/<task>.<n>.dsse.json
<state>/runs/<run>/mutation/<task>.<n>.json
<state>/runs/<run>/budget.jsonl                  budget events (budget_*; never in events.jsonl)
<state>/budget.toml                              budget config (optional; read once per run)
```

Every `gosplan verify` invocation gets its own `<seq>` (the lowest free one,
claimed with an atomic mkdir), so a verify left running by a crashed
`gosplan run` can never write the files a live one reads.

Run one `gosplan run` per state dir at a time. A second writer fails with
"database is locked" (SQLITE_BUSY) and writes nothing.

## Dependencies

- **Sidecar `deps`.** `deps = ["1.a", ...]` in `prompts/<id>.task.toml`
  replaces the tier-derived edges for that task (`deps_source = sidecar`).
  `deps = []` means "no deps": the task is ready at once.
- **Tier-derived fallback.** A task without a `deps` key depends on every
  task of the previous tier (`deps_source = tiers`); tier 1 has no deps. Old
  specs import unchanged.
- **Validation.** A dep must exist, must not be the task itself, and must sit
  in a lower tier. So every spec still runs under the tier-ordered bash
  orchestrator, and cycles are impossible.
- **Bash compatibility.** The bash parser ignores `deps` and reads only
  one-line arrays of double-quoted strings for `scope`. Keep sidecars in that
  form.
- **No tier barrier.** A failed, blocked or abandoned task blocks only its
  descendants (they end `abandoned`). Independent tasks keep running.

## Workers

Argv: `<claude> -p <prompt> --output-format stream-json --verbose
--permission-mode bypassPermissions --session-id <uuid> --model <model>
--max-turns <n>`, in the task worktree, in its own process group. Each
attempt gets a fresh session id. Attempt 1 runs on `--model`, later attempts on
`--retry-model`.

Environment: the orchestrator's environment minus every variable whose name
starts with `GOSPLAN_ATTEST` or `NIGHTRUNNER_`, plus `GOSPLAN_RUN`, `GOSPLAN_TASK`,
`GOSPLAN_ATTEMPT`, plus `GOSPLAN_ATTEMPT_NONCE` (a per-attempt nonce the orchestrator
uses to recognise its own orphaned workers after a restart). The attest key path is never in the argv, prompt or environment.

**Retries start fresh.** Before every launch, the runner resets the task
worktree and branch to the recomputed task base (`git reset --hard <base>`,
`git clean -fdx`). The new attempt sees the previous failure only as a summary
in its prompt.

**GOSPLAN_BLOCKED.** If the worker's final result text has a line
`GOSPLAN_BLOCKED: <reason>`, the task goes to `blocked` without verification, and
its descendants are abandoned. A line whose reason only says there is no
blocker (empty, `none`, `n/a`, `none - task complete`) is ignored and the task
is verified as usual.

## Retry, backoff and stall rules

- A worker silent on stdout for `--stall-timeout` is SIGKILLed with its whole
  process group; the attempt ends `stalled`. An attempt ends when its
  leader exits: the rest of the group is killed and stdout is read for at
  most 2s more, so a `setsid` child that keeps stdout open cannot hold the
  attempt (it can keep running, though: docs/trust-until-d7.md).
- **SIGINT/SIGTERM** abort the run: workers are killed, `gosplan run` waits at
  most 2s (stdout) + 10s (workers, verifies) and then records the run
  `aborted` and exits 2; with a responsive `gosplan.db` that is within about 15s.
  A second signal ends `gosplan run` at once.
- A non-zero exit, a stall or a `fail` verdict takes the retry path: back to
  `pending` with `not_before = now + min(retry-base*2^(n-1), 5m)` while
  attempts remain, else `failed`.
- **A usage limit does not use up an attempt.** When the worker's final
  `result` line is an error with `api_error` `usage_limit_reached` (or
  status 429 with type `rate_limit_error`) and the exit is non-zero, the
  plan's window refused the session; the task did not fail. The attempt is
  refunded (its row still ends `exited`), the task goes back to `pending`
  with reason `usage limit: waiting for reset at <time>`, and `not_before`
  is set to the latest `resetsAt` of a `rejected` `rate_limit_event` in the
  stream plus 2 minutes (5 minutes from now if the stream names no future
  reset). Every other pending task that would be ready sooner gets the same
  `not_before`, so the run does not spend one refused launch per task. No
  backoff growth, no same-failure breaker, `last_failure` unchanged. The run
  waits for the reset however far away it is; `--deadline` bounds that. Both
  waits are policy (`reconcile.Config.UsageResetSlack`, `UsageLimitBackoff`;
  no flag). The fields are undocumented claude output.
- **Infrastructure failures do not use up attempts.** An attempt whose
  worker never started (`start_failed`: worktree, prompt, launch, or
  `events.jsonl` not writable) or was lost with a crashed orchestrator
  (`lost`) is given back: `attempts_used` is not consumed and the task is
  retried after the backoff. These count toward a run-level counter
  instead; 10 in a row with no worker launched in between halt the run
  `infrastructure`. A worker that never started cannot have failed. They
  are bounded per task too, across restarts: once a task has
  `--max-attempts` attempts that ended `lost` or `start_failed` (counted
  from the `attempts` table), its next dispatch blocks it with reason
  `infrastructure: N lost/start_failed attempts` instead of launching it
  again.
- `--verify-timeout` (2h) bounds one `gosplan verify`. On timeout (or SIGINT)
  `gosplan verify` gets SIGTERM, which cancels it and makes it kill its gate
  commands' process groups; after 10s its own group is SIGKILLed. A timeout
  is a could-not-verify result.
- `gosplan verify` exit 2 (could not verify) retries the verify once after 30s;
  two in a row block the task ("could not verify"). Could-not-verify is never
  a pass.
- **Resume.** `gosplan run --run ID` after a crash treats `gosplan.db` as untrusted:
  the task rows' static columns (tier, prompt path, deps source, deps,
  scope, allow_new_protected) and the task set must match the spec as it
  is on disk, the run base and spec hash must match the log's
  `run_started` event, the log must replay to it, the integration tip must match, and every
  passed task is verified again by SHA (no cache) before anything runs.
  Then it kills its orphaned workers and ends those attempts `lost`. A lease
  records the leader's pid, pgid and `/proc/<pid>/stat` start time, and the
  per-attempt nonce the worker got as `GOSPLAN_ATTEMPT_NONCE` (written in the
  dispatch transaction, before launch). A group is killed only if its
  pgid is the leader's pid and the leader's `/proc` pgrp, and the
  leader's start time matches or its environment carries the nonce (or,
  once the leader has exited, a live member of the group still carries
  the run's nonce or `GOSPLAN_RUN`); any
  other process holding the pid (reuse, another user's, EPERM) is left
  alone and the attempt still ends `lost`. Every live process carrying one
  of the run's nonces is killed too, which covers a crash between launch
  and recording the pid. `gosplan verify` runs with
  `GOSPLAN_ATTEMPT_NONCE=<run>.verify.<task>.<n>`, so the same sweep stops a
  crashed run's verifies before the resume starts verifying: SIGTERM, so
  `gosplan verify` cancels and kills its gate commands (which run with a
  scrubbed environment and carry no nonce), then SIGKILL after 10s. Only one `gosplan run` drives a run at a time: it holds
  an exclusive `flock` on `<state>/runs/<run>/lock`, and a second
  `gosplan run --run ID` exits 2 with "run ID is in use by another gosplan run".
  Passed tasks are not re-run. Resuming a run that already ended
  (`complete`, `halted`) reports it and changes nothing: it prints the
  report rebuilt from `gosplan.db` and exits 0 for `complete`, 1 for `halted`,
  without running the checks above and without writing any DB row, log
  line, `state.json` or `report.json` (a run finished by an older `gosplan`,
  whose log has no `integration_sha=`, is reported as it ended). Use
  `gosplan replay --check` and `gosplan status` to inspect an ended run. An
  `aborted` run (SIGINT/SIGTERM) is resumed like a crashed one: the checks
  run, it goes back to `running` and continues.

## Verdicts

The verdict comes only from `gosplan verify --base <recomputed task base> --head
<head SHA resolved at worker exit>`, run as a subprocess, never with
`--cache`. A pass counts only if its JSON names that same base and head.
Nothing in the worktree (`DONE.md`, `VERDICT`, ...) counts.

## Signing: what a signature does and does not mean

Without `--attest-key`, verdicts are **unsigned** and the report notes
"signing off until D7". With `--attest-key PATH` every verdict that has JSON
(pass and fail) is signed: an in-toto v1 statement with an SLSA VSA
predicate in a DSSE envelope, ed25519, at `attest/<task>.<n>.dsse.json`. A
missing key file means unsigned plus a warning, unless `--require-attest`
is set, which then fails before any task runs.

A valid signature means only: **the holder of this key made this statement
about this tree.** It does not show that the tests ran, or ran honestly.

**Workers run unsandboxed as the same OS user** (no sandbox until D7). The key
is kept out of the repo, the worktree and the worker's argv, prompt and
environment, but a worker can still read the key file and the whole state
dir (`gosplan.db`, verdicts, envelopes). A signature can therefore look
authoritative without being unforgeable.

Check an envelope offline:

```
gosplan verify-attestation --envelope <state>/runs/<run>/attest/<task>.<n>.dsse.json \
  --pubkey <key>.pub [--tree <tree_sha from the verdict>] [--json]
```

## Mutation (advisory)

With `--mutation advisory` (default), after each **pass** the runner starts
`gremlins unleash --diff <base> ...` on a detached worktree of the head SHA, in
the background. The result is an **advisory signal about test strength on
changed lines**. It is stored on the attempt and shown with `advisory: true`.
It never changes a task's status, never blocks a dependent and never enters
the signed verdict. A missing gremlins binary is status `unavailable`. The run
waits for in-flight mutation runs before it writes the report.

Where it runs: each mutation run makes a temp dir `$TMPDIR/mutation-*` (Go's
`os.TempDir()`, usually `/tmp`), holding the detached worktree of the head
(`tree/`, registered in the project repo with `git worktree add --detach`)
and its `gremlins.yaml`. The dir and the worktree registration are removed
when the run ends. The report is `<state>/runs/<run>/mutation/<task>.<n>.json`.

## Windows and cost

Window rows come from the workers' `rate_limit_event` / `api_retry` stream
lines and the `/usage` probe text. `unifiedWindows`, `rateLimitType` and the
`/usage` text layout are **undocumented**: rows from them carry
`documented: false`. Only the budget gate (below) decides anything from
them, and only in `enforce` mode or for the run flags; in `observe` mode it
logs what it would decide. `cost_usd_estimate` is the client-side list-price
estimate claude reports, not billing or plan usage.

## Budget

`gosplan run` has a budget gate that can hold back an attempt when the plan's
rate-limit windows look too full for it. The full operator guide, with the
config keys, the surfaces and what the numbers do and do not mean, is
[gosplan-budget.md](gosplan-budget.md). This section covers what `gosplan run` itself does.

**Modes.** `--budget off|observe|enforce`. Without the flag the mode is
`budget.mode` of the config file (`--budget-config FILE`, default
`<state>/budget.toml`). A config file must set `budget.mode`, or the run
exits 2 with `budget.mode must be set`. With no config file the mode is
`observe`, and `gosplan run` prints
`gosplan run: budget: no config at <path>; observe mode (decisions logged, not enforced)`
(the same text is a report note).

| mode | what `gosplan run` does |
|---|---|
| `observe` (default) | every admission decision, breaker evaluation and settlement is computed and logged with `"enforced": false`; dispatch proceeds as without a gate |
| `enforce` | refusals park the task (it stays `pending` with a later `not_before`), breakers act, belts (`--max-budget-usd`, when `belt.usd_factor` is set) are passed to the worker. Requires an explicit `budget.cold.unit_fraction` |
| `off` | no budget code runs; `--deadline`, `--cap-window` and `--cap-usd-estimate` are rejected (exit 2) |

**Run flags** `--deadline`, `--cap-window` and `--cap-usd-estimate` are
enforced whenever they are given, in `observe` and `enforce` mode alike:
they are the operator's explicit request for this run. A deadline stops the
run (halt `deadline`); a cap drains it (halt `cap:…`). Either way the
pending tasks stay `pending` (parked, not abandoned), the report's
`budget.halt.parked` lists them, and `gosplan run --run ID` can resume them later.

**Two flags are always on**, in every mode and with no gate at all, because
they are not budget code:

- `--max-verify-errors N` (default 3, **policy**): N consecutive
  could-not-verify results across the run halt it `verifier_error`
  (the per-task limit of two still blocks the task first).
- `--stall-quiet-max D` (default 15m, **policy**): a worker's
  `system/api_retry` line extends its stall deadline by
  `min(retry_delay_ms, D)`, so claude's own rate-limit wait is not killed as
  a stall, and one line cannot buy an unbounded wait.

**Where the events are.** Budget events go to
`<state>/runs/<run>/budget.jsonl` (types `budget_input`, `budget_decision`,
`budget_settle`, `budget_breaker`, `budget_fit`, `budget_probe`,
`budget_halt`), with the same envelope as `events.jsonl` but never in it.
They are written to `gosplan.db` (`budget_events`) inside the transaction that
made the decision and flushed to the file on every tick and at the end of the
run. `gosplan replay --check` recomputes every recorded decision from its recorded
inputs when `budget.jsonl` sits next to `--log`; that shows each decision
follows from its inputs, not that the inputs were true.

**What it does not claim.** Admission reduces rate-limit stalls; it does not
prevent them. Token and USD figures are estimates. Window utilisation from
`unifiedWindows`, `rateLimitType` and `/usage` text is undocumented.
`--max-budget-usd` does not cap plan usage. Every budget default is policy.

Inspect it with `gosplan budget`, `gosplan budget explain TASK` and
`gosplan budget calibrate` ([gosplan-budget.md](gosplan-budget.md)); `gosplan status` prints one
`budget:` line and `report.json` has a `budget` block.

## Inspecting a run

```
gosplan status --state-dir <state> [--run ID] [--json]     # tasks, attempts, verdicts, mutation, windows
cat <state>/runs/<run>/report.json
gosplan replay --log <state>/runs/<run>/events.jsonl --state <state>/runs/<run>/state.json --check   # + budget.jsonl when present
gosplan budget --state-dir <state> [--json]                  # pools, headroom at the horizon, reservations, breakers
gosplan budget explain TASK --state-dir <state> [--run ID]   # why a task is or is not admitted now
gosplan budget calibrate --state-dir <state> [--json]        # estimate and projection checks
gosplan verify-attestation --envelope ... --pubkey ... --tree ...
git -C <project> log gosplan/<run>/integration
```
