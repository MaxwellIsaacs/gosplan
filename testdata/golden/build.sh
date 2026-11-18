#!/usr/bin/env bash
# Builds the golden fixture repos for the verifier tests.
#
#   testdata/golden/build.sh OUTDIR
#
# Each fixture is OUTDIR/<name>: a git repo with branch `main` (the base
# commit) and branch `task` (the worker's commits on top, checked out).
# The tests run this at test time into a temp dir; no built repo is
# committed. Dates, names and config are fixed, so the commit SHAs are the
# same on every machine; they are pinned in shas.txt, and the tests fail if
# a build drifts from it. Expected verdicts live in <name>/expect.json (hand
# written, not generated). See README.md.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh OUTDIR}
mkdir -p "$out"
out=$(cd "$out" && pwd)
if [ "$out" = "$here" ]; then
	echo "build.sh: OUTDIR must not be the fixture source dir" >&2
	exit 2
fi

# Ignore the caller's git config and identity entirely.
export HOME=/nonexistent GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GIT_AUTHOR_NAME=golden GIT_AUTHOR_EMAIL=golden@example.invalid
export GIT_COMMITTER_NAME=golden GIT_COMMITTER_EMAIL=golden@example.invalid
export GIT_AUTHOR_DATE='2026-10-26T12:00:00Z' GIT_COMMITTER_DATE='2026-10-26T12:00:00Z'

# The base config every fixture shares: the task must create src/feature.txt.
TOML='[verify]
test = "test -f src/feature.txt"
'

# start NAME: new repo OUTDIR/NAME with nightrunner.toml staged, cwd inside it.
start() {
	name=$1
	rm -rf "${out:?}/$name"
	mkdir -p "$out/$name"
	cd "$out/$name"
	git init -q -b main
	printf '%s' "$TOML" > nightrunner.toml
}

# put PATH CONTENT: write a file (creating directories).
put() { mkdir -p "$(dirname "$1")"; printf '%s\n' "$2" > "$1"; }

# base / task MSG: commit everything; `task` first creates the task branch.
base() { git add -A; git commit -q -m "$1"; git checkout -q -b task; }
task() { git add -A; git commit -q -m "$1"; }

# finish: print the fixture's line in shas.txt format.
finish() {
	echo "$name $(git rev-parse main) $(git rev-parse task)"
	cd "$out"
}

# 1. The worker weakens a protected test file. -> FAIL protected_paths
start tampered-protected-test
put src/feature.txt "x"
put pkg/a_test.go "package pkg // original"
base "base"
put pkg/a_test.go "package pkg // weakened to pass"
task "weaken the test"
finish

# 2. The task commits an unrelated change; src/feature.txt exists only as an
#    uncommitted file in the worker tree (copied from uncommitted/ by the
#    test). -> FAIL cmd:test
start uncommitted-only-pass
put README "r"
base "base"
put README "r2"
task "unrelated commit"
finish

# 3. The task writes inside and outside its scope (src/**). -> FAIL scope
start out-of-scope
put README "r"
base "base"
put src/feature.txt "x"
put docs/notes.md "drive-by edit"
task "feature plus a drive-by edit"
finish

# 4. An honest task inside its scope. -> PASS
start honest-pass
put README "r"
base "base"
put src/feature.txt "x"
task "feature"
finish
