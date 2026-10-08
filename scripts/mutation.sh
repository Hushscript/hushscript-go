#!/usr/bin/env bash
# go-mutesting on the HTTP core and transcribe pipeline; fails below the given score (percent). It rewrites sources in place,
# so it runs on a copy of the tree.
set -euo pipefail

floor="${1:?minimum score in percent}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp -a . "$work/src"
cd "$work/src"
out="$work/mutation.txt"
# The pinned runner reports compile failures as PASS on current Go. Debug output
# includes the test result for each mutant, so the aggregate score is not used.
NO_COLOR=1 go tool -modfile=tools/go.mod go-mutesting --debug --exec-timeout=60 http.go retry.go pacing.go transcribe.go upload.go resume.go wait.go heartbeat.go 2>&1 | tee "$out"
awk -v floor="$floor" '
  /^Execute built-in exec command for mutation$/ {
    if (phase) invalid = 1
    phase = 1
    compile = failed = passed = timeout = 0
    next
  }
  phase == 1 {
    if ($0 ~ /\[(build|setup) failed\]/) compile = 1
    if ($0 ~ /^--- FAIL: /) failed = 1
    if ($0 ~ /^ok[[:space:]]+[^[:space:]]+[[:space:]]/) passed = 1
    if ($0 ~ /^panic: test timed out after /) timeout = 1
  }
  /^Exited with -?[0-9]+$/ {
    if (phase != 1) invalid = 1
    phase = 2
    code = $3
    next
  }
  /^(PASS|FAIL|SKIP|UNKOWN exit code for) "[^"]+" with checksum [[:xdigit:]]+$/ {
    if (phase != 2) invalid = 1
    if (($1 == "PASS" && code != 0) || ($1 == "FAIL" && code != 1) || ($1 == "SKIP" && code != 2)) invalid = 1
    if ($1 == "SKIP" || compile) skipped++
    else if ($1 == "PASS" && timeout) timedout++
    else if ($1 == "PASS" && failed) killed++
    else if ($1 == "FAIL" && passed) survived++
    else errors++
    phase = 0
    next
  }
  /^INTERNAL ERROR / { errors++ }
  /^The mutation score is / { summary = 1 }
  END {
    printf "mutation outcomes: %d killed, %d survived, %d timed out, %d not tested, %d errors\n", killed, survived, timedout, skipped, errors
    if (invalid || phase || !summary) {
      print "mutation: incomplete or unrecognized mutant output" > "/dev/stderr"
      exit 1
    }
    scored = killed + survived + timedout
    if (!scored) {
      print "mutation: no scored mutants" > "/dev/stderr"
      exit 1
    }
    score = killed * 100 / scored
    printf "mutation score %.1f%% (floor %s%%)\n", score, floor
    exit (score < floor)
  }
' "$out"
