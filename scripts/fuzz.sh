#!/usr/bin/env bash
# Runs every native fuzz target for FUZZ_TIME (default 60s) each. `go test -fuzz` accepts one
# target in one package at a time, so the targets are enumerated first.
set -euo pipefail

fuzz_time="${FUZZ_TIME:-60s}"
found=0
packages="$(go list ./...)"
while read -r pkg; do
  [ -n "$pkg" ] || continue
  listed="$(go test -list '^Fuzz' "$pkg")"
  while read -r target; do
    [[ "$target" =~ ^Fuzz[A-Za-z0-9_]+$ ]] || continue
    found=1
    echo "fuzzing $pkg $target for $fuzz_time"
    go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzz_time" "$pkg"
  done <<< "$listed"
done <<< "$packages"
[ "$found" = 1 ] || { echo "fuzz: no targets found" >&2; exit 1; }
