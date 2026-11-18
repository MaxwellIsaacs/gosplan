#!/usr/bin/env bash
# PROTECTED. A human reviews every change to this file by
# hand. Later operations must not edit it to make the suite pass.
#
# Runs the conformance suite and checks every result against RED.md.
#
#   conformance/run.sh [--repeat N]
#
# --repeat N (default 3) re-runs the green tests until each has run N times
# in total, to catch flakes. Prints the green and red counts.
# Exit: 0 all as expected; 3 only "newly green" red tests besides that;
# 1 anything unexpected (a green test failing, a red test failing with the
# wrong text, a panic, a timeout, a meta-check failure, a vet failure).
set -euo pipefail

repeat=3
while [ $# -gt 0 ]; do
	case $1 in
	--repeat) repeat=${2:?}; shift 2 ;;
	*) echo "usage: conformance/run.sh [--repeat N]" >&2; exit 1 ;;
	esac
done

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

# Toolchain: go, git and sqlite3 on PATH, else from nixpkgs (no network if cached).
if ! command -v go >/dev/null || ! command -v git >/dev/null; then
	if [ -z "${GOSPLAN_CONF_REEXEC:-}" ] && command -v nix >/dev/null; then
		export GOSPLAN_CONF_REEXEC=1
		exec nix shell nixpkgs#go nixpkgs#git nixpkgs#sqlite -c "$0" --repeat "$repeat"
	fi
	echo "run.sh: need go and git on PATH" >&2
	exit 1
fi
if [ -z "${GOSPLAN_CONF_SQLITE3:-}" ]; then
	if command -v sqlite3 >/dev/null; then
		GOSPLAN_CONF_SQLITE3=$(command -v sqlite3)
	else
		GOSPLAN_CONF_SQLITE3=$(ls -d /nix/store/*-sqlite-3*-bin/bin/sqlite3 2>/dev/null | sort -V | tail -1 || true)
	fi
fi
if [ -z "${GOSPLAN_CONF_SQLITE3:-}" ] || [ ! -x "$GOSPLAN_CONF_SQLITE3" ]; then
	echo "run.sh: need a sqlite3 CLI (set GOSPLAN_CONF_SQLITE3)" >&2
	exit 1
fi
export GOSPLAN_CONF_SQLITE3

unset GOSPLAN_CONF_META_PARTIAL
go vet ./conformance/...

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
go build -o "$tmp/redcheck" ./conformance/redcheck

echo "== pass 1: every test once"
go test -json -count=1 -timeout 30m ./conformance/ >"$tmp/pass1.json" 2>&1 || true
rc=0
"$tmp/redcheck" --red conformance/RED.md --green-out "$tmp/green.txt" <"$tmp/pass1.json" || rc=$?
if [ "$rc" = 1 ]; then
	exit 1
fi

if [ "$repeat" -gt 1 ] && [ -s "$tmp/green.txt" ]; then
	pattern="^($(paste -sd'|' "$tmp/green.txt"))\$"
	echo "== pass 2: green tests $((repeat - 1)) more time(s)"
	go test -json -count=$((repeat - 1)) -timeout 30m -run "$pattern" ./conformance/ >"$tmp/pass2.json" 2>&1 || true
	rc2=0
	"$tmp/redcheck" --red conformance/RED.md --green-only <"$tmp/pass2.json" || rc2=$?
	if [ "$rc2" != 0 ]; then
		exit 1
	fi
fi
exit "$rc"
