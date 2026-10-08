#!/usr/bin/env bash
# go-mutesting on the HTTP core and transcribe pipeline. It rewrites sources in place, so mutating runs on a
# copy of the tree. MUTATION_TIMEOUT (seconds, default 21600) bounds one run.
#   mutation.sh FLOOR [FILE...]        mutate the files (all of them without any: a long local run), fail below FLOOR percent
#   mutation.sh --counts OUT FILE      mutate one file and write its killed and scored counts to OUT; fails only on
#                                      a timeout or output it cannot read, whatever the score (a nightly shard)
#   mutation.sh --sum FLOOR COUNTS...  add up the counts of every file (one <file>.counts each) and fail below FLOOR
#                                      percent; a missing, extra or malformed counts file fails by name
set -euo pipefail

known=(http.go retry.go pacing.go transcribe.go upload.go resume.go wait.go heartbeat.go)

die() {
  local code="$1"
  shift
  echo "mutation: $*" >&2
  exit "$code"
}

is_known() {
  local name
  for name in "${known[@]}"; do
    if [ "$1" = "$name" ]; then return 0; fi
  done
  return 1
}

sum_counts() {
  local floor="$1" path name killed scored total_killed=0 total_scored=0
  shift
  local -A seen=()
  for path in "$@"; do
    name="$(basename -- "$path")"
    name="${name%.counts}"
    if ! is_known "$name" || [ "$(basename -- "$path")" != "$name.counts" ]; then
      die 2 "unexpected counts file '${path}' (want <source>.counts for one of: ${known[*]})"
    fi
    if [ -n "${seen[$name]:-}" ]; then die 1 "two counts files for ${name}"; fi
    seen[$name]=1
    if [ ! -f "$path" ]; then die 1 "counts file for ${name} is missing: ${path}"; fi
  done
  for name in "${known[@]}"; do
    if [ -z "${seen[$name]:-}" ]; then die 1 "no counts file for ${name}"; fi
  done
  for path in "$@"; do
    name="$(basename -- "$path")"
    name="${name%.counts}"
    mapfile -t lines < "$path"
    local tag="" source="" extra=""
    killed="" scored=""
    if [ "${#lines[@]}" -eq 1 ]; then read -r tag source killed scored extra <<< "${lines[0]}"; fi
    if [ "${#lines[@]}" -ne 1 ] || [ "$tag" != "mutation-counts" ] || [ "$source" != "$name" ] ||
      [[ ! "${killed:-}" =~ ^[0-9]+$ ]] || [[ ! "${scored:-}" =~ ^[0-9]+$ ]] || [ -n "$extra" ] || [ "$killed" -gt "$scored" ]; then
      die 1 "malformed counts file for ${name}: ${path}"
    fi
    echo "mutation counts ${name}: ${killed} killed of ${scored} scored"
    total_killed=$((total_killed + killed))
    total_scored=$((total_scored + scored))
  done
  if [ "$total_scored" -eq 0 ]; then die 1 "no scored mutants"; fi
  awk -v killed="$total_killed" -v scored="$total_scored" -v floor="$floor" 'BEGIN {
    score = killed * 100 / scored
    printf "mutation score %.1f%% (floor %s%%) over %d killed of %d scored\n", score, floor, killed, scored
    exit (score < floor)
  }'
}

mode=floor
counts_out=""
case "${1:-}" in
  --counts)
    mode=counts
    counts_out="${2:?counts file}"
    shift 2
    ;;
  --sum)
    shift
    sum_counts "${1:?minimum score in percent}" "${@:2}"
    exit
    ;;
esac

floor=0
if [ "$mode" = floor ]; then
  floor="${1:?minimum score in percent}"
  shift
fi
files=("$@")
if [ "$mode" = counts ] && [ "${#files[@]}" -ne 1 ]; then
  die 2 "--counts takes exactly one source file"
fi
if [ "${#files[@]}" -eq 0 ]; then
  files=("${known[@]}")
fi
for file in "${files[@]}"; do
  if ! is_known "$file"; then
    die 2 "unknown source file '${file}' (known: ${known[*]})"
  fi
done
limit="${MUTATION_TIMEOUT:-21600}"
if [[ ! "$limit" =~ ^[1-9][0-9]*$ ]]; then
  die 2 "MUTATION_TIMEOUT must be a whole number of seconds, got '${limit}'"
fi
if [ "$mode" = counts ]; then
  counts_out="$(realpath -m -- "$counts_out")"
  if [ ! -d "$(dirname -- "$counts_out")" ]; then
    die 2 "the directory of ${counts_out} does not exist"
  fi
  rm -f -- "$counts_out" # a failed run must not leave an earlier shard's counts behind
fi
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp -a . "$work/src"
cd "$work/src"
out="$work/mutation.txt"
# The pinned runner reports compile failures as PASS on current Go. Debug output
# includes the test result for each mutant, so the aggregate score is not used.
status=0
NO_COLOR=1 timeout "$limit" go tool -modfile=tools/go.mod go-mutesting --debug --exec-timeout=60 "${files[@]}" 2>&1 | tee "$out" || status=$?
if [ "$status" -eq 124 ]; then
  die 1 "${files[*]} timed out after ${limit}s"
fi
if [ "$status" -ne 0 ]; then
  exit "$status"
fi
awk -v floor="$floor" -v mode="$mode" -v counts_out="$counts_out" -v name="${files[0]}" '
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
    if (mode == "counts") {
      printf "mutation-counts %s %d %d\n", name, killed, scored > counts_out
      close(counts_out)
      printf "mutation shard %s: %d killed of %d scored\n", name, killed, scored
      exit 0
    }
    if (!scored) {
      print "mutation: no scored mutants" > "/dev/stderr"
      exit 1
    }
    score = killed * 100 / scored
    printf "mutation score %.1f%% (floor %s%%)\n", score, floor
    exit (score < floor)
  }
' "$out"
