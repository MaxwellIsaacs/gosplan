---
name: gosplan-spec
description: Decompose a specification document into a gosplan spec directory (`spec.md`, `prompts/<id>.md`, `prompts/<id>.task.toml`, `prompts/sequence.txt`) plus a `nightrunner.toml.example` for the project root, ready for `gosplan run`. Handles greenfield projects and existing codebases. Use when a spec has to become a set of tasks gosplan can schedule, run and verify. (Running and watching the result is gosplan-drive's job.)
argument-hint: [path/to/spec.md]
---

# Gosplan spec: decompose a spec into verifiable tasks

You turn one specification document into a spec directory that `gosplan run`
imports. Each task is one `claude -p` worker in its own git worktree; the only
thing that decides whether its work counts is `gosplan verify`, run on the
task's commits in a clean export. Write every task so that an honest worker
can pass that verifier and a dishonest one cannot.

## Input

Read the spec file at: $ARGUMENTS

If no path is given, ask for one. Also ask for the project directory the
workers will change (the `--project` of `gosplan run`) if you do not know it.

## What gosplan does with your output

Know this before you write anything; every rule below follows from it.

- **The worker sees only its prompt.** The prompt is gosplan's fixed preamble
  (stay in scope, stage files by name, never edit existing tests, commit
  before finishing, `GOSPLAN_BLOCKED: <reason>` if the task cannot be done
  honestly) followed by `prompts/<id>.md`, verbatim. `spec.md` is **not**
  copied into the worktree, there is no `CLAUDE.md` template and no handoff
  files. If a fact matters to the task, it is in the prompt or in the code.
- **The worktree** holds the project at the run base with the verified heads
  of every dependency merged in. Nothing from tasks that are not
  dependencies, and nothing that failed.
- **Only commits count.** The head is resolved when the worker exits.
  Uncommitted work is lost.
- **Retries start fresh.** Attempt 2 starts from a clean reset of the task
  base, on `--retry-model` (opus by default), with a short summary of the
  previous failure in its prompt. It does not see attempt 1's code.
- **The verifier** runs these gates on `base..head`; the commands run in a
  clean export with no `.git` and a fresh `HOME`:
  1. `object_integrity`, `committed_work`: the head is a real commit that
     descends from the base and changes something.
  2. `locked_paths`: no change at all to `nightrunner.toml`,
     `.gitattributes`, `.mcp.json`, `.claude/**`, `CLAUDE.md`, plus
     `[verify.locked] paths`.
  3. `protected_paths`: no modify, delete or type change of
     `**/*_test.go`, `tests/**`, `**/testdata/**`, plus
     `[verify.protected] paths`. **Adding** a file there fails too, unless
     the sidecar sets `allow_new_protected = true`.
  4. `scope`: every changed path must match the sidecar's `scope` globs
     (no `scope` = unrestricted).
  5. `commands_present`, `clean_checkout`, then `cmd:test`, `cmd:lint`,
     `cmd:build`, `cmd:custom[i]` from `[verify]`, each under its timeout.

  `DONE.md`, test reports, a claim in the final message: none of it counts.
- **The config is read from the base commit**, never from the worker's tree.
  `nightrunner.toml` must be committed in the project before the run, and
  the base (`--base`, default `HEAD`) must contain it.
- **Scheduling is by dependency, not by tier.** A task runs when its deps
  have passed. A failed or blocked task abandons only its descendants;
  independent branches of the graph keep running. At the end, the passed
  heads are merged in (tier, id) order into `gosplan/<run>/integration`; a
  conflict there blocks the task and halts the run.

## Process

### 1. Name the operation and pick the output path

Pick a real Allied military operation name, `OPERATION_<NAME>` (uppercase,
underscores), loosely matching the project if you can.

Write to `~/specs/OPERATION_<NAME>/` unless the user names another place.
`mkdir -p ~/specs`, list it, and pick another name if that one is taken.
Never overwrite an existing operation.

### 2. Read the spec

Find the functional areas, the dependencies between them, what can be built
in parallel, and where the natural phase boundaries are.

### 3. Greenfield or existing codebase

Inspect the project directory, or ask. It changes how you decompose:

**Greenfield.** Phase 1 creates the repo layout, the shared types and every
file later tasks plug into, and it locks the test, lint and build commands.
The project still needs a first commit holding `nightrunner.toml` before the
run (the verifier reads it from the base), so tell the operator to commit
the toml and use that commit as the base.

**Existing codebase.** Do a repo-exploration pass first:

1. Read the real tree. Note the actual files and public symbols each task
   depends on or changes, and ground every file list, scope glob and
   acceptance criterion in paths that exist. No invented module names.
2. Read the real verification commands from the project (`go.mod` →
   `go test ./...`, `package.json` scripts, `pyproject.toml`, the Makefile,
   a flake's checks). Do not invent them.
3. The aggregator files (the router, the central app struct, the one big
   UI file) already exist and were not designed for parallel extension.
   §4.7 is sharper here: you usually cannot shape the aggregator up front,
   so serialize or split into leaf files.
4. Inline in each prompt the existing signatures it must call or keep.
   Workers have no repo map, so tell them where to look.

### 4. Design the graph

Tasks are `N.letter` (`1.a`, `2.b`): N is the tier, the letter is the
position in it. `prompts/sequence.txt` holds the tiers:

```
# comments and blank lines are allowed
1.a -> (2.a || 2.b || 2.c) -> (3.a || 3.b) -> 4.a
```

`->` separates tiers, `||` lists tasks in one tier, parentheses group them.

Edges come from the sidecar, not from the tiers. Give **every** task a
`deps` key listing exactly what it needs:

- With `deps`, a task depends on those tasks and nothing else. `deps = []`
  means ready at once.
- Without `deps`, it falls back to depending on every task of the previous
  tier. That re-creates the tier barrier you are trying to avoid; use it
  only on purpose.
- A dep must exist and sit in a lower tier, so the tiers are a topological
  order and cycles are impossible.

Guidelines:

- Phase 1 is foundation: layout, shared types, core abstractions, the
  aggregator files, the verification commands.
- Middle phases are feature work; fan out where file sets are disjoint.
- The last phase is integration and end-to-end tests.
- Draw real edges. If 3.b needs only 2.a, say `deps = ["2.a"]`; then a
  failure in 2.c does not abandon 3.b.
- Every task must be completable without seeing work from tasks it does not
  depend on.

### 4.5. Audit the graph before writing prompts

1. **Parallelism asymmetry.** A tier fanning out to 3+ tasks next to
   single-task tiers: are the single tasks really sequential? Split them, or
   write down why not in the README Notes.
2. **Hidden ambiguities.** External data shapes, library choices, interface
   decisions the spec leaves open. Resolve them in the prompts or list them
   in README Notes for the operator before the run. Never push "check the
   live API" onto an implementation worker.
3. **Cross-cutting invariants.** Properties that span tasks (schema
   authority, data lifecycle, idempotency). They go in the README's System
   invariants and are restated in every prompt they constrain.
4. **Soft dependencies.** A task that uses another's output without being
   blocked by it. Make it a real dep, a fallback, or a later tier. Never
   implicit.
5. **Open foundation choices.** Close every "X or Y" in Phase 1, including
   the shape of each file later tasks plug into (subcommand registry, router
   includes, handler tables). Open choices become merge conflicts.
6. **Verification commands.** Decide `test`, `lint`, `build` now. They run
   in a clean export with no `.git` and a fresh `HOME`: no caches, no git
   commands, nothing from `~`. Budget minutes for cold caches (a Go or Nix
   cold start can take one to two minutes). If Phase 1 picks the framework,
   the command can be guarded (`if [ -e go.mod ]; then ...; fi`) so 1.a can
   pass before it exists, but `nightrunner.toml` itself is locked: no task
   can edit it, so the final command must be in it from the start.
7. **File-write isolation.** For every pair of tasks that do not depend on
   each other (not just tasks in one tier: with real `deps`, 2.b and 3.a can
   be concurrent), list the files each writes. Any path in two lists
   conflicts at integration and halts the run. Fix it by one of:

   a. **Foundation owns the shared file.** Phase 1 writes it in full,
      including stubs for each later contribution; later tasks fill in
      per-task files only.
   b. **Per-task files.** Phase 1 writes an aggregator that imports one file
      per task; each task owns its file and never touches the aggregator.
      Preferred whenever each task adds a registered behavior.
   c. **Serialize.** Add a dep between them. Fine for two tasks when (a) and
      (b) are awkward.

   Then **encode the result as `scope`**. The verifier enforces scope; a
   sentence in the prompt does not. Every task that can run concurrently
   with another gets a `scope` covering exactly its files.
8. **Tests and the protected gate.** Every task that adds tests needs
   `allow_new_protected = true`, or its new `*_test.go` / `tests/**` /
   `testdata/**` files fail the protected gate. No task can modify an
   existing test file. If the spec needs an existing test changed, that is a
   human commit before the run, not a task.

#### Worked example: dispatcher-style CLI

A CLI with subcommands `scrape`, `normalize`, `serve`, one per parallel task
in tier 2.

**Bad.** 1.a writes `cli.py` with stubbed subparsers; 2.a, 2.b, 2.c each edit
`cli.py`. Integration merges three edits to the same lines; the run halts
`integration_conflict`.

**Good (b).** 1.a writes `cli.py` as a thin dispatcher that calls
`register(sub)` and `run(args)` from `commands/scrape.py`, `normalize.py`,
`serve.py`, each a stub that exits 2. Then:

```toml
# prompts/2.a.task.toml
deps = ["1.a"]
scope = ["src/pkg/commands/scrape.py", "tests/test_scrape.py"]
allow_new_protected = true
```

and likewise for 2.b and 2.c. A worker that edits `cli.py` fails `scope`
and is retried, instead of poisoning integration.

### 5. Write the prompts

One file per task, `prompts/N.L.md`. Each prompt:

- Starts with the full graph as a comment with this task marked, so the
  worker knows where it sits.
- Says what the worktree holds: "Your worktree is the project at the run
  base plus the merged work of <deps>. Read the code for signatures; there
  are no handoff files and no copy of the spec."
- Inlines everything load-bearing: the relevant part of the spec, the types
  and signatures from earlier tasks, the invariants that constrain it. Do
  not say "see the spec" or "see 1.a".
- States what to produce: files, functions, behavior.
- States its file ownership in words that match its `scope`: "You may
  change only <paths>. Do not edit <aggregator>."
- Gives acceptance criteria the verifier checks, each a test or command:
  "`go test ./internal/scrape/...` passes and `TestScrapeEmptyPage` covers
  the empty-page case", not "the code is clean".
- Makes every choice in foundation prompts. No "use X or Y".
- Does not repeat the preamble's rules (commit, stage by name, don't touch
  tests, `GOSPLAN_BLOCKED`). Gosplan prepends them.

End each prompt with:

```markdown
## When you're done

1. Run `<test>`, `<lint>`, `<build>` from the repo root and fix what fails.
   The verifier runs the same commands in a clean checkout of your commits.
2. Commit every file you changed, staged by name.
3. If you could not do the task honestly, end with
   `GOSPLAN_BLOCKED: <reason>` instead of claiming it is done.
```

Size prompts by scope: one module or one coherent set of acceptance
criteria. Most land at 50–300 lines of markdown; much past that, split.

### 6. Write the sidecars

One `prompts/N.L.task.toml` per task. Keys (unknown keys are a warning):

| key | meaning |
|---|---|
| `deps = ["1.a"]` | the task's dependencies; `[]` = none. Write it on every task |
| `scope = ["src/x/**"]` | globs the task may change; omitted = unrestricted |
| `allow_new_protected = true` | the task may **add** test/testdata files |
| `class = "normal"` | budget class for admission (default `normal`) |
| `prefer = ["sonnet"]` | budget routing: models in preference order |
| `max_turns = 300` | overrides the run's `--max-turns` for this task |

Leave `scope` off only for an integration task that may need to touch
anything, and say so in a comment.

### 7. Write `prompts/sequence.txt`

The graph from §4, one line, optionally with `#` comments above it.

### 8. Copy the spec

Copy the source spec to `<op_dir>/spec.md`, a real copy, not a symlink. It is
for the operator and for you. Workers never see it.

### 9. Write `nightrunner.toml.example`

```toml
# Copy to <project>/nightrunner.toml and COMMIT it before the run:
# gosplan verify reads it from the base commit, never from a worker's tree,
# and no task may change it (it is a locked path).
#
# Every command runs in a clean export of the task's commits: no .git,
# a fresh HOME, cold caches.

[verify]
test = "<e.g. go test -count=1 ./...>"
lint = "<e.g. sh -c '! gofmt -l . | grep .' && go vet ./...>"
build = "<e.g. CGO_ENABLED=0 go build ./...>"
# custom = ["<extra gate>", ...]

[verify.timeouts]
test_seconds = 900
lint_seconds = 300
build_seconds = 300
# custom_seconds = 60

# [verify.locked]      # added to the default locks; no task may change these
# paths = ["internal/core/**"]

# [verify.protected]   # added to the default protected test/testdata globs
# paths = ["fixtures/**"]
```

Use the commands from §4.5.6. If one cannot be known yet, guard it so it
is a no-op until Phase 1 creates what it needs, and say so in README Notes.

### 10. Write the README

Operator-facing; gosplan does not read it:

1. Operation name as the title.
2. Source spec path.
3. Overview, two or three sentences.
4. Graph, plus a table of each task's `deps` and `scope`.
5. Phase breakdown: one line per task.
6. How to run:
   ```
   cp nightrunner.toml.example <project>/nightrunner.toml   # then commit it
   gosplan run --spec <op_dir> --project <project> --dry-run
   gosplan run --spec <op_dir> --project <project> [--base <ref>]
   ```
7. Notes: assumptions, open questions from §4.5, anything the operator must
   do or decide first.
8. System invariants: the cross-task rules from §4.5.3.

## Output

```
~/specs/OPERATION_<NAME>/
├── README.md
├── spec.md
├── nightrunner.toml.example
└── prompts/
    ├── sequence.txt
    ├── 1.a.md
    ├── 1.a.task.toml
    ├── 2.a.md
    ├── 2.a.task.toml
    └── ...
```

## Before you hand it over

- Run `gosplan run --spec <op_dir> --project <project> --dry-run` if gosplan
  is installed. It validates the spec and prints each task's deps and the
  waves in which tasks become ready. Fix any warning about unknown keys.
- Check that every task that adds tests has `allow_new_protected = true`.
- Check that every pair of concurrent tasks has disjoint `scope`.
- Check that no prompt says "see the spec", "see 1.a's handoff" or
  "write DONE.md".
