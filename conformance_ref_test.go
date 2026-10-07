package hushscript

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The conformance ref is pinned in one file; nothing else may restate it (compose.yaml, the justfile,
// the helper script and the workflows all read it).
func TestTheConformanceRefIsPinnedInOneFileOnly(t *testing.T) {
	raw, err := os.ReadFile("conformance-ref")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+\n$`).Match(raw) {
		t.Fatalf("conformance-ref = %q, want a tag and a newline", raw)
	}
	tag := strings.TrimSpace(string(raw))
	for _, name := range []string{"compose.yaml", "justfile", "scripts/conformance-dir.sh", "scripts/generate.go", ".github/workflows/ci.yml", "internal/gen/gen.go"} {
		content, err := os.ReadFile(name) //nolint:gosec // fixed names of this repository
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), tag) || regexp.MustCompile(`#v\d+\.\d+\.\d+`).Match(content) {
			t.Errorf("%s restates the conformance tag; read conformance-ref instead", name)
		}
	}
}
