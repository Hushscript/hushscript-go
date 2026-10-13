#!/usr/bin/env bash
# Pinned go-mutesting custom exec: 0 killed, 1 survived, 2 compile skip,
# 3 runtime/output error, 4 timeout. Only named, completed assertions earn kills, and so
# does a panic that testing.tRunner recovered and repanicked after marking its test failed.
# A panic outside a named test, a second panic, a fatal error or a data race stays an error.
set -euo pipefail
baseline=false
if [ "${1:-}" = --baseline ]; then baseline=true; fi
limit="${MUTATE_TIMEOUT:-60}"
[[ "$limit" =~ ^[1-9][0-9]*$ ]] || exit 3
# Go's own deadline fires first, so a hang ends with its goroutine dump instead of a bare kill.
inner=$(( limit > 10 ? limit - 5 : limit ))
work="$(mktemp -d)"
restore=false
# shellcheck disable=SC2329 # Called by the EXIT trap.
cleanup() {
  if "$restore"; then cp -- "$work/original" "$MUTATE_ORIGINAL"; fi
  rm -rf -- "$work"
}
trap cleanup EXIT
# Every mutant compiles a fresh copy of the package, about 12 MB of cache entries a mutant; into the shared
# cache they fill the /tmp tmpfs on a long shard (upload.go has ~300 mutants). A mutant gets a private copy of
# the cache the baseline warmed, removed with its work directory.
if ! "$baseline" && [ -n "${GOCACHE:-}" ] && [ -d "$GOCACHE" ]; then
  cp -a -- "$GOCACHE" "$work/gocache" || exit 3 # exit 1 would read as a survivor
  export GOCACHE="$work/gocache"
fi
printf 'mutation-exec begin\n'
if ! "$baseline"; then
  cp -- "${MUTATE_ORIGINAL:?}" "$work/original"
  restore=true
  cp -- "${MUTATE_CHANGED:?}" "$MUTATE_ORIGINAL"
fi
phase() {
  local name="$1" status=0 start elapsed result
  shift
  printf 'mutation-exec start %s\n' "$name"
  start="$(date +%s%3N)"
  timeout "$limit" "$@" > "$work/output" 2>&1 || status=$?
  elapsed=$(( $(date +%s%3N) - start ))
  cat "$work/output"
  if [ "$status" -eq 124 ] || grep -Eq '^panic: test timed out after ' "$work/output"; then
    result=timeout
  elif [ "$name" = build ]; then
    if [ "$status" -eq 0 ] && [ -x "$work/tests" ]; then result=passed
    elif [ "$status" -eq 1 ] && awk '
      /^FAIL.*\[build failed\]/ { build=1 }
      /^# [^[:space:]]+( \[[^]]+\])?$/ { header=1 }
      /^[^[:space:]]+\.go:[0-9]+:[0-9]+: .+/ { if (header) diagnostic=1 }
      END { exit !(build || diagnostic) }' "$work/output"; then result=compile
    else result=error; fi
  else
    result="$(awk -v status="$status" '
      /^--- FAIL: Test/ { failed=1 }
      /^[[:space:]]+[^[:space:]]+_test.go:[0-9]+:/ { assertion=1 }
      /^--- PASS: TestMutationContract(PacerAcquire|HeartbeatRun) / { contracts++ }
      /^PASS$/ { passed=1 }
      /^FAIL$/ { completed=1 }
      /^panic: .* \[recovered, repanicked\]$/ { if (failed && !panics) recovered=1 }
      /^panic:/ { panics++ }
      /^testing\.tRunner\.func1/ { if (recovered) runner=1 }
      /^(fatal error:|WARNING: DATA RACE)/ { fatal=1 }
      END {
        if (status == 2 && recovered && runner && panics == 1 && !fatal) print "killed"
        else if (panics || fatal) print "error"
        else if (status == 1 && failed && assertion && completed) print "killed"
        else if (status == 0 && passed && (contracts >= 2)) print "survived"
        else print "error"
      }' "$work/output")"
  fi
  # The score parser requires a build-failed marker; go test -c may omit it.
  if [ "$name" = build ] && [ "$result" = compile ] && ! grep -Eq '^FAIL.*\[build failed\]' "$work/output"; then
    printf 'FAIL mutation-exec [build failed]\n'
  fi
  printf 'mutation-exec end %s %s %d %d\n' "$name" "$result" "$status" "$elapsed"
  PHASE_RESULT="$result"
}
finish() {
  printf 'mutation-exec result %s\n' "$1"
  if "$baseline"; then [ "$1" = survived ]; exit; fi
  case "$1" in killed) exit 0;; survived) exit 1;; compile) exit 2;; timeout) exit 4;; *) exit 3;; esac
}
# One compilation per baseline/mutant; both test phases reuse this root binary.
phase build go test -c -o "$work/tests" .
[ "$PHASE_RESULT" = passed ] || finish "$PHASE_RESULT"
phase focused "$work/tests" -test.run '^TestMutationContract' -test.failfast -test.v -test.timeout="${inner}s"
[ "$PHASE_RESULT" = survived ] || finish "$PHASE_RESULT"
phase full "$work/tests" -test.failfast -test.v -test.timeout="${inner}s"
finish "$PHASE_RESULT"
