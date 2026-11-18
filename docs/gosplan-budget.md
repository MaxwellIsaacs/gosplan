# gosplan budget: operator guide

`gosplan` can hold back an attempt when the plan's rate-limit windows look too
full for it. This is the budget gate. It decides from numbers the plan
reports about itself and from this machine's own history, and several of
those numbers are undocumented, so read everything here as "what the gate
believes", not "what is true". Every default below is **policy**: a choice,
not a measurement.

## What the gate claims, and does not

- Admission **reduces** rate-limit stalls. It does not prevent them: the
  level it sees lags the plan, and other clients share the plan.
- A token figure is an **estimate** of how many tokens an attempt will use
  (a quantile of this machine's finished attempts). It is never "the cost".
- A fitted ratio (utilisation per million tokens) is a regression over
  observed intervals, with a basis (`fit`, or `none:<reason>`) and an n. It
  is not the plan's formula, and the plan has not told us it has one.
- Window utilisation read from `unifiedWindows`, `rateLimitType` or the
  `/usage` text is undocumented. Every surface that shows one says so
  (`(undocumented)` in text, `"documented": false` in JSON).
- `--max-budget-usd` does not cap plan usage. It is a client-side list-price
  estimate passed to the worker and it can overshoot.
- Class fractions and every `budget.*` default are policy.
- Admission is not "conservative by construction": the projection can be
  over- or understated (see headroom below).
- `gosplan replay --check` is not proof that the recorded inputs were true (see
  "What replay proves").

## Modes and the observe → enforce ladder

`budget.mode` is `off`, `observe` or `enforce`.

| mode | what happens |
|---|---|
| `off` | no budget code runs |
| `observe` | every decision, breaker evaluation and settlement is computed and logged with `"enforced": false`; nothing is applied and admission proceeds as before. The mode when no config file exists |
| `enforce` | refusals park the task, breakers act, belts are passed |

Run flags (`--deadline`, `--cap-window`, `--cap-usd-estimate`) are always
enforced when given, in observe and enforce mode; `--budget off` rejects
them (exit 2). Two rules are not budget code and stay on in every mode, with
or without a gate: the `verifier_error` halt (`--max-verify-errors`, default
3) and the api_retry quiet allowance (`--stall-quiet-max`, default 15m). See
[gosplan-run.md](gosplan-run.md#budget).

The config is read **once** at run start, hashed (`config_sha256`, recorded
in `budget_runs` and in every decision) and never re-read mid-run.

The ladder is the point of observe mode:

1. Run in `observe` for about a week of real runs. Nothing changes for the
   runs; the gate collects observations, estimates and decisions.
2. Run `gosplan budget calibrate` and read the one-week check below.
3. If every check passes (or is `n too small` only because you ran little),
   set `budget.mode = "enforce"` **and** `budget.cold.unit_fraction`
   explicitly (enforce refuses a defaulted unit fraction).
4. If any check fails, stay in `observe` until it is fixed.

A config file must name its mode. A file written only to set an alias does
not opt in to enforce.

## Config keys

The file is `<state>/budget.toml` (or `--budget-config FILE`). Unknown keys
are an error. All of these are policy unless the key says otherwise:

`mode`, `stale_after`, `probe_min_interval`, `lookback`, `max_horizon`,
`reset_epsilon`, `jitter`, `recheck_max`, `stale_recheck`,
`fairness_recheck`, `run_ttl`, `stale_max_open`, `model_families`,
`alias.<source_window>`, `estimate.{quantile,min_samples,window,cold_tokens,safety_factor,cold_duration}`,
`fit.{lookback,min_points,max_rel_se,max_interval}`, `cold.unit_fraction`,
`belt.usd_factor`, `breaker.retry_storm.{retries,window,cooldown}`,
`breaker.same_failure.attempts`,
`breaker.rate_limited.{cooldown_base,cooldown_max}`, `routing.enabled`, and
the class table (`class.<name>.rank`, `.reserve_fraction`).

`reset_epsilon` is the one with an observed basis (utilisation is reported
in whole percents, so a drop under one point is rounding). `gosplan budget` ends
with `policy defaults in effect: <keys>` so you can see which values you
chose and which you inherited.

## Reading the surfaces

- `gosplan budget [--json]`: each pool with its level, age, source (marked
  undocumented), burn rate and its basis, projected level and **headroom at
  the horizon**, the open reservations and breakers, and the estimates.
- `gosplan budget explain TASK [--run ID]`: builds the snapshot the gate would use
  now and says why the task would or would not be admitted, per pool, plus
  the last recorded decision for it. `admit_at` is when it would fit if
  nothing else changes. It decides with the run's **recorded** mode and
  effective config (its `budget_runs` row), so a run started with
  `--budget-config` elsewhere is explained as that run's gate sees it; an
  explicit `--budget-config` overrides both. It has no in-process
  observation floor, so a running gate may see a slightly higher level. It
  never writes.
- `gosplan status` prints one `budget:` line; `report.json` has a `budget` block
  (mode, pools, this run's reservations, breakers, estimates, decision
  counts by rule, `halt` with its `parked` tasks, notes). `halt.parked` is
  every task the halt left `pending`, including tasks that were never
  refused (for example a dependent of a task the deadline killed).
- `gosplan budget calibrate [--since DUR] [--json]`: the checks below.

### Reading a decision

Each `budget_decision` line carries `mode`, `enforced` and the full
`decision`. `decision.admit` and `decision.rule` are what the rules say, in
both modes. In `enforce` mode that is also what happened. In `observe` mode
a refusal by any rule except the run flags (`halt`, `deadline`, `cap`) is
recorded with `admit: false` and `enforced: false`, a note says `observe:
rule <rule> not enforced (enforce mode would refuse)`, and the attempt
starts anyway. An attempt admitted under the bootstrap or stale rule
carries `admit: true` with `rule: bootstrap` or `rule: stale` (the stale
pools are in `stale_pools`); `rule: admit` means every applicable pool was
fresh and fit. The report's `decisions` counts follow `decision.admit`, so
in `observe` mode `refused` counts the refusals enforce mode would have
made, not attempts that were held back.

Reservations count in a pool's `open_reserved` while open, and after they
settle, until a newer observation of the pool arrives. A reservation made
before a pool was ever observed (every bootstrap admit) has no entry for it
in its row; decisions count it as one cold unit (`cold.unit_fraction`,
basis `unit`) in every pool that applies to its model and was first
observed after it was made, because its use lands there.

A refusal in enforce mode parks the task: a `budget_parks` row and a later
`not_before`; the task stays `pending`. Any settle deletes the re-armable
parks (`headroom`, `class_hold`, `fairness`, `bootstrap`, `stale`) of every
run, and each run re-offers its parked tasks within about one run-key
interval. A refusal identical to the task's previous one is not logged
again; the next logged decision for the task carries `suppressed: n`.

### Breakers

Every open and close is a `budget_breaker` event with its inputs; in
observe mode they are recorded with `enforced: false` and change nothing.

- `RETRY_STORM` (run): `breaker.retry_storm.retries` retries started within
  `breaker.retry_storm.window` (refunded `start_failed`/`lost` predecessors
  do not make a retry) opens it for `breaker.retry_storm.cooldown`; while
  open no attempt of the run is admitted (rule `breaker`).
- `SAME_FAILURE` (task): the last `breaker.same_failure.attempts` attempts
  of a task with the same failure signature (no commit, or the same failed
  gates) send it to `blocked` instead of another retry. It never closes on
  its own.
- `RATE_LIMITED` (global): a `rejected`/`rate_limited` observation stops
  new admissions until the reset (or a backoff between
  `breaker.rate_limited.cooldown_base` and `cooldown_max`). Running workers
  are not killed.
- `VERIFIER_ERROR` is logged only; the halt is the reconciler's
  (`--max-verify-errors`).

### Headroom at the horizon, not at the reset

A refused attempt's headroom is judged at the **horizon**: the pool's reset
when the reset is within `max_horizon` of the observation, else `now +
max_horizon`. With the defaults, a pool whose reset is days away (seven_day)
is judged two hours out, not at its reset. Every surface prints the
`horizon_basis` (`reset` or `max_horizon`); call it "headroom at the
reset" only when it says `reset`.

## `gosplan budget calibrate` and the one-week check

`calibrate` reads `gosplan.db` read-only and every run's `budget.jsonl` and
prints, for each number, what it is and how much data is behind it.

- **estimates**: per model, n samples, p50/p90/p99 of attempt tokens, and the
  overrun rate: the share of settled reservations (with both an estimate and
  a known actual) whose actual tokens exceeded the estimate. If the quantile
  is calibrated, the expected rate is `1 − quantile` (0.10 at p90).
- **fitted ratios**: per pool, with `residual_rms`, n and basis.
- **refusals by rule**.
- **projection error**: for decisions whose horizon ended at a reset that has
  since passed, the projected level minus the last level observed inside
  `max(10 min, 5% of the horizon)` before the reset. `mean > 0` means we
  over-projected (cautious); `< 0` means we under-projected.
- **alias evidence**: see below.
- **segments per pool**: how many reset segments the observations split into
  and which rule made each split (`drop`, `resets_at_jump`, `past_reset`).
  The 60 s `resets_at` jump rule assumes `resetsAt` is fixed within a window;
  that rests on one sample. If every pair splits, the rule is wrong for that
  source.

The **one-week check** prints each item as `PASS`, `FAIL` or `n too small`:

1. `overrun_rate` per model within ±0.05 of `1 − q`, with n ≥ 20.
2. Every pool with a `fit` basis has `residual_rms` below the typical
   (median) reservation made in that pool.
3. Projection error `mean ≥ 0` and `mean_abs` below `cold.unit_fraction`.
   `calibrate` prints `n too small` under 5 items; that limit is a display
   choice of the command, not a budget policy.
4. Refusals are mostly `headroom` / `class_hold`, not `stale` / `bootstrap`.
   If `stale` dominates, the stream is not refreshing pools: set an alias or
   lower the run width.

Any `FAIL` means the model is not working: run in `observe` until it is
fixed. A pass with a small n says little; the n is printed next to it.

### Aliasing `session` ↔ `five_hour`

Some sources name the same window differently (`session` in the `/usage`
text, `five_hour` in the rate-limit stream). Set `budget.alias.session =
"five_hour"` only after the `alias evidence` line shows many pairs with a
small `mean_abs_diff`. An alias can create false resets: a `/usage` row up to
60 minutes old can sit several points below the stream and split the
segment, so one agreeing pair is not a case for aliasing.

## Sharing a state dir

Several `gosplan run` processes may share one state dir (one SQLite file, one
writer at a time). They see each other's reservations, breakers and parks,
because the snapshot is read inside the dispatch transaction. The limit: the
default state dir is per project (`<project>-<hash>`). Runs of different
projects with different state dirs do not see each other's reservations, only
each other's usage, late, through observations. To share, pass the same
`--state-dir`.

## Events and replay

Budget events go to `<state>/runs/<run>/budget.jsonl`, not `events.jsonl`.
Inputs are logged once per file (`budget_input`) and each `budget_decision`
names them by id.

`gosplan replay --log events.jsonl --state state.json --check [--budget-log FILE]`
keeps its v1 behaviour. When `--budget-log` is given, or a `budget.jsonl`
sits next to `--log`, it also rebuilds each decision's snapshot from the
`budget_input` lines **before** it, recomputes `budget.Decide`, recomputes
each breaker change and each fit, and compares. Success adds
`ok: <n> budget decisions recomputed`. Any difference exits 1 with
`budget drift: eid <n> (<type>): <first differing field>`. A decision that
names an input id with no earlier `budget_input` is drift.

### What replay proves, and what it does not

Replay shows that each recorded decision **follows from its recorded
inputs**. It does not show that the inputs were true. The inputs come from
`gosplan.db` rows a worker can write, and the log is writable by the same user, so
an edit of an input line **and** of the decision that used it replays clean.
The test suite asserts exactly that, to keep the limit visible. Within one
run the end-of-run self-check compares `budget.jsonl` with the decisions the
process made, so a mid-run edit of the log halts the run; an edit after the
run ends is not detected.

## Trust limits

Budget state is worker-writable until the worker sandbox (D7) gives workers their own uid: a
worker can edit `window_observations`, reservations and `budget.jsonl`. The
gate keeps what it can in memory and halts on a mismatch, and foreign
observation rows may only tighten a pool, but none of this is a boundary.
See [trust-until-d7.md](trust-until-d7.md).

## Not built

- Deficit round robin between runs: fairness is strict class priority, then
  smaller settled consumption, then FIFO.
- `/api/oauth/usage` as a source.
- `--fallback-model` routing on a refusal.
- Parsing `/usage` reset times from its text.
- Bucketing estimates by prompt size.
