#!/usr/bin/env bash
# Runs every native fuzz target for FUZZ_TIME (default 60s) each. `go test -fuzz` accepts one
# target in one package at a time, so the targets are enumerated first.
set -euo pipefail

fuzz_time="${FUZZ_TIME:-60s}"
found=0
while read -r pkg; do
  [ -n "$pkg" ] || continue
  for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
    found=1
    echo "fuzzing $pkg $target for $fuzz_time"
    go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzz_time" "$pkg"
  done
done < <(go list ./...)
[ "$found" = 1 ] || echo "no fuzz targets yet"
