#!/bin/sh
# make-e2e-project.sh DIR: build the fixture project that cmd/gosplan/run_test.go
# builds for the e2e-spec fixture: a git repo on master with a committed
# nightrunner.toml whose test gate fails without fake-*.txt or with BAD in one.
set -eu
dir=${1:?usage: make-e2e-project.sh DIR}
mkdir -p "$dir"
cd "$dir"
g() { git -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false "$@"; }
g init -q -b master
printf '[verify]\ntest = "sh check.sh"\n' > nightrunner.toml
printf 'ls fake-*.txt >/dev/null 2>&1 && ! grep -q BAD fake-*.txt\n' > check.sh
printf 'fixture\n' > README
g add nightrunner.toml check.sh README
g commit -q -m base
g rev-parse HEAD
