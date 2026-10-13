#!/usr/bin/env bash
# go-mutesting on the HTTP core and transcribe pipeline. It rewrites sources in place, so mutating runs on a
# copy of the tree. MUTATION_TIMEOUT (seconds, default 21600) bounds one run.
#   mutation.sh FLOOR [FILE...]        mutate the files (all of them without any: a long local run), fail below FLOOR percent
#   mutation.sh --counts OUT FILE      mutate one file and write its killed and scored counts to OUT; fails only on
#                                      a failing baseline, timeout or output it cannot read, whatever the score (a nightly shard)
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
executor="$(cd -- "$(dirname -- "$0")" && pwd)/mutation-exec.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp -a . "$work/src"
cd "$work/src"
# Baseline uses the same compiled-binary phases as every mutant.
if ! timeout "$limit" bash "$executor" --baseline; then
  die 1 "baseline tests failed; refusing to run mutants"
fi
out="$work/mutation.txt"
# Custom exec owns compilation and phase classification; ignore the engine score.
status=0
NO_COLOR=1 timeout "$limit" go tool -modfile=tools/go.mod go-mutesting --debug --exec-timeout=60 --exec="bash $executor" "${files[@]}" 2>&1 | tee "$out" || status=$?
if [ "$status" -eq 124 ]; then
  die 1 "${files[*]} timed out after ${limit}s"
fi
if [ "$status" -ne 0 ]; then
  exit "$status"
fi
awk -v floor="$floor" -v mode="$mode" -v counts_out="$counts_out" -v name="${files[0]}" '
  /^Execute built-in exec command for mutation$/ { invalid = 1 }
  /^mutation-exec begin$/ {
    if (phase) invalid = 1
    phase = 1
    stage = 0
    result = ""
    next
  }
  /^mutation-exec start (build|focused|full)$/ {
    want = stage == 0 ? "build" : (stage == 1 ? "focused" : "full")
    if (phase != 1 || active || $3 != want || stage > 2 || (stage > 0 && previous != "passed" && previous != "survived")) invalid = 1
    active = $3
    failed = assertion = passed = completed = runtime = timeout = contracts = compile = recovered = panics = runner = fatal = 0
    next
  }
  active {
    if ($0 ~ /^FAIL.*\[(build|setup) failed\]/) compile = 1
    if ($0 ~ /^--- FAIL: Test/) failed = 1
    if ($0 ~ /^[[:space:]]+[^[:space:]]+_test.go:[0-9]+:/) assertion = 1
    if ($0 ~ /^--- PASS: TestMutationContract(PacerAcquire|HeartbeatRun) /) contracts++
    if ($0 == "PASS") passed = 1
    if ($0 == "FAIL") completed = 1
    if ($0 ~ /^(panic:|fatal error:|WARNING: DATA RACE)/) runtime = 1
    if ($0 ~ /^panic: .* \[recovered, repanicked\]$/ && failed && !panics) recovered = 1
    if ($0 ~ /^panic:/) panics++
    if ($0 ~ /^testing\.tRunner\.func1/ && recovered) runner = 1
    if ($0 ~ /^(fatal error:|WARNING: DATA RACE)/) fatal = 1
    if ($0 ~ /^panic: test timed out after /) timeout = 1
  }
  /^mutation-exec end / {
    if (phase != 1 || $3 != active || NF != 6 || $5 !~ /^[0-9]+$/ || $6 !~ /^[0-9]+$/) invalid = 1
    previous = $4
    if (active == "build") {
      if (previous != "passed" && previous != "compile" && previous != "error" && previous != "timeout") invalid = 1
      if ((previous == "passed" && $5 != 0) || (previous == "compile" && ($5 != 1 || !compile))) invalid = 1
    } else {
      if (previous == "killed" && ($5 != 1 || !failed || !assertion || !completed || runtime) && ($5 != 2 || !recovered || !runner || panics != 1 || fatal)) invalid = 1
      else if (previous == "survived" && ($5 != 0 || !passed || contracts < 2 || runtime)) invalid = 1
      else if (previous != "killed" && previous != "survived" && previous != "error" && previous != "timeout") invalid = 1
    }
    if (previous == "timeout" && $5 != 124 && !timeout) invalid = 1
    active = ""
    stage++
    next
  }
  /^mutation-exec result / {
    if (phase != 1 || active || $3 != previous || NF != 3 || stage < 1 || (previous == "survived" && stage != 3) || previous == "passed") invalid = 1
    result = $3
    phase = 2
    next
  }
  /^Exited with -?[0-9]+$/ {
    if (phase != 2) invalid = 1
    phase = 3
    code = $3
    expected = result == "killed" ? 0 : (result == "survived" ? 1 : (result == "compile" ? 2 : (result == "timeout" ? 4 : 3)))
    if (code != expected) invalid = 1
    next
  }
  /^(PASS|FAIL|SKIP|UNKOWN exit code for) "[^"]+" with checksum [[:xdigit:]]+$/ {
    if (phase != 3) invalid = 1
    if (($1 == "PASS" && code != 0) || ($1 == "FAIL" && code != 1) || ($1 == "SKIP" && code != 2) || ($1 == "UNKOWN" && code < 3)) invalid = 1
    if (result == "compile") skipped++
    else if (result == "timeout") timedout++
    else if (result == "killed") killed++
    else if (result == "survived") survived++
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
    if (errors > 0) {
      print "mutation: mutant execution or engine errors; refusing to score or write counts" > "/dev/stderr"
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
