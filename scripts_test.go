package hushscript

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func gateFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // fake commands must be executable
		t.Fatal(err)
	}
}

func runGate(t *testing.T, script, mode string, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	gateFile(t, filepath.Join(bin, "go"), `#!/bin/sh
case "$1" in
list)
  printf 'example.test/probe\n'
  if [ "$GATE_MODE" = list-error ]; then exit 1; fi
  if [ "$GATE_MODE" = build-error ]; then printf 'example.test/broken\n'; fi ;;
test)
  case "$2" in
  -list)
    case "$GATE_MODE" in
    build-error)
      if [ "$4" = example.test/broken ]; then echo 'build failed' >&2; exit 1; fi
      printf 'FuzzSniff\nok example.test/probe\n' ;;
    no-targets) printf 'ok example.test/probe\n' ;;
    *) printf 'FuzzSniff\nok example.test/probe\n' ;;
    esac ;;
  *) echo "fuzz-run $*" ;;
  esac ;;
build) exit 0 ;;
tool)
  case "$*" in
  *gofumpt*)
    case "$GATE_MODE" in
    formatter-error) exit 1 ;;
    unformatted) echo 'dirty.go' ;;
    esac ;;
  *go-mutesting*)
    echo "mutation-files $*"
    if [ "$GATE_MODE" = mutation-hang ]; then exec sleep 30; fi
    case "$GATE_MODE" in
    mutation)
      for n in 1 2 3 4 5 6 7 8; do
        printf 'Execute built-in exec command for mutation\n--- FAIL: TestProbe (0.00s)\nFAIL\nExited with 0\nPASS "/tmp/probe.go.%s" with checksum abc123\n' "$n"
      done
      for n in 9 10; do
        printf 'Execute built-in exec command for mutation\nok example.test/probe 0.001s\nExited with 1\nFAIL "/tmp/probe.go.%s" with checksum abc123\n' "$n"
      done ;;
    *) printf '%s\n' "$MUTATION_OUTPUT" ;;
    esac
    if [ "${MUTATION_NO_SUMMARY:-0}" != 1 ]; then
      echo 'The mutation score is 0.99 (99 passed, 1 failed, 0 duplicated, 0 skipped, total is 100)'
    fi
    exit "${MUTATION_EXIT:-0}" ;;
  esac ;;
*) exit 2 ;;
esac
`)
	gateFile(t, filepath.Join(bin, "golangci-lint"), "#!/bin/sh\necho lint-ran\n")
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{"-c", script, "gate"}, args...)...) //nolint:gosec // script and arguments come from test fixtures
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GATE_MODE="+mode, "FUZZ_TIME=1ms")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gateScript(t *testing.T, name string) (script, path string) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("scripts", name))
	if err != nil {
		t.Fatal(err)
	}
	return `script="$1"; shift; bash "$script" "$@"` + "\n", path
}

func TestFuzzGateRejectsBuildErrorsAndZeroTargets(t *testing.T) {
	script, path := gateScript(t, "fuzz.sh")
	for _, tc := range []struct {
		mode      string
		wantError bool
	}{
		{"list-error", true},
		{"build-error", true},
		{"no-targets", true},
		{"targets", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			out, err := runGate(t, script, tc.mode, path)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, output = %s", err, out)
			}
			if !tc.wantError && strings.Count(out, "fuzz-run ") != 1 {
				t.Fatalf("want one fuzz run, got %s", out)
			}
		})
	}
}

func TestMutationGateIncludesTheTranscribePipelineAndKeepsTheFloor(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	out, err := runGate(t, script, "mutation", path, "75")
	if err != nil {
		t.Fatalf("mutation gate: %v\n%s", err, out)
	}
	if !strings.Contains(out, " --debug ") {
		t.Fatalf("mutation invocation must include --debug: %s", out)
	}
	for _, name := range []string{"http.go", "retry.go", "pacing.go", "transcribe.go", "upload.go", "resume.go", "wait.go", "heartbeat.go"} {
		if strings.Count(out, " "+name) != 1 {
			t.Errorf("mutation invocation must include %s once: %s", name, out)
		}
	}
	if out, err = runGate(t, script, "mutation", path, "81"); err == nil {
		t.Fatalf("score below floor passed: %s", out)
	}
}

func TestMutationGateSelectsSourceFilesAndKeepsTheFloor(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	for _, tc := range []struct {
		name, floor string
		files       []string
		wantError   bool
	}{
		{"one-file", "75", []string{"http.go"}, false},
		{"multiple-files", "75", []string{"upload.go", "retry.go"}, false},
		{"floor-boundary", "80", []string{"http.go"}, false},
		{"below-floor", "81", []string{"http.go"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{path, tc.floor}, tc.files...)
			out, err := runGate(t, script, "mutation", args...)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError = %v, output = %s", err, tc.wantError, out)
			}
			want := "mutation-files tool -modfile=tools/go.mod go-mutesting --debug --exec-timeout=60 " + strings.Join(tc.files, " ") + "\n"
			if strings.Count(out, want) != 1 {
				t.Fatalf("want one invocation %q, got %s", want, out)
			}
		})
	}
	for _, files := range [][]string{{"http.go"}, {"upload.go", "retry.go"}} {
		t.Run("zero-scored-"+strings.Join(files, "-"), func(t *testing.T) {
			t.Setenv("MUTATION_OUTPUT", "")
			args := append([]string{path, "75"}, files...)
			out, err := runGate(t, script, "mutation-fixture", args...)
			if err == nil || !strings.Contains(out, "mutation: no scored mutants") {
				t.Fatalf("zero scored mutants: error = %v, output = %s", err, out)
			}
		})
	}
}

func TestMutationGateRejectsUnknownSourceFiles(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	for _, files := range [][]string{{"nope.go"}, {"http.go", "nope.go"}, {""}, {"http.go retry.go"}, {"HTTP.go"}, {"./http.go"}} {
		t.Run(strings.Join(files, "|"), func(t *testing.T) {
			out, err := runGate(t, script, "mutation", append([]string{path, "75"}, files...)...)
			if err == nil {
				t.Fatalf("accepted %q: %s", files, out)
			}
			if !strings.Contains(out, "mutation: unknown source file '") || strings.Contains(out, "mutation-files") {
				t.Fatalf("output = %s", out)
			}
		})
	}
}

func TestMutationGateScoresOnlyTestedMutants(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	mutant := func(output, exit, status string) string {
		return "Execute built-in exec command for mutation\n" + output + "\nExited with " + exit + "\n" + status + " \"/tmp/probe.go.1\" with checksum abc123\n"
	}
	killed := mutant("--- FAIL: TestProbe (0.00s)\nFAIL\nFAIL example.test/probe 0.001s", "0", "PASS")
	survived := mutant("ok example.test/probe 0.001s", "1", "FAIL")
	compile := mutant("# example.test/probe\n./probe.go:1: undefined: missing\nFAIL example.test/probe [build failed]", "0", "PASS")
	skipped := mutant("Mutation did not compile", "2", "SKIP")
	timedOut := mutant("panic: test timed out after 1m0s\nFAIL example.test/probe 60.001s", "0", "PASS")
	errored := mutant("Unknown exit code", "3", "UNKOWN exit code for")
	for _, tc := range []struct {
		name, output, floor, want string
		wantError                 bool
	}{
		{"compile-only", compile, "75", "no scored mutants", true},
		{"skip-only", skipped, "75", "no scored mutants", true},
		{"error-only", errored, "75", "no scored mutants", true},
		{"empty", "", "75", "no scored mutants", true},
		{"killed", killed, "75", "mutation score 100.0%", false},
		{"survived", survived, "75", "mutation score 0.0%", true},
		{"timeout", timedOut, "75", "mutation score 0.0%", true},
		{"mixed", killed + survived + compile + skipped + errored + timedOut, "34", "mutation score 33.3%", true},
		{"mixed-floor", killed + survived + compile + skipped + errored + timedOut, "33", "mutation score 33.3%", false},
		{"timeout-after-failure", mutant("--- FAIL: TestProbe (0.00s)\npanic: test timed out after 1m0s", "0", "PASS"), "75", "mutation score 0.0%", true},
		{"unexplained-pass", mutant("go: tool failed", "0", "PASS"), "75", "no scored mutants", true},
		{"no-tests", mutant("? example.test/probe [no test files]", "1", "FAIL"), "75", "no scored mutants", true},
		{"setup-error", mutant("FAIL example.test/probe [setup failed]", "0", "PASS"), "75", "no scored mutants", true},
		{"incomplete", killed + "Execute built-in exec command for mutation\n--- FAIL: TestProbe (0.00s)\n", "75", "incomplete or unrecognized mutant output", true},
		{"status-without-debug", "PASS \"/tmp/probe.go.1\" with checksum abc123\n", "75", "incomplete or unrecognized mutant output", true},
		{"unmarked-panic", mutant("panic: initialization failed", "0", "PASS"), "75", "no scored mutants", true},
		{"missing-summary", killed, "75", "incomplete or unrecognized mutant output", true},
		{"mismatched-status", mutant("--- FAIL: TestProbe (0.00s)", "1", "PASS"), "75", "incomplete or unrecognized mutant output", true},
		{"engine-error", killed, "75", "The mutation score is", true},
		{"internal-error", "INTERNAL ERROR formatting mutant\n" + killed, "75", "1 errors", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MUTATION_OUTPUT", tc.output)
			if tc.name == "missing-summary" {
				t.Setenv("MUTATION_NO_SUMMARY", "1")
			}
			if tc.name == "engine-error" {
				t.Setenv("MUTATION_EXIT", "2")
			}
			out, err := runGate(t, script, "mutation-fixture", path, tc.floor)
			if (err != nil) != tc.wantError || !strings.Contains(out, tc.want) {
				t.Fatalf("error = %v, wantError = %v, want %q in output:\n%s", err, tc.wantError, tc.want, out)
			}
		})
	}
}

func TestGofumptGateRejectsFormatterFailureAndDirtyFiles(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, lint, ok := strings.Cut(string(raw), "\n  lint:\n")
	if !ok {
		t.Fatal("compose has no lint service")
	}
	lint, _, ok = strings.Cut(lint, "\n  conformance:\n")
	if !ok {
		t.Fatal("compose lint service has no end")
	}
	_, script, ok := strings.Cut(lint, "      - >-\n")
	if !ok {
		t.Fatal("compose lint command missing")
	}
	script = strings.Join(strings.FieldsFunc(script, func(r rune) bool { return r == '\n' }), " ")
	script = os.Expand(script, func(name string) string {
		if name == "$" {
			return "$"
		}
		return ""
	})
	for _, tc := range []struct {
		mode      string
		wantError bool
		wantFiles string
	}{
		{"formatter-error", true, ""},
		{"unformatted", true, "gofumpt: unformatted files:\ndirty.go\n"},
		{"formatted", false, ""},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			out, err := runGate(t, script, tc.mode)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, output = %s", err, out)
			}
			if tc.wantFiles != "" && !strings.Contains(out, tc.wantFiles) {
				t.Fatalf("the unformatted files are not printed: %s", out)
			}
			if tc.wantFiles == "" && strings.Contains(out, "dirty.go") {
				t.Fatalf("a file is named although none is unformatted: %s", out)
			}
			if !tc.wantError && strings.Count(out, "lint-ran") != 1 {
				t.Fatalf("lint did not run once: %s", out)
			}
			if tc.wantError && strings.Contains(out, "lint-ran") {
				t.Fatalf("lint ran after formatting failed: %s", out)
			}
		})
	}
}

var shardNames = []string{"http.go", "retry.go", "pacing.go", "transcribe.go", "upload.go", "resume.go", "wait.go", "heartbeat.go"}

func TestMutationShardWritesItsCountsAndFailsOnlyOnABrokenRun(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	mutant := func(output, exit, status string) string {
		return "Execute built-in exec command for mutation\n" + output + "\nExited with " + exit + "\n" + status + " \"/tmp/probe.go.1\" with checksum abc123\n"
	}
	killed := mutant("--- FAIL: TestProbe (0.00s)\nFAIL\nFAIL example.test/probe 0.001s", "0", "PASS")
	survived := mutant("ok example.test/probe 0.001s", "1", "FAIL")
	for _, tc := range []struct {
		name, mode, output, want string
		wantError                bool
	}{
		{"killed and survived", "mutation", "", "mutation-counts http.go 8 10\n", false},
		{"every mutant survived", "mutation-fixture", survived + survived, "mutation-counts http.go 0 2\n", false},
		{"no scored mutants", "mutation-fixture", "", "mutation-counts http.go 0 0\n", false},
		{"one killed", "mutation-fixture", killed, "mutation-counts http.go 1 1\n", false},
		{"output that cannot be read", "mutation-fixture", killed + "Execute built-in exec command for mutation\n--- FAIL: TestProbe (0.00s)\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MUTATION_OUTPUT", tc.output)
			counts := filepath.Join(t.TempDir(), "http.go.counts")
			gateFile(t, counts, "stale from an earlier run\n")
			out, err := runGate(t, script, tc.mode, path, "--counts", counts, "http.go")
			got, readErr := os.ReadFile(counts) //nolint:gosec // a path the test made
			if tc.wantError {
				if err == nil || !strings.Contains(out, "mutation: incomplete or unrecognized mutant output") || readErr == nil {
					t.Fatalf("error = %v, counts read error = %v, output = %s", err, readErr, out)
				}
				return
			}
			if err != nil || readErr != nil || string(got) != tc.want {
				t.Fatalf("error = %v, counts = %q (%v), want %q, output = %s", err, got, readErr, tc.want, out)
			}
		})
	}
}

func TestMutationShardRefusesWhatItCannotRun(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	counts := filepath.Join(t.TempDir(), "x.counts")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no file", []string{"--counts", counts}, "--counts takes exactly one source file"},
		{"two files", []string{"--counts", counts, "http.go", "retry.go"}, "--counts takes exactly one source file"},
		{"unknown file", []string{"--counts", counts, "nope.go"}, "unknown source file 'nope.go'"},
		{"no directory", []string{"--counts", filepath.Join(t.TempDir(), "missing", "x.counts"), "http.go"}, "does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runGate(t, script, "mutation", append([]string{path}, tc.args...)...)
			if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, "mutation-files") {
				t.Fatalf("error = %v, want %q in output: %s", err, tc.want, out)
			}
		})
	}
}

func TestMutationTimeoutNamesTheFileAndLeavesNoCounts(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	t.Setenv("MUTATION_TIMEOUT", "1")
	counts := filepath.Join(t.TempDir(), "http.go.counts")
	out, err := runGate(t, script, "mutation-hang", path, "--counts", counts, "http.go")
	if err == nil || !strings.Contains(out, "mutation: http.go timed out after 1s") {
		t.Fatalf("shard: error = %v, output = %s", err, out)
	}
	if _, statErr := os.Stat(counts); statErr == nil {
		t.Error("a timed-out shard left a counts file")
	}
	out, err = runGate(t, script, "mutation-hang", path, "75", "http.go", "retry.go")
	if err == nil || !strings.Contains(out, "mutation: http.go retry.go timed out after 1s") {
		t.Fatalf("floor run: error = %v, output = %s", err, out)
	}
}

func TestMutationTimeoutMustBeWholeSeconds(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	for _, limit := range []string{"0", "-5", "1.5", "abc", " 3", "03x"} {
		t.Run(limit, func(t *testing.T) {
			t.Setenv("MUTATION_TIMEOUT", limit)
			out, err := runGate(t, script, "mutation", path, "75", "http.go")
			if err == nil || !strings.Contains(out, fmt.Sprintf("mutation: MUTATION_TIMEOUT must be a whole number of seconds, got '%s'", limit)) || strings.Contains(out, "mutation-files") {
				t.Fatalf("error = %v, output = %s", err, out)
			}
		})
	}
}

func writeShards(t *testing.T, counts map[string]string) (dir string, paths []string) {
	t.Helper()
	dir = t.TempDir()
	for _, name := range shardNames {
		content, ok := counts[name]
		if !ok {
			continue
		}
		file := filepath.Join(dir, name+".counts")
		gateFile(t, file, content)
		paths = append(paths, file)
	}
	return dir, paths
}

func allShards(killed, scored int) map[string]string {
	counts := map[string]string{}
	for _, name := range shardNames {
		counts[name] = fmt.Sprintf("mutation-counts %s %d %d\n", name, killed, scored)
	}
	return counts
}

func TestMutationSumKeepsTheAggregateFloor(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	weakSmallFile := allShards(10, 10)
	weakSmallFile["pacing.go"] = "mutation-counts pacing.go 0 3\n"
	weakSmallFile["heartbeat.go"] = "mutation-counts heartbeat.go 0 0\n"
	for _, tc := range []struct {
		name, floor, want string
		counts            map[string]string
		wantError         bool
	}{
		{"at the floor", "70", "mutation score 70.0% (floor 70%) over 56 killed of 80 scored", allShards(7, 10), false},
		{"below the floor", "71", "mutation score 70.0% (floor 71%) over 56 killed of 80 scored", allShards(7, 10), true},
		{"one weak small file does not sink the total", "75", "mutation score 95.2% (floor 75%) over 60 killed of 63 scored", weakSmallFile, false},
		{"a file with no scored mutants is fine", "75", "mutation counts heartbeat.go: 0 killed of 0 scored", weakSmallFile, false},
		{"nothing scored at all", "75", "mutation: no scored mutants", allShards(0, 0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, paths := writeShards(t, tc.counts)
			out, err := runGate(t, script, "mutation", append([]string{path, "--sum", tc.floor}, paths...)...)
			if (err != nil) != tc.wantError || !strings.Contains(out, tc.want) {
				t.Fatalf("error = %v, want %q in output: %s", err, tc.want, out)
			}
		})
	}
}

func TestMutationSumFailsLoudlyOnAMissingOrBrokenShard(t *testing.T) {
	script, path := gateScript(t, "mutation.sh")
	good := func() map[string]string { return allShards(8, 10) }
	run := func(t *testing.T, counts map[string]string, extra ...string) (string, error) {
		t.Helper()
		_, paths := writeShards(t, counts)
		return runGate(t, script, "mutation", append(append([]string{path, "--sum", "75"}, paths...), extra...)...)
	}
	t.Run("a shard artifact that never arrived", func(t *testing.T) {
		counts := good()
		delete(counts, "retry.go")
		out, err := run(t, counts)
		if err == nil || !strings.Contains(out, "mutation: no counts file for retry.go") || strings.Contains(out, "mutation score") {
			t.Fatalf("error = %v, output = %s", err, out)
		}
	})
	t.Run("a listed file that does not exist", func(t *testing.T) {
		dir, paths := writeShards(t, good())
		for _, p := range paths {
			if strings.HasSuffix(p, "wait.go.counts") {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
		}
		out, err := runGate(t, script, "mutation", append([]string{path, "--sum", "75"}, paths...)...)
		want := "mutation: counts file for wait.go is missing: " + filepath.Join(dir, "wait.go.counts")
		if err == nil || !strings.Contains(out, want) {
			t.Fatalf("error = %v, want %q in output: %s", err, want, out)
		}
	})
	for name, content := range map[string]string{
		"empty file":          "",
		"garbage":             "killed everything\n",
		"another file's name": "mutation-counts http.go 8 10\n",
		"negative":            "mutation-counts upload.go -1 10\n",
		"killed above scored": "mutation-counts upload.go 11 10\n",
		"fractions":           "mutation-counts upload.go 1.5 10\n",
		"missing scored":      "mutation-counts upload.go 8\n",
		"extra field":         "mutation-counts upload.go 8 10 1\n",
		"two lines":           "mutation-counts upload.go 8 10\nmutation-counts upload.go 8 10\n",
		"wrong tag":           "counts upload.go 8 10\n",
	} {
		t.Run("malformed: "+name, func(t *testing.T) {
			counts := good()
			counts["upload.go"] = content
			out, err := run(t, counts)
			if err == nil || !strings.Contains(out, "mutation: malformed counts file for upload.go: ") || strings.Contains(out, "mutation score") {
				t.Fatalf("error = %v, output = %s", err, out)
			}
		})
	}
	t.Run("a file that is not one of the shards", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "other.go.counts")
		gateFile(t, other, "mutation-counts other.go 1 1\n")
		out, err := run(t, good(), other)
		if err == nil || !strings.Contains(out, "mutation: unexpected counts file '"+other+"'") {
			t.Fatalf("error = %v, output = %s", err, out)
		}
	})
	t.Run("the same shard twice", func(t *testing.T) {
		_, paths := writeShards(t, good())
		again := filepath.Join(t.TempDir(), "http.go.counts")
		gateFile(t, again, "mutation-counts http.go 8 10\n")
		out, err := runGate(t, script, "mutation", append(append([]string{path, "--sum", "75"}, paths...), again)...)
		if err == nil || !strings.Contains(out, "mutation: two counts files for http.go") {
			t.Fatalf("error = %v, output = %s", err, out)
		}
	})
	t.Run("an unmatched glob", func(t *testing.T) {
		out, err := runGate(t, script, "mutation", path, "--sum", "75", "counts/*.counts")
		if err == nil || !strings.Contains(out, "mutation: unexpected counts file 'counts/*.counts'") {
			t.Fatalf("error = %v, output = %s", err, out)
		}
	})
}

func TestMutationFloorIsStoredOnceAndTheNightlyMatrixCoversEveryFile(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(name) //nolint:gosec // fixed repository paths
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	justfile, compose, nightly, script := read("justfile"), read("compose.yaml"), read(".github/workflows/nightly.yml"), read("scripts/mutation.sh")
	if strings.Count(justfile, "mutation_floor :=") != 1 {
		t.Error("the justfile must define mutation_floor once")
	}
	_, mutation, ok := strings.Cut(compose, "\n  mutation:\n")
	if !ok {
		t.Fatal("compose has no mutation service")
	}
	mutation, _, _ = strings.Cut(mutation, "\n  fuzz:\n")
	if strings.Contains(mutation, "mutation.sh") || regexp.MustCompile(`\b75\b`).MatchString(mutation) {
		t.Errorf("the compose mutation service must not carry a command or a floor:\n%s", mutation)
	}
	if regexp.MustCompile(`\b75\b`).MatchString(nightly) {
		t.Error("the nightly workflow must not carry the floor")
	}
	declared := regexp.MustCompile(`(?m)^known=\((.*)\)$`).FindStringSubmatch(script)
	if declared == nil {
		t.Fatal("mutation.sh declares no known files")
	}
	var matrix []string
	for _, match := range regexp.MustCompile(`\{file: ([a-z]+\.go),`).FindAllStringSubmatch(nightly, -1) {
		matrix = append(matrix, match[1])
	}
	if got, want := strings.Join(matrix, " "), declared[1]; got != want {
		t.Errorf("the nightly matrix runs %q, mutation.sh knows %q", got, want)
	}
	if got := strings.Join(shardNames, " "); got != declared[1] {
		t.Errorf("shardNames = %q, mutation.sh knows %q", got, declared[1])
	}
}
