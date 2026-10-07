package hushscript_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Hushscript/hushscript-go/internal/goldens"
)

// conformanceDir is a checkout of the conformance repository; a run without one fails, so the goldens
// cannot go untested by leaving a variable unset.
func conformanceDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CONFORMANCE_DIR")
	if dir == "" {
		t.Fatal("CONFORMANCE_DIR must point at a checkout of hushscript-sdk-conformance (the justfile sets it)")
	}
	return dir
}

func readFixture(t *testing.T, dir string, parts ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{dir}, parts...)...)) //nolint:gosec // a fixture path from the environment
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// The list of goldens must be exactly the files the suite ships: a golden added, dropped or renamed
// upstream fails here until the list follows.
func TestTheGoldenListIsExactlyWhatTheSuiteShips(t *testing.T) {
	dir := conformanceDir(t)
	var scenario struct {
		Expect struct {
			Result struct {
				Checked int `json:"checked"`
			} `json:"result"`
		} `json:"expect"`
	}
	if err := json.Unmarshal(readFixture(t, dir, "scenarios", "format-goldens.json"), &scenario); err != nil {
		t.Fatal(err)
	}
	all := goldens.All()
	names := make([]string, len(all))
	for i, g := range all {
		names[i] = g.File
	}
	if len(names) != scenario.Expect.Result.Checked || len(names) != 72 {
		t.Fatalf("%d goldens listed, the scenario checks %d (72 expected)", len(names), scenario.Expect.Result.Checked)
	}
	slices.Sort(names)
	if len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Error("a golden is listed twice")
	}
	if shipped := fileNames(t, filepath.Join(dir, "fixtures", "exports")); !slices.Equal(names, shipped) {
		t.Errorf("listed goldens differ from fixtures/exports:\nlisted:  %v\nshipped: %v", names, shipped)
	}
	wantTranscripts := slices.Clone(goldens.Transcripts)
	slices.Sort(wantTranscripts)
	shipped := fileNames(t, filepath.Join(dir, "fixtures", "transcripts"))
	for i, name := range shipped {
		shipped[i] = name[:len(name)-len(".json")]
	}
	if !slices.Equal(wantTranscripts, shipped) {
		t.Errorf("listed transcripts %v, fixtures/transcripts holds %v", wantTranscripts, shipped)
	}
}

// Every golden is rendered from its transcript fixture and must match byte for byte.
func TestEveryGoldenIsRenderedByteForByte(t *testing.T) {
	dir := conformanceDir(t)
	matched := 0
	for _, g := range goldens.All() {
		t.Run(g.File, func(t *testing.T) {
			rendered, err := goldens.Render(g, readFixture(t, dir, "fixtures", "transcripts", g.Transcript+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if want := string(readFixture(t, dir, "fixtures", "exports", g.File)); rendered != want {
				t.Errorf("rendered output differs from the golden\n got: %q\nwant: %q", rendered, want)
				return
			}
			matched++
		})
	}
	if matched != 72 {
		t.Errorf("%d goldens matched, want 72", matched)
	}
}
