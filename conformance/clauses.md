# Clause index

PROTECTED. A human reviews every change by hand.

Every conformance test names one or more clauses with a header line
`// Clause: <file>#<id>` in its doc comment. The meta-check
(`conformance/meta_test.go`) fails if a test has no such line, or names an
id that is not a row of the tables below. A clause id is the first column of
a table row. `<file>` names the contract the clause comes from. The
contracts are design notes that are not published; each clause's
statement below is the contract.

| short name | contract |
|---|---|
| `00-event-schema.md` | the v1 event log schema |
| `01-verify-contract.md` | the verifier contract (rev 4) |
| `02-protected-config.md` | protected and locked config |
| `events-writer` | writer guarantees documented in `internal/events/events.go` (package and `Log` docs) |
| `orchestrator` | `gosplan run`, `gosplan status`, the SQLite state, attestation, mutation, windows |
| `budget` | the budget gate: pools, admission, breakers, surfaces |
| `suite` | the conformance suite's own rules |

Kind: **G** = green now, **R** = red by design (not built yet). No clause is R now.

## Event log

| clause | kind | statement | source |
|---|---|---|---|
| `00-event-schema.md#EV-ENVELOPE` | G | Every line is one JSON object with `v`=1, `seq`, `ts` (RFC 3339 UTC), `run`, `task` (empty for run-level), `type`, `data` (object). | Envelope |
| `00-event-schema.md#EV-SEQ` | G | `seq` is per file, starts at 1 and is strictly increasing with no gaps, including across separate writers appending to one file. | Envelope |
| `00-event-schema.md#EV-WRITER-ORDER` | G | `gosplan verify --log` writes, in order: `launch` (only with `--task-base` or `--attempt`; only `attempt` and `base_sha` filled), one `gate` per gate in gate order, one `verdict`. | Writer |
| `00-event-schema.md#EV-WRITER-RUNID` | G | `--run ID` sets the envelope `run`. | Writer |
| `00-event-schema.md#EV-WRITER-NOLOG` | G | No `--log` flag, no log file. `--run`/`--task-base`/`--attempt` without `--log` is a usage error (exit 2). | Writer |
| `00-event-schema.md#EV-WRITER-INFRA` | G | An infrastructure error (exit 2) writes no `gate` or `verdict`. | Writer |
| `00-event-schema.md#EV-VERDICT-ONCE` | G | One `verdict` per verification, cache hit included, with `cache_hit: true`. | Types |
| `00-event-schema.md#EV-REPLAY` | G | `gosplan replay --check` exits 0 when replayed `task_state` statuses equal `state.json`, 1 on mismatch and prints the first differing task. | Replay contract |
| `00-event-schema.md#EV-VALIDATE` | G | Reading a log rejects unknown `v`, non-monotonic `seq`, unknown `type`, a `task_state` whose `from` is not the prior `to`, and `verdict.result=pass` with non-empty `failed_gates`. | Validation |
| `events-writer#EV-FLOCK` | G | Each append holds an exclusive `flock` on the log file and takes seq from the file, so concurrent writers (several `gosplan verify --log`, or any process holding the lock) never interleave or duplicate a seq; a writer waits while another holds the lock. | events.go `Log` doc |
| `events-writer#EV-TORN` | G | An unterminated last line (torn write) is moved to `<log>.torn-<unix-ts>`, the log is truncated to its last newline with a warning on stderr, and appending continues with the next seq. | events.go `quarantineTornTail` doc |
| `events-writer#EV-BADLINE` | G | A complete (newline-terminated) but invalid line is not repaired: the writer refuses the log (`gosplan verify --log` exits 2) and leaves the file unchanged. | events.go `quarantineTornTail` doc |

## Verifier (contract rev 4)

| clause | kind | statement | source |
|---|---|---|---|
| `01-verify-contract.md#VC-REFS` | G | `--base`/`--head` must be full 40-char SHAs; refs and short ids are rejected with exit 2. | Invocation |
| `01-verify-contract.md#VC-GATE-ORDER` | G | Gates run in contract order, all of them, even after a failure: `object_integrity, committed_work, locked_paths, protected_paths, scope, commands_present, clean_checkout, cmd:<name>…`. | Gates; event schema `gate` |
| `01-verify-contract.md#VC-RESULT` | G | `result` is `pass` only if no gate failed; skipped gates do not fail a verdict; `failed_gates` lists exactly the failed gates. | Gates |
| `01-verify-contract.md#VC-OBJECT-INTEGRITY` | G | A transfer failure (object mismatch, shallow root, missing history) is an `object_integrity` FAIL, not a pass and not exit 2. | Trust model 1 |
| `01-verify-contract.md#VC-COMMITTED-WORK` | G | `committed_work` fails without at least one commit beyond base that changes at least one file; an empty commit is not work. | Gate 1 |
| `01-verify-contract.md#VC-LOCKED-PATHS` | G | Any change to a locked file fails `locked_paths`; project `[verify.locked]` globs are added to the defaults, never substituted. | Gate 2 |
| `01-verify-contract.md#VC-LOCKED-DEFAULTS` | G | Default locked paths, always in force: `nightrunner.toml`, `.gitattributes`, `.mcp.json`, `.claude/**`, `CLAUDE.md`. | Gate 2 |
| `01-verify-contract.md#VC-PROTECTED-PATHS` | G | Modify, delete or type change of a protected file fails `protected_paths`. Defaults `**/*_test.go`, `tests/**`, `**/testdata/**` always apply. | Gate 3 |
| `01-verify-contract.md#VC-PROTECTED-NEW` | G | An added file matching a protected glob fails unless `--allow-new-protected`. | Gate 3 |
| `01-verify-contract.md#VC-SCOPE` | G | Every changed path must match a `--scope` glob, else `scope` fails naming the path. | Gate 4 |
| `01-verify-contract.md#VC-SCOPE-UNRESTRICTED` | G | Without `--scope` the scope gate is `skip` and the verdict records `scope_unrestricted: true`. | Gate 4; 02 config |
| `01-verify-contract.md#VC-COMMANDS-PRESENT` | G | No configured verification command fails `commands_present`. | Gate 5 |
| `01-verify-contract.md#VC-CLEAN-CHECKOUT` | G | An export failure (symlink, submodule, unsafe path) fails `clean_checkout`, never skips. | Gate 6; Trust model 3 |
| `01-verify-contract.md#VC-CMD` | G | `cmd:<name>` passes only if the command exits 0 within its timeout, in a clean export of head (uncommitted worker files are absent). | Gate 7 |
| `01-verify-contract.md#VC-CONFIG-FROM-BASE` | G | Config is read from base; editing `nightrunner.toml` on the task branch cannot change the commands that run. | Trust model 2; 02 config |
| `01-verify-contract.md#VC-EXIT` | G | Exit 0 pass, 1 gate(s) failed, 2 could not verify (bad refs, missing config, git failure, cancellation, signal). | Errors |
| `01-verify-contract.md#VC-CANCEL` | G | A cancelled or signalled run exits 2 and still removes its export dir, HOME dir and command process groups. | Errors |
| `01-verify-contract.md#VC-CACHE-HIT` | G | With `--cache`, a second verification of the same tree with the same key is a hit (`cache_hit: true`) with the same result. | Cache |
| `01-verify-contract.md#VC-CACHE-KEY` | G | The key covers commands (name, shell, timeout), protected, locked and scope globs: changing any of them misses. | Cache |
| `01-verify-contract.md#VC-CACHE-PASS-ONLY` | G | Entries are written only when gates 6–7 pass: a failed verdict is never served from cache. | Cache |
| `01-verify-contract.md#VC-CACHE-INVALID` | G | Empty or partial entries are misses, never hits and never a crash. | Cache |
| `01-verify-contract.md#VC-CACHE-SCOPE-OF` | G | The cache stores only gates 6–7: gates 1–5 are evaluated on every run, so a hit cannot hide a locked/protected/scope failure. | Cache |

## Config

| clause | kind | statement | source |
|---|---|---|---|
| `02-protected-config.md#CF-COMMANDS` | G | `test`, `lint`, `build`, then each `custom[]` entry in order, each only if set, run through `bash -c` in the clean checkout; gate names `cmd:test`, `cmd:lint`, `cmd:build`, `cmd:custom[i]`. | Config |
| `02-protected-config.md#CF-TIMEOUTS` | G | `[verify.timeouts]` keys in seconds; `custom_seconds` applies to every custom entry; defaults (600/120/600/300) do not kill a short command. | Config |
| `02-protected-config.md#CF-PROTECTED-DEFAULT` | G | `[verify.protected] paths` defaults to the three globs; project globs add to them. | Config |

## Orchestrator

| clause | kind | statement | source |
|---|---|---|---|
| `orchestrator#DY-RUN-E2E` | G | `gosplan run --spec D --project P` imports a spec dir and runs every task to `passed`; the run ends `complete` with exit 0. | design notes |
| `orchestrator#DY-RUN-EXIT` | G | `gosplan run` exits 0 `complete`, 1 `halted`, 2 error or `aborted`. | 5.a |
| `orchestrator#DY-SEQ-TIERS` | G | `sequence.txt` grammar: `#` comments, `->` tiers, `||` parallel, parentheses ignored; without a `deps` key a task depends on every task of the previous tier (`deps_source = tiers`). | design notes |
| `orchestrator#DY-SEQ-VALIDATE` | G | Import fails and writes nothing on a dep not in the sequence, a self dep, a dep whose tier is not lower, a duplicate id, a missing prompt. | design notes |
| `orchestrator#DY-DEPS-EXPLICIT` | G | A sidecar `deps` key (even `[]`) replaces the tier-derived edges for that task (`deps_source = sidecar`). | design notes |
| `orchestrator#DY-NO-BARRIER` | G | A failed, blocked or abandoned task blocks only its descendants; independent tasks keep running (no tier barrier). | design notes |
| `orchestrator#DY-ABANDON` | G | At run end, pending tasks with a failed/blocked/abandoned ancestor become `abandoned`; the run is `halted` unless every task passed. | design notes |
| `orchestrator#DY-DEP-ORDER` | G | A task starts only after all its deps `passed`. | design notes |
| `orchestrator#DY-READY-ONCE` | G | A task never has two workers at once. | design notes |
| `orchestrator#DY-CAPACITY` | G | Never more than `--max-parallel` tasks running or verifying. | design notes |
| `orchestrator#DY-WORKQUEUE` | G | Dispatch goes through `k8s.io/client-go/util/workqueue` rate-limiting queue (linked into `gosplan`). | design notes |
| `orchestrator#DY-LEASE-RECLAIM` | G | After an orchestrator crash, a restart finds `running` tasks with no live handle, kills the lease's process group, ends the attempt `lost` and retries. | design notes |
| `orchestrator#DY-STALL` | G | A worker with no stdout line for the stall timeout has its whole process group SIGKILLed; the attempt ends `stalled` and goes down the retry path. | design notes |
| `orchestrator#DY-BACKOFF` | G | Retry delay before attempt n+1 is `min(base·2^(n-1), 5m)` (base 10s default, `--retry-base`); attempts are capped by `--max-attempts`, then `failed`. | design notes |
| `orchestrator#DY-RETRY-MODEL` | G | Attempt 1 uses `--model` (default sonnet), later attempts `--retry-model` (default opus), each with a fresh `--session-id`. | design notes |
| `orchestrator#DY-ARGV` | G | Worker argv carries `-p`, `--output-format stream-json`, `--verbose`, `--permission-mode bypassPermissions`, `--session-id`, `--model`, `--max-turns`. | design notes |
| `orchestrator#DY-ENV` | G | Worker env drops every `GOSPLAN_ATTEST*` and `NIGHTRUNNER_*` var and adds `GOSPLAN_RUN`, `GOSPLAN_TASK`, `GOSPLAN_ATTEMPT`. | design notes |
| `orchestrator#DY-RESUME` | G | `gosplan run --run ID` after a crash resumes the run without re-running tasks that already passed. | design notes |
| `orchestrator#DY-VERDICT-SOURCE` | G | A verdict comes only from `gosplan verify`; `DONE.md`, `VERDICT` or any worktree file has no effect. | design notes |
| `orchestrator#DY-NR-BLOCKED` | G | A result whose text has a line `GOSPLAN_BLOCKED: <reason>` sends the task to `blocked` without verifying. | design notes |
| `orchestrator#DY-INTEGRATION` | G | Passed heads are merged into `gosplan/<run>/integration`; `master` and the checked-out branch are never written. | design notes |
| `orchestrator#DY-OUTBOX` | G | The run's `events.jsonl` passes validation, has no duplicate event, and `gosplan replay --check` agrees with `state.json`. | design notes |
| `orchestrator#DY-SQLITE-WAL` | G | `gosplan.db` is SQLite in WAL mode with `user_version = 2`: exactly the 8 v1 tables with their v1 columns (plus `leases.nonce`, `leases.start_time` at the end), plus the 6 budget tables with their columns. (Amended in a human-reviewed commit when the budget tables were added.) | design notes |
| `orchestrator#DY-SQLITE-IMMEDIATE` | G | Writes use `BEGIN IMMEDIATE` with busy_timeout 5000: while another writer holds the write lock, `gosplan run` gets SQLITE_BUSY and fails without a partial write; read-only `gosplan status` still works. | design notes |
| `orchestrator#DY-ATTEST-DSSE` | G | Each verdict signs to a DSSE envelope (payloadType `application/vnd.in-toto+json`) holding an in-toto v1 Statement with SLSA VSA predicate, subjects `gitTree`/`gitCommit`, `verifiedLevels` `GOSPLAN_GATE_<gate>` for passed gates only, none starting `SLSA_`. | design notes |
| `orchestrator#DY-ATTEST-VERIFY` | G | `gosplan verify-attestation` exits 0 on a valid envelope and 1 on a tampered payload, a wrong key, or a tree mismatch. | design notes |
| `orchestrator#DY-ATTEST-MEANING` | G | `gosplan verify-attestation` prints that a valid signature does not show that the tests ran, or ran honestly. | design notes |
| `orchestrator#DY-KEYGEN` | G | `gosplan keygen --out P` writes a PKCS#8 key and `P.pub`, and refuses (exit 2, no file) a path inside a git work tree. | design notes |
| `orchestrator#DY-ATTEST-RUN` | G | `gosplan run` with a key signs every verdict; each `attestation_path` in the report verifies with the public key. | design notes |
| `orchestrator#DY-MUTATION-ADVISORY` | G | Mutation runs only after a pass, is marked `advisory: true`, and never changes a status, blocks a dependent or fails the run, even when gremlins exits 10. | design notes |
| `orchestrator#DY-MUTATION-ARGV` | G | gremlins gets `unleash --diff <base_sha> --config … -o …` and never a `--threshold-*` flag; a missing binary is status `unavailable`. | design notes |
| `orchestrator#DY-WINDOW-STREAM` | G | A `rate_limit_event` line yields a `rate_limit_event` row (window from `rateLimitType`, documented 0) plus one `rate_limit_event.unifiedWindows` row per entry (documented 0) with utilization and resets_at. | design notes |
| `orchestrator#DY-WINDOW-FALLBACK` | G | Missing fields degrade, never fail: no `rateLimitType` → window `unknown` documented 1; `api_retry` with `error: rate_limit` → `unknown`/`rate_limited`/`api_retry`; unparsable lines are ignored and the task still passes. | design notes |
| `orchestrator#DY-WINDOW-USAGE` | G | The `/usage` fallback maps `Current session` → `session`, `Current week (all models)` → `week_all`, `Current week (<Name>)` → `week_<name>`, percent → fraction, `resets_at` NULL, source `usage_text`, documented 0; no match → one `_probe` row `parse_failed`; `--usage-probe=false` never calls it. | design notes |
| `orchestrator#DY-STATUS-SCHEMA` | G | `gosplan status --json` prints the `gosplan.run-report/v1` report with every documented field; `<state>/runs/<run>/report.json` is the same report; cost is `cost_usd_estimate`; `notes` carry the claim limits. | design notes |
| `orchestrator#DY-STATUS-TEXT` | G | `gosplan status` text shows the run status, one line per task, the mutation column marked `(advisory)` and `(undocumented)` windows; missing DB → exit 2. | design notes |
| `orchestrator#CORE-CONTRACT` | G | `internal/core` exists with the contract's exported names, fields and types verbatim (checked by `coreprobe/`). | design notes |
| `orchestrator#CORE-RETRYDELAY` | G | `core.RetryDelay(n)` = `min(10s·2^(n-1), 5m)`, n < 1 treated as 1. | design notes |
## Budget

| clause | kind | statement | source |
|---|---|---|---|
| `budget#BG-SCHEMA` | G | `gosplan.db` moves to `user_version = 2` by adding the 6 budget tables only; a v1 DB is migrated in place (after `migrateLeases`), v1 tables, columns and rows unchanged. | design notes |
| `budget#BG-MODE` | G | Without a budget config the run is in `observe` mode: it says so on stderr and in the report, enforces nothing, and still logs every decision with `enforced: false`. | design notes |
| `budget#BG-EVENTS` | G | Budget events go to `<run>/budget.jsonl` with the v1 event envelope (v 1, contiguous seq, run, ts, eid), types `budget_*` only; every input row a decision names (config, observations, spans, samples) is logged once as `budget_input` before it; one settle per attempt; `events.jsonl` carries no budget type. | design notes |
| `budget#BG-BOOTSTRAP` | G | With no observation at all, enforce mode admits exactly one attempt at a time (rule `bootstrap`) and never deadlocks. | design notes |
| `budget#BG-POOLS` | G | A pool per observed window (rateLimitType, unifiedWindows, /usage names), level from the newest row with utilisation, with source, documented flag and age; `week_`/`_<family>` pools apply only to that model family. | design notes |
| `budget#BG-ADMIT` | G | An attempt is admitted only if, in every applicable pool, projected headroom at the horizon (the reset, or `max_horizon` when that is sooner) minus live open reservations (all runs, plus those settled after the pool's newest observation) minus class hold covers its reserve; a refusal names rule and pool and records every input number. | design notes |
| `budget#BG-PARK` | G | A refused task stays `pending` with `not_before` no later than the projected admit time (the observed reset) and starts only after it. | design notes |
| `budget#BG-STALE` | G | A pool older than `stale_after` or past its reset is stale: the gate runs the $0 /usage probe during the run, and a still-stale pool admits under rule `stale` only (`stale_max_open`). | design notes |
| `budget#BG-SETTLE` | G | When an attempt ends its reservation is settled with the result's usage (sum of the four token classes) and a `budget_settle` event. | design notes |
| `budget#BG-EST` | G | The token estimate is the nearest-rank quantile (default p90) of the same model's prior attempts across runs, with n and basis `history…`; below `min_samples` the basis is `cold…`. | design notes |
| `budget#BG-CLASS` | G | A class's `reserve_fraction` of projected headroom (a share of headroom, not of the window) is held back from lower classes (rule `class_hold`); higher classes go first. | design notes |
| `budget#BG-SHARED` | G | `gosplan run` processes sharing a state dir see each other's open reservations in the admission snapshot, so they cannot both claim the same headroom. | design notes |
| `budget#BG-RATELIMITED` | G | A `rejected`/`rate_limited` observation opens RATE_LIMITED: no new attempt starts until the observed reset; the worker that saw it is not killed; open and close are events and the row ends closed. | design notes |
| `budget#BG-HEARTBEAT` | G | An `api_retry` line extends the stall allowance by its `retry_delay_ms`, so a worker waiting on a retry is not killed as stalled (all modes). | design notes |
| `budget#BG-SAMEFAILURE` | G | Three consumed attempts of a task with the same no-commit or failed-gates signature send it to `blocked` with reason `SAME_FAILURE`, even with attempts left. | design notes |
| `budget#BG-RETRYSTORM` | G | N retries started within the window open RETRY_STORM; no attempt of the run starts before the cooldown ends. | design notes |
| `budget#BG-VERIFIERERR` | G | `--max-verify-errors` consecutive could-not-verify results across the run halt it `verifier_error` (exit 1), in every mode (the reconciler's rule); later tasks never start; the gate logs a VERIFIER_ERROR breaker event. | design notes |
| `budget#BG-DEADLINE` | G | At `--deadline` the run stops, ends `halted` (exit 1) with reason `deadline` and the parked tasks listed and left `pending`; a past deadline starts nothing. | design notes |
| `budget#BG-BELT` | G | In enforce mode with cost history, the worker gets `--max-budget-usd` = quantile cost estimate × `usd_factor`, rounded up to cents; observe mode passes none. | design notes |
| `budget#BG-CAPS` | G | `--cap-usd-estimate` drains the run once settled cost estimates reach it: no new attempt, run `halted` with reason `cap:usd_estimate`, later tasks `pending`. | design notes |
| `budget#BG-ROUTING` | G | With `budget.routing.enabled`, a sidecar `prefer` list picks the first model whose pools admit; with routing off the list is ignored with a warning. | design notes |
| `budget#BG-SURFACE` | G | `gosplan budget [--json]` (`gosplan.budget/v1`) shows pools with level, source, documented flag, age, staleness, rate, projection and headroom, plus reservations, breakers, estimates and fits; text marks `(undocumented)`. | design notes |
| `budget#BG-EXPLAIN` | G | `gosplan budget explain TASK --json` (`gosplan.budget-explain/v1`) gives admit, rule, admit_at, the pool numbers and the last recorded decision. | design notes |
| `budget#BG-CALIBRATE` | G | `gosplan budget calibrate --json` (`gosplan.budget-calibrate/v1`) gives fits, estimate quantiles with observed and expected overrun rate, refusals by rule, projection error and alias evidence. | design notes |
| `budget#BG-REPORT` | G | `report.json` / `gosplan status --json` carry an additive `budget` block (mode, pools, reservations, breakers, estimates, decisions by rule, halt, notes); `gosplan status` text prints a `budget:` line. | design notes |
| `budget#BG-CLAIMS` | G | Help text and report notes say estimates are estimates, window numbers are undocumented, `--max-budget-usd` does not cap plan usage, and defaults are policy. | design notes |
| `budget#BG-REPLAY` | G | `gosplan replay --check` recomputes every budget decision from its recorded inputs (`ok: … budget decisions recomputed`); a tampered decision exits 1 with `budget drift`. | design notes |
## The suite's own rules

| clause | kind | statement | source |
|---|---|---|---|
| `suite#CONF-META` | G | Every test names an existing clause; RED.md rows name real tests and a clause the test names; red tests name only R clauses, green tests only G clauses; every clause has a test; every file carries the PROTECTED header; no `t.Skip`. | design notes |