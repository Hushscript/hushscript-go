package hushscript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const contractChildEnv = "HUSHSCRIPT_CONTRACT_CHILD"

// A coverage-instrumented test binary prints its summary after PASS or FAIL.
var childCoverageLine = regexp.MustCompile(`(?m)^coverage: .*\n`)

// The SDK frames whose lock misuse a contract child may die of with an unlocked-mutex fatal error.
var (
	heartbeatLockFrames = regexp.MustCompile(`(?m)^github\.com/Hushscript/hushscript-sdk-go\.\(\*heartbeat\)\.(run|stop)\(`)
	pacerLockFrames     = regexp.MustCompile(`(?m)^github\.com/Hushscript/hushscript-sdk-go\.\(\*pacer\)\.(reserve|acquire)\(`)
	// The upload pipeline also paces its create and stops its heartbeat, so their frames count there too.
	uploadLockFrames = regexp.MustCompile(`(?m)^github\.com/Hushscript/hushscript-sdk-go\.(\(\*bodyHold\)\.(acquire|release|wait)|\(\*core\)\.newUploader\.func1|\(\*uploader\)\.sendDirect|\(\*partPages\)\.(store|fetchPage|load|urlFor|refresh)|\(\*pacer\)\.(reserve|acquire)|\(\*heartbeat\)\.(run|stop))\(`)
)

// contractHungMarker is what a contract child prints when one of its scenarios overran its deadline.
const contractHungMarker = "contract child hung: "

var (
	childRuntimeFailure = regexp.MustCompile(`(?m)^(panic:|fatal error:|WARNING: DATA RACE)`)
	childPanicLine      = regexp.MustCompile(`(?m)^panic:`)
	childRecoveredPanic = regexp.MustCompile(`(?m)^panic: .* \[recovered, repanicked\]$`)
	childTestRunner     = regexp.MustCompile(`(?m)^testing\.tRunner\.func1`)
	childUnrecoverable  = regexp.MustCompile(`(?m)^(fatal error:|WARNING: DATA RACE)`)
	childRunningHeader  = regexp.MustCompile(`(?m)^goroutine \d+ \[running\]:$`)
	childSDKCreator     = regexp.MustCompile(`(?m)^created by github\.com/Hushscript/hushscript-sdk-go\.`)
)

// sdkGoroutinePanic reports whether every panic in output crashed a goroutine the SDK started (an
// upload worker; two workers may crash at once): nothing can recover it, so it takes the caller's whole
// process down.
func sdkGoroutinePanic(output string) bool {
	panics := childPanicLine.FindAllStringIndex(output, -1)
	if len(panics) == 0 || childUnrecoverable.MatchString(output) {
		return false
	}
	for _, at := range panics {
		rest := output[at[0]:]
		header := childRunningHeader.FindStringIndex(rest)
		if header == nil {
			return false
		}
		stack := rest[header[0]:]
		if end := strings.Index(stack, "\n\n"); end >= 0 {
			stack = stack[:end]
		}
		if !childSDKCreator.MatchString(stack) || strings.Contains(stack, "testing.tRunner") {
			return false
		}
	}
	return true
}

// boundedScenario runs test as a subtest of a contract child and ends the child with exit 3 and the
// hung marker when it has not returned within limit, so a hang is a named failure in the parent and
// not a package timeout.
func boundedScenario(t *testing.T, limit time.Duration, test func(*testing.T)) {
	t.Helper()
	name := runtime.FuncForPC(reflect.ValueOf(test).Pointer()).Name()
	name = name[strings.LastIndex(name, ".")+1:]
	watchdog := time.AfterFunc(limit, func() {
		fmt.Printf("%s%s\n", contractHungMarker, name)
		os.Exit(3)
	})
	defer watchdog.Stop()
	t.Run(name, test)
}

func contractChildResult(name, output string, code int, watchdog bool, lockFrames *regexp.Regexp) string {
	output = childCoverageLine.ReplaceAllString(output, "")
	if watchdog || strings.Contains(output, "panic: test timed out after ") {
		return "timeout"
	}
	started := strings.Contains(output, "contract child started: "+name+"\n")
	if !started {
		return "error"
	}
	if code == 3 && strings.Contains(output, "\n"+contractHungMarker) && !childRuntimeFailure.MatchString(output) {
		return "hung"
	}
	if code == 2 && strings.Contains(output, "\nfatal error: sync: unlock of unlocked mutex\n") && lockFrames.MatchString(output) {
		return "fatal"
	}
	if code == 2 && sdkGoroutinePanic(output) {
		return "detached"
	}
	failed := strings.Index(output, "--- FAIL: "+name+" ")
	if recovered := childRecoveredPanic.FindStringIndex(output); code == 2 && failed >= 0 && recovered != nil && failed < recovered[0] &&
		len(childPanicLine.FindAllStringIndex(output, -1)) == 1 && childTestRunner.MatchString(output[recovered[0]:]) && !childUnrecoverable.MatchString(output) {
		return "panic"
	}
	if childRuntimeFailure.MatchString(output) {
		return "error"
	}
	if code == 1 && failed >= 0 &&
		regexp.MustCompile(`(?m)^\s+\S+_test\.go:\d+:`).MatchString(output) && strings.HasSuffix(output, "FAIL\n") {
		return "assertion"
	}
	if code == 0 && strings.Contains(output, "--- PASS: "+name+" ") && strings.HasSuffix(output, "PASS\n") {
		return "passed"
	}
	return "error"
}

// contractSubprocess reruns the current test in a child binary, so a fatal error or panic the SDK
// raises becomes a named failure of this test; it reports true in the parent.
func contractSubprocess(t *testing.T, lockFrames *regexp.Regexp) bool {
	t.Helper()
	if os.Getenv(contractChildEnv) == t.Name() {
		fmt.Printf("contract child started: %s\n", t.Name())
		return false
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.failfast", "-test.v", "-test.timeout=9s") //nolint:gosec // current test binary and exact current test name
	cmd.Env = append(os.Environ(), contractChildEnv+"="+t.Name())
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	switch contractChildResult(t.Name(), string(out), code, ctx.Err() != nil, lockFrames) {
	case "passed":
	case "assertion":
		t.Fatalf("SDK contract assertion failed in child: %q", out)
	case "fatal":
		t.Fatalf("SDK lock use must not fatally abort: child exited %d with an unlocked mutex", code)
	case "panic":
		t.Fatalf("SDK contract panicked in child: %q", out)
	case "hung":
		t.Fatalf("SDK contract scenario did not finish within its deadline in child: %q", out)
	case "detached":
		t.Fatalf("SDK goroutine panicked in child, which would crash the caller's process: %q", out)
	case "timeout":
		fmt.Println("panic: test timed out after contract child watchdog")
		t.Fatal("contract child exceeded its deadline; no semantic assertion established")
	default:
		// Not a verdict on the SDK: the marker keeps the mutation harness from scoring this run.
		fmt.Println("fatal error: contract child execution error")
		t.Fatalf("contract child execution error (exit %d): %q", code, out)
	}
	return true
}

func TestContractChildClassification(t *testing.T) {
	name := "TestMutationContractHeartbeatLifecycle"
	start := "contract child started: " + name + "\n"
	fatal := start + "fatal error: sync: unlock of unlocked mutex\ngithub.com/Hushscript/hushscript-sdk-go.(*heartbeat).stop(0x1)\n"
	named := "--- FAIL: " + name + " (0.00s)\npanic: nil pointer [recovered, repanicked]\n\ngoroutine 7 [running]:\ntesting.tRunner.func1.2({0x1, 0x2})\n"
	for _, tc := range []struct {
		name, output, want string
		code               int
		watchdog           bool
	}{
		{"SDK fatal", fatal, "fatal", 2, false},
		{"watchdog despite fatal text", fatal, "timeout", -1, true},
		{"fatal wrong exit", fatal, "error", 1, false},
		{"unrelated fatal", strings.ReplaceAll(fatal, "(*heartbeat).stop", "(*other).stop"), "error", 2, false},
		{"missing child proof", strings.TrimPrefix(fatal, start), "error", 2, false},
		{"unhandled panic", start + "panic: nil pointer\n", "error", 2, false},
		{"panic in the named test", start + named, "panic", 2, false},
		{"panic in another test", start + strings.ReplaceAll(named, name+" ", "TestOther "), "error", 2, false},
		{"panic before the failure", start + "panic: nil pointer [recovered, repanicked]\n--- FAIL: " + name + " (0.00s)\ntesting.tRunner.func1()\n", "error", 2, false},
		{"panic outside a test", start + "--- FAIL: " + name + " (0.00s)\npanic: nil pointer\n\ngoroutine 9 [running]:\nnet/http.(*persistConn).writeLoop()\n", "error", 2, false},
		{"panic with a race", start + "WARNING: DATA RACE\n" + named, "error", 2, false},
		{"second panic", start + named + "panic: again\n", "error", 2, false},
		{"package timeout", start + "panic: test timed out after 9s\n", "timeout", 2, false},
		{"tool exit", start, "error", 127, false},
		{"incomplete failure", start + "--- FAIL: " + name + " (0.00s)\nFAIL\n", "error", 1, false},
		{"named assertion", start + "    mutation_boundaries_test.go:12: mismatch\n--- FAIL: " + name + " (0.00s)\nFAIL\n", "assertion", 1, false},
		{"completed success", start + "--- PASS: " + name + " (0.00s)\nPASS\n", "passed", 0, false},
		{"covered success", start + "--- PASS: " + name + " (0.00s)\nPASS\ncoverage: 1.3% of statements in ./...\n", "passed", 0, false},
		{"covered assertion", start + "    mutation_boundaries_test.go:12: mismatch\n--- FAIL: " + name + " (0.00s)\nFAIL\ncoverage: 1.3% of statements in ./...\n", "assertion", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contractChildResult(name, tc.output, tc.code, tc.watchdog, heartbeatLockFrames); got != tc.want {
				t.Fatalf("child classification = %s, want %s", got, tc.want)
			}
		})
	}
	worker := "panic: runtime error: slice bounds out of range [:-3956]\n\ngoroutine 41 [running]:\ngithub.com/Hushscript/hushscript-sdk-go.(*uploader).worker(0x1)\n\t/work/upload.go:330 +0x1\ncreated by github.com/Hushscript/hushscript-sdk-go.(*uploader).send in goroutine 40\n\t/work/upload.go:281 +0x1\n\ngoroutine 1 [chan receive]:\ntesting.tRunner(0x1)\n"
	nested := "panic: boom [recovered]\n\tpanic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation]\n\ngoroutine 9 [running]:\ngithub.com/Hushscript/hushscript-sdk-go.(*uploader).progress.func1()\ncreated by github.com/Hushscript/hushscript-sdk-go.(*uploader).send in goroutine 8\n"
	for _, tc := range []struct {
		name, output, want string
		code               int
	}{
		{"SDK worker panic", start + worker, "detached", 2},
		{"SDK worker panic while recovering", start + nested, "detached", 2},
		{"SDK worker panic without the child proof", worker, "error", 2},
		{"SDK worker panic with another exit", start + worker, "error", 1},
		{"two SDK workers panic", start + worker + worker, "detached", 2},
		{"SDK worker panic and a bare second panic", start + worker + "panic: again\n", "error", 2},
		{"SDK worker panic and a test goroutine panic", start + worker + "panic: x\n\ngoroutine 7 [running]:\ntesting.tRunner.func1.2()\n", "error", 2},
		{"SDK worker panic with a race", start + "WARNING: DATA RACE\n" + worker, "error", 2},
		{"panic on a goroutine the SDK did not start", start + strings.ReplaceAll(worker, "created by github.com/Hushscript/hushscript-sdk-go.(*uploader).send", "created by net/http.(*Transport).dialConnFor"), "error", 2},
		{"panic on the test goroutine", start + "panic: x\n\ngoroutine 7 [running]:\ntesting.tRunner.func1.2()\ncreated by github.com/Hushscript/hushscript-sdk-go.(*uploader).send in goroutine 8\n", "error", 2},
		{"hung scenario", start + contractHungMarker + "TestX\n", "hung", 3},
		{"hung marker with another exit", start + contractHungMarker + "TestX\n", "error", 1},
		{"hung marker after a panic", start + "panic: x\n" + contractHungMarker + "TestX\n", "error", 3},
		{"hung marker without the child proof", contractHungMarker + "TestX\n", "error", 3},
		{"upload lock fatal", start + "fatal error: sync: unlock of unlocked mutex\ngithub.com/Hushscript/hushscript-sdk-go.(*partPages).fetchPage(0x1)\n", "fatal", 2},
		{"upload closure lock fatal", start + "fatal error: sync: unlock of unlocked mutex\ngithub.com/Hushscript/hushscript-sdk-go.(*core).newUploader.func1(0x1)\n", "fatal", 2},
		{"pipeline pacer lock fatal", start + "fatal error: sync: unlock of unlocked mutex\ngithub.com/Hushscript/hushscript-sdk-go.(*pacer).reserve(0x1)\n", "fatal", 2},
		{"pipeline heartbeat lock fatal", start + "fatal error: sync: unlock of unlocked mutex\ngithub.com/Hushscript/hushscript-sdk-go.(*heartbeat).stop(0x1)\n", "fatal", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contractChildResult(name, tc.output, tc.code, false, uploadLockFrames); got != tc.want {
				t.Fatalf("child classification = %s, want %s", got, tc.want)
			}
		})
	}
	pacerFatal := start + "fatal error: sync: unlock of unlocked mutex\ngithub.com/Hushscript/hushscript-sdk-go.(*pacer).reserve(0x1)\n"
	if got := contractChildResult(name, pacerFatal, 2, false, pacerLockFrames); got != "fatal" {
		t.Errorf("pacer fatal = %s, want fatal", got)
	}
	if got := contractChildResult(name, fatal, 2, false, pacerLockFrames); got != "error" {
		t.Errorf("heartbeat fatal under the pacer frames = %s, want error", got)
	}
}
