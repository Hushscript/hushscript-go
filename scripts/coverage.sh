#!/usr/bin/env bash
# Fails when statement coverage of the hand-written packages is below the floor.
# Usage: scripts/coverage.sh <coverprofile> <percent>
# The profile is filtered first: generated code, the generator command, the conformance runner and
# the smoke program are not part of the floor (spec 13.2).
set -euo pipefail

profile="${1:?coverprofile}"
floor="${2:?floor in percent}"

# With -coverpkg every test binary writes a line for every block, so a block appears once per binary:
# counts are summed per block before judging it.
awk -v floor="$floor" '
  NR == 1 { next }
  $1 ~ /\/generated\// || $1 ~ /\/scripts\// || $1 ~ /\/conformance\// || $1 ~ /\/smoke\// || $1 ~ /\/examples\// { next }
  {
    stmts[$1] = $2
    hits[$1] += $3
  }
  END {
    for (block in stmts) {
      total += stmts[block]
      if (hits[block] > 0) covered += stmts[block]
    }
    if (total == 0) { print "coverage: no statements in the profile" > "/dev/stderr"; exit 1 }
    pct = 100 * covered / total
    printf "coverage: %.1f%% of %d statements (floor %s%%)\n", pct, total, floor
    if (pct < floor) exit 1
  }
' "$profile"
