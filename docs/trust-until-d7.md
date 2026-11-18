# Trust model until D7: workers share the orchestrator's uid

D7 is the roadmap milestone that sandboxes workers (see the README's
Roadmap, item 3). Until then this document is the trust model.

Until D7 gives workers a separate uid (and a cgroup), every `claude` worker
runs as the same OS user as `gosplan run`. A worker that wants to can read and
write anything that user can reach: `gosplan.db`, `events.jsonl`, the state
directory, the project repo's `.git` (objects, refs, config), the attest key
file, other workers' worktrees. Nothing in this document makes the
orchestrator's state protected from the worker. What `gosplan run` does instead
is defence in depth plus detection: it keeps the facts it decides on in its
own memory, re-derives what it can from content-addressed git objects, and
halts loudly when what is on disk disagrees.

Where things live:

- `<state>/gosplan.db`, `<state>/runs/<run>/` and the task worktrees
  `<state>/worktrees/<run>/<task>` are all under one state directory. A
  worker's cwd is its worktree, so `gosplan.db` is three directories up.
- The state dir is created `0700` and `gosplan.db*`, `events.jsonl`, `state.json`
  and `report.json` `0600`. That is hygiene against other local users only;
  it is not a boundary against a worker, which runs as the owner.
- Task branches `gosplan/<run>/<task>` and the integration branch
  `gosplan/<run>/integration` are refs in the project repo. A worktree made with
  `git worktree add` shares the project's object store, refs and config, so
  a worker can move any ref, including the integration branch and other
  tasks' branches.
- The worker environment carries `GOSPLAN_RUN`, `GOSPLAN_TASK` and `GOSPLAN_ATTEMPT`
  (conformance `TestRunWorkerEnv` requires all three). No state path is
  exported; the worker can find the state dir from its cwd anyway.

## Defended: a tampered result cannot pass without a halt

- **Forged task status, base, head, scope, attempt counts in `gosplan.db`
  while `gosplan run` is up.** Dispatch, verify inputs, integration and
  completion read the orchestrator's in-memory copy of the run, not the DB. Every transaction
  first reads every task row and the run row and compares them with memory;
  any difference rolls back and halts the run `halted`, reason
  `state_tampered: <table> row <id>: <field> db=… mem=…`, exit 1. The
  run key re-checks every 2s even when nothing is being written. (The
  reviewer's `dbother`, `dbbase2` and `dbpass` cases.)
- **Verify base.** A task's base is recomputed, never read back: the run
  base with the dependencies' verified heads merged in order, by
  `wt.ComputeBase` (plumbing only: `merge-base`, `merge-tree`,
  `commit-tree`, fixed identity and dates), so the same SHAs give the same
  base every time. The recomputed base is what `gosplan verify --base` gets; it
  is written to the DB for the record.
- **Verify head.** The head is resolved once, through gitx, when the worker
  exits, and held in memory. Verify and integration use that SHA. Moving the
  task branch afterwards changes nothing the orchestrator uses.
- **Integration.** A task is merged only if this process's in-memory
  verdict for its attempt is a pass for exactly its recomputed base and its
  resolved head, and the verdict JSON names that same base and head. Heads
  are merged by SHA (no ref name is resolved), and afterwards the tip is
  recomputed from the same SHAs and the branch re-resolved; any difference
  halts `state_inconsistent`.
- **Resume (`gosplan run --run ID`) treats the DB as untrusted.** Before
  anything runs: the DB's task set and every static task column (tier,
  `prompt_path`, `deps_source`, deps, `scope_json` with `[]` = null,
  `allow_new_protected`) must equal what importing the spec as it is on
  disk now would write; `specs.content_sha256` must equal the spec dir's
  hash; and `runs.base_sha` and that hash must equal what the log's
  `run_started` event recorded. So a scope widened, a dep edge deleted or
  a prompt redirected in `gosplan.db` while `gosplan run` is down halts
  `state_tampered` naming the task and field (`TestRunE2EResumeForgedScope`,
  `TestReconcileResumeChecksSpecColumns`). This does not make the
  definition trustworthy: the spec dir and `events.jsonl`
  are as writable by a worker as `gosplan.db`. It moves the forgery from one
  file to three that must agree (spec dir, `gosplan.db`, the log's
  `run_started`).

  Then the event log must replay to the DB's task statuses, the
  DB's integration tip must equal the one the log's `run_finished` records
  (and the branch), and each task's `base_sha` must equal the base its last
  `launch` event names. Then every task recorded `passed` is verified again
  by SHA against its recomputed base. `gosplan run` never passes `--cache` to
  `gosplan verify`, so no cached verdict is consulted, on resume or otherwise.
  Any failure halts `state_tampered` or `state_inconsistent`.
- **End of run.** `gosplan run` runs the `gosplan replay --check` logic in-process:
  the DB must still equal memory, `events.jsonl` must replay to memory's
  statuses, and the branch and the log must name the tip this process
  produced. A mismatch halts `state_inconsistent`, exit 1.

- **Restart kills only its own workers.** Leases carry the leader's
  `/proc` start time and a per-attempt nonce (`GOSPLAN_ATTEMPT_NONCE`, also in
  the worker's environment). A restarted orchestrator kills a recorded
  group only when one of them matches, so a reused pid or another user's
  process is never killed and never wedges the run. The lease's pgid is
  what gets signalled, so it must be the leader's own: a group is killed
  only when the lease's pgid equals its pid (the launcher starts every
  worker with `Setpgid`, and a launch whose pgid is not its pid records no
  group and is stopped) and, while the leader lives, its `/proc` pgrp is
  that group (`TestKillVerifiedRequiresLeaderGroup`). If the leader has
  already exited, the group is still killed when a live member of it
  carries the run (an attempt nonce of the run, or `GOSPLAN_RUN=<run>`), so a
  same-group child that dropped only `GOSPLAN_ATTEMPT_NONCE` dies on resume
  (`TestRunE2EResumeKillsGroupOfDeadLeader`). These group kills run before
  the nonce sweep, and both run before the resume checks, so a resume that
  halts leaves nothing of the crashed run running. A nonce holder that
  leads its own process group is killed with its group, so a crash before
  the pid reached the lease does not leave a nonce-less child behind
  (`TestReconcileSweepKillsUnrecordedWorkerGroup`). A crashed run's `gosplan verify` carries a verify nonce of
  the run and is stopped by the same sweep (SIGTERM, so it cancels its gate
  commands, then SIGKILL) before any verify of the resume starts; each verify writes to its own `verify/<task>.<n>.<seq>/`, so
  a straggler cannot turn a resumed verify into could-not-verify
  (`TestRunE2EResumeKillsStragglerVerify`).

## Detected: halts loudly

- Any write to `tasks` or `runs` between two orchestrator steps
  (`state_tampered`).
- A forged pass that survives into a resume (the re-verify fails).
- An integration branch moved after integration, or a log/DB tip mismatch
  (`state_inconsistent` at the end of the run, `state_tampered` on resume).
- A worker holding the SQLite lock: past one minute of `SQLITE_BUSY` the run
  halts `state_locked` instead of retrying forever. Heartbeat and window
  writes happen off the stdout read path, so a locked DB never stalls
  reading a worker (which would have looked like a stall).
- A write that keeps failing: the worker edits a row a step must update
  (say, sets `attempts.ended_at` so its exit can never be recorded), or
  the run dir turns read-only. After 10 consecutive non-busy failures of
  one step, or 60s, the run halts `state_write_failed: <step>: <error>`
  instead of retrying all night (`TestRunE2EUnrecordableExitHalts`,
  `TestReconcileReadOnlyStateDirHalts`).
- A run that can no longer progress (a task ready but never dispatched,
  running with no worker or with an unrecorded exit, or verifying with no
  verify or with an unrecorded verdict, for two minutes) halts
  `no_progress`, naming the tasks.
- A change to this process's own budget rows halts `state_tampered: budget: …`,
  and a mid-run edit of `budget.jsonl` halts `state_inconsistent` at the
  end-of-run self-check.

## Still open until D7

- **Ended runs are not re-checked.** `gosplan run --run ID` on a `complete` or
  `halted` run only reports what `gosplan.db` records and writes nothing; a
  forged status or tip on an ended run is not detected by it (nothing is
  integrated or decided either).
- **Consistent forgery of DB and log together while `gosplan run` is down.** The
  resume checks compare the DB with `events.jsonl`; a worker (or a process it
  left behind) that edits both the same way, and keeps every `passed` task's
  recorded head pointing at a commit that really does pass `gosplan verify`, is
  not detected. The re-verify still means only content that passes the
  verifier can be integrated.
- **Forging the spec together with the DB and the log.** A worker that
  edits a sidecar in the spec dir (say, widens `scope`), the matching
  `tasks` row and specs hash in `gosplan.db`, and the `spec_sha256` in the
  log's `run_started` line, all while `gosplan run` is down, passes the resume
  checks: the widened scope is then what `gosplan verify` checks against. The
  spec dir is not under the orchestrator's control until D7.
- **Denial of service.** A worker can halt the run at will (any DB write
  does it), hold the DB lock (halts after a minute), fill the disk, or kill
  the orchestrator. It can reset its own or another task's attempt counters
  while `gosplan run` is down, buying extra retries (quota). The same goes for
  the per-task bound on refunded attempts: the count of `lost` and
  `start_failed` attempts is read from the `attempts` table, which the
  in-memory tamper check does not cover. While `gosplan run` is up, the count
  this process recorded itself is a floor, so deleting rows does not lower
  it; while `gosplan run` is down, deleting or rewriting those rows resets the
  bound. Without that, the bound stops a worker that kills `gosplan run` from
  being relaunched at full quota on every resume
  (`TestRunE2ECrashLoopIsBounded`).
- **The project repo's shared object store, refs and config.** Objects are
  content-addressed, so a worker cannot change what a SHA names. But it can
  write `.git/config` (gitx disables only system and global config, hooks,
  fsmonitor, attributes files and replace objects), add
  `objects/info/alternates`, or graft history with `info/grafts`, which can
  change merge bases and `rev-list` counts that `gosplan verify` and
  `ComputeBase` see. It can move every ref, including `main`, which
  `gosplan run` never writes but does not guard either.
- **Forging lease identity.** The lease row is in `gosplan.db`; a worker can
  point it at a process of its own choosing that carries a copied nonce, or
  clear `GOSPLAN_ATTEMPT_NONCE` from a child's environment so the restart sweep
  misses it. A child that also clears `GOSPLAN_RUN` (or calls `setsid()`) after
  its leader exits is not recognised and survives the resume. The worst
  case is a wrong same-uid kill or a surviving orphan.
- **Escaping the process group.** A child that calls `setsid()` leaves the
  worker's process group and is not killed with it; it can keep running,
  editing worktrees, refs and the DB, after the attempt ends and during
  verify. What is defended: it can no longer wedge the run. The driver waits
  on the leader's exit, kills the group, reads stdout for at most 2s more
  and then closes the pipe, so a setsid child holding stdout does not block
  the attempt, and SIGINT still ends `gosplan run` within a bounded time. D7
  scopes each worker with a cgroup and kills the cgroup, not the group.
- **Git inside the worker's worktree.** Every git call `gosplan run` makes
  (worktrees, bases, integration, the base ref lookup, and the reset before
  each launch) goes through gitx: hooks (`core.hooksPath=/dev/null`),
  fsmonitor, system and global config, the global attributes file and
  replace objects are all off. The reset (`git reset --hard <base>`,
  `git clean -fdx`) still runs in a worktree the worker controls, against
  the repo-local `.git/config` the worker can write and the in-tree
  `.gitattributes`: a worker can define a `filter.<name>.smudge` command
  (or `core.worktree`, `include.path`, ...) that git then runs as this user
  during the reset, outside the worker's process group and lifetime. That
  gives it nothing it could not already do as the same uid, but it is a
  way to run code after the attempt ends.
- **The attest key file** is readable by the worker if it is anywhere the
  user can read. A signed verdict means only that the key holder made the
  statement.
- **Reading other tasks' worktrees and the spec.** Nothing stops it.
- **Forged budget rows from other runs or from before this run.**
  Observations and reservations this process did not write (other runs
  sharing the state dir, or rows left from before this run) are trusted as
  the DB says, so a forged row can over-commit or starve shared runs. A
  forged low observation cannot lower a pool below this process's own newest
  observation of it (the floor), but a forged high one can raise it; a
  deleted or forged reservation of another run is not detected.
- **Forged `attempts` history.** The estimates, the fitted ratio and the
  belts are computed from `attempts` rows (`usage_json`,
  `cost_usd_estimate`); a worker that edits them skews those numbers for
  every later run.
- **`budget.toml` in the state dir is worker-writable between runs.** A
  worker can change the mode, `cold.unit_fraction` or the classes for the
  next run. The file is read once per run and its hash is in every decision
  (`config_sha256`), so a change shows up as a new hash, but nothing stops
  it.
- **Budget state restored on resume comes from the DB.** Breakers, parks
  and reservations restored by `gosplan run --run ID`, and the attempt facts of
  attempts made before this process started, are read from `gosplan.db` as it
  is; the in-memory cross-checks only cover what this process wrote.
- **A consistent rewrite of `budget.jsonl`** (inputs and decisions edited
  together) passes `gosplan replay --check`: replay proves each decision follows
  from its recorded inputs, not that the inputs were true. The in-process
  self-check catches a mid-run edit only; an edit after the run ends is not
  detected.
