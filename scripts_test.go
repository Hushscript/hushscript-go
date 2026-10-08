package hushscript

import (
	"os"
	"os/exec"
	"path/filepath"
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
	return `bash "$1" "${2:-}"` + "\n", path
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
	}{
		{"formatter-error", true},
		{"unformatted", true},
		{"formatted", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			out, err := runGate(t, script, tc.mode)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, output = %s", err, out)
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
