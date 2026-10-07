#!/usr/bin/env bash
# go-mutesting on the HTTP core; fails below the given score (percent). It rewrites sources in place,
# so it runs on a copy of the tree.
set -euo pipefail

floor="${1:?minimum score in percent}"
work="$(mktemp -d)"
cp -a . "$work/src"
cd "$work/src"
out="$work/mutation.txt"
go tool -modfile=tools/go.mod go-mutesting --exec-timeout=60 http.go retry.go pacing.go 2>&1 | tee "$out"
score="$(sed -n 's/^The mutation score is \([0-9.]*\).*/\1/p' "$out" | tail -n1)"
[ -n "$score" ] || { echo "mutation: no score in the output" >&2; exit 1; }
awk -v score="$score" -v floor="$floor" 'BEGIN { printf "mutation score %.1f%% (floor %s%%)\n", score * 100, floor; exit (score * 100 < floor) }'
