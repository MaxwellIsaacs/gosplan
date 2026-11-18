---
name: gosplan-drive
description: Operate gosplan end to end: preflight and launch `gosplan run` on a spec, watch it, read why a task failed, resume or re-run, review and merge the integration branch, clean up, and read the budget/usage surfaces. Use when asked to run, watch, debug, rerun, ship or cost a gosplan run. (Writing the spec is gosplan-spec's job.)
---

# Drive gosplan

If `~/.config/gosplan/operator.md` exists, read it first. It holds this
machine's specifics (which host runs gosplan, the binary path, resource
policy, notifications) and overrides the defaults below.

Deeper references, in the gosplan repo: `docs/gosplan-run.md` (flags, halt
reasons, state layout, resume rules), `docs/gosplan-budget.md` (budget gate),
`docs/trust-until-d7.md` (what a worker can and cannot get away with). Read
them before guessing.

## Build

```
CGO_ENABLED=0 go build -o gosplan ./cmd/gosplan     # in the gosplan repo
gosplan version
```

## Preflight

All of these, every time:

1. **Spec validates.**
   `gosplan run --spec <op> --project <proj> --dry-run`
   prints every task with its deps and the waves it would run in. Exit 0
   and no `unknown key` warnings.
2. **`nightrunner.toml` is committed at the base.** The verifier reads it
   from the base commit (`--base`, default `HEAD`), never from the working
   tree. `git -C <proj> show <base>:nightrunner.toml` must print it. A
   missing config is a could-not-verify on every task.
3. **The base is what you mean.** `--base` is resolved once at start. Use a
   named ref (`base/<op>`) rather than whatever `HEAD` is; you will want it
   again for a re-run.
4. **The verify commands pass at the base**, run by hand the way the
   verifier will (clean checkout, no `.git`, fresh `HOME`). A gate that
   fails at the base fails every task.
5. **Headroom.** Each task gets a full worktree plus cold build caches, and
   `budget.jsonl`/`events.jsonl` keep growing. Check free disk and RAM for
   `--max-parallel` workers plus their verifies. Running out of disk mid-run
   produces torn logs and a halt.
6. **Usage window.** `gosplan budget --project <proj>` (or `gosplan status`)
   shows the observed five-hour and weekly windows. Launching into a nearly
   full window means workers stalling on rate limits.

## Launch

Detach it. A run takes hours.

```
tmux new-session -d -s gosplan-<op> "cd <proj> && \
  gosplan run --spec <op> --project <proj> --base <ref> \
    --max-parallel 3 --model sonnet --retry-model opus 2>&1 | tee -a ~/<op>.log; \
  echo GOSPLAN_EXIT=\${PIPESTATUS[0]} | tee -a ~/<op>.log; exec bash"
```

Use `${PIPESTATUS[0]}`, not `$?`: after `| tee`, `$?` is tee's exit and
reads 0 even when the run halted.

The first lines print the run id and the state dir
(`$XDG_STATE_HOME/gosplan/<project>-<hash>/`). Note both.

Useful flags: `--deadline HH:MM` (no new attempt after it; live ones
stop), `--cap-window five_hour=0.8`, `--max-attempts`, `--stall-timeout`,
`--max-turns`. Exit codes: 0 complete, 1 halted, 2 setup error or aborted.

## Watch

```
gosplan status --project <proj> [--run ID]       # tasks, attempts, verdicts, windows
gosplan status --project <proj> --json | jq ...
tail -f <state>/runs/<run>/events.jsonl
grep -E 'halted|complete|GOSPLAN_EXIT' ~/<op>.log
```

Task states: `pending` → `running` → `verifying` → `passed` | `failed` |
`blocked` | `abandoned` (a dependency did not pass). A task with
`not_before` in the future is in retry backoff or parked by the budget gate;
`gosplan budget explain <task> --project <proj>` says which.

## When a task fails

Read the evidence, not the worker's account of it:

1. **The verdict:** `<state>/runs/<run>/verify/<task>.<n>.<seq>/verdict.json`.
   `jq -c '.failed_gates, (.gates[] | {name, status})' verdict.json`. Gates
   run in order: `object_integrity`, `committed_work`, `locked_paths`,
   `protected_paths`, `scope`, `commands_present`, `clean_checkout`,
   `cmd:test`, `cmd:lint`, `cmd:build`, `cmd:custom[i]`. The common ones:
   - `scope`: the worker wrote outside its `scope`. The prompt's file list
     and the sidecar disagree, or the task really needs more files.
   - `protected_paths` with `A <path>`: the task added a test but its sidecar
     lacks `allow_new_protected = true`. With `M`/`D`: it edited an existing
     test, which no task may do.
   - `locked_paths`: it touched `nightrunner.toml`, `CLAUDE.md`, `.claude/**`
     or a project lock.
   - `committed_work`: the worker exited without committing.
   - `cmd:*`: real failure; `verdict.stderr` and the `verify-logs/` dir
     have the output.

   No `verify/<task>.<n>.*` dir at all means the attempt never reached the
   verifier (non-zero exit, stall): go to step 2.
2. **What the worker did:** `attempts/<task>.<n>.stream.jsonl` (the full
   stream-json) and `.stderr`. The final `result` line has its last message,
   and any `GOSPLAN_BLOCKED:` reason.
3. **Stalls and infrastructure:** an attempt that ended `stalled` was silent
   for `--stall-timeout`; `start_failed`/`lost` are infrastructure and do not
   use up attempts.

The halt reason in the log or report names what stopped the run
(`docs/gosplan-run.md`, Halt reasons).

## Resume or re-run

- **Resume** (crash, Ctrl-C, reboot, deadline or cap park):
  `gosplan run --spec <op> --project <proj> --run <ID>`. There is no
  `--resume` flag. Resume refuses if the spec changed since import ("spec
  changed since import", exit 2). It re-verifies every passed task by SHA
  before continuing. A run that already ended `complete` or `halted` is only
  reported, not continued.
- **Re-run after fixing the spec.** If you edit a prompt or sidecar, the run
  cannot be resumed. Start a new run that keeps what passed:
  1. Point a ref at the old run's integration tip:
     `git -C <proj> branch base/<op>-r2 gosplan/<old-run>/integration`.
  2. Copy the op to `<op>_R2`, drop the passed tasks from `sequence.txt`
     and from every `deps`, renumber the tiers if the first is now empty, fix
     the failing prompts.
  3. `--dry-run`, then launch with `--base base/<op>-r2`.

## Ship

Gosplan never touches `main`, `master` or the checked-out branch. Passed
work is on `gosplan/<run>/integration`, merged in (tier, id) order.

```
git -C <proj> log --stat <base>..gosplan/<run>/integration
git -C <proj> diff <base>..gosplan/<run>/integration
```

Review it as a human, run the project's tests on it, then merge it yourself.
A halted run's integration branch still holds every task that passed and
merged; it is shippable on its own if what is there is coherent.

## Clean up

Worktrees live under the state dir but are registered in the project repo.
After shipping:

```
git -C <proj> worktree list | grep <state>/worktrees/<run>
git -C <proj> worktree remove --force <each path>; git -C <proj> worktree prune
git -C <proj> for-each-ref --format='%(refname:short)' 'refs/heads/gosplan/<run>/' | xargs -r git -C <proj> branch -D
```

Keep `<state>/runs/<run>/` (events, verdicts, budget log) for calibration and
audit; it is small next to the worktrees.

## Cost and budget

- `gosplan status` and `report.json` carry per-attempt tokens and the
  client-side `cost_usd_estimate` (list price, not plan usage).
- `gosplan budget --project <proj>` shows pools, headroom at the horizon,
  reservations and breakers. `gosplan budget calibrate` checks the
  estimates against past runs.
- The gate is **observe-only** until a `<state>/budget.toml` with
  `budget.mode = "enforce"` (and an explicit `budget.cold.unit_fraction`)
  exists. Move to enforce only after a week of observe runs pass
  `calibrate` (`docs/gosplan-budget.md`).
- Window figures come from undocumented `rate_limit_event` and `/usage`
  text. Treat them as indications.

## Gotchas

- **A usage limit parks the run; it does not fail it.** When the five-hour
  or weekly limit refuses a worker (`"api_error": "usage_limit_reached"` in
  the final `result` line, exit 1 within a second), gosplan refunds the
  attempt and holds every pending task until the reset plus 2 minutes. The
  task's transition reason reads `usage limit: waiting for reset at <time>`.
  The run waits however long that is: a weekly limit can park it for days,
  so set `--deadline` on overnight runs. Builds before this fix spent all
  attempts in seconds instead (every attempt `exit_code 1`, `num_turns 1`,
  ~1s); re-run those tasks after the reset.
- Workers run as your uid, unsandboxed. Do not run a spec you would not
  run `claude --permission-mode bypassPermissions` on.
- One `gosplan run` per state dir. A second one fails "database is locked".
- `--usage-probe` runs `claude -p /usage` when windows are stale; it is free
  but needs a working `claude` login.
- A spec written for the old bash harness (DONE.md schema, TESTS.md,
  "read the handoff") passes `--dry-run` but its workers chase files that
  do not exist. Rewrite it with gosplan-spec.
