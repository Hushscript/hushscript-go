// Command runexamples runs every program in examples/ against the conformance mock the container
// shares a network with. Each example gets a fresh mock scenario, a sample recording and the arguments
// it documents. It runs only through `just examples`.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

const mockToken = /* the mock accepts any hsr token */ "hsr1:eu:pat.00000000000000000000000000000000.0000000000000000000000000000000000000000000000000000000000000000" //nolint:gosec // a made-up token for the mock

func main() {
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Minute)
	err := run(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "runexamples:", err)
		os.Exit(1)
	}
}

func baseURL() string {
	if base := os.Getenv("HUSHSCRIPT_BASE_URL"); base != "" {
		return base
	}
	return "http://127.0.0.1:4010"
}

func mock(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, baseURL()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s answered %d", method, path, resp.StatusCode)
	}
	return data, nil
}

func run(ctx context.Context) error {
	work, err := os.MkdirTemp("", "examples-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	sample := filepath.Join(work, "sample.mp3")
	media, err := mock(ctx, http.MethodGet, "/__files/fixtures/media/cbr-20s.mp3", nil)
	if err != nil {
		return err
	}
	recordings := filepath.Join(work, "recordings")
	if mkErr := os.MkdirAll(recordings, 0o750); mkErr != nil {
		return mkErr
	}
	for _, name := range []string{sample, filepath.Join(recordings, "one.mp3"), filepath.Join(recordings, "two.mp3")} {
		if writeErr := os.WriteFile(name, media, 0o600); writeErr != nil {
			return writeErr
		}
	}
	tokenFile := filepath.Join(work, "agent-token")

	// What each example is run with: the arguments it documents.
	args := map[string][]string{
		"transcribe":   {sample},
		"export-srt":   {sample, filepath.Join(work, "meeting.srt")},
		"batch-folder": {recordings, filepath.Join(work, "transcripts")},
		"buy":          {"-pack", "300min", "-yes"},
		"agent-signup": {"-terms", "2026-10-01", "-token-file", tokenFile},
	}
	entries, err := os.ReadDir("examples")
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("no examples found")
	}
	failed := false
	for _, name := range names {
		scenario := []byte(`{"mock":{"account":{"balance_seconds":360000},"completeState":"done"}}`)
		if _, err = mock(ctx, http.MethodPost, "/__scenario", scenario); err != nil {
			return err
		}
		fmt.Println("==", name)
		cmd := exec.CommandContext(ctx, "go", append([]string{"run", "./examples/" + name}, args[name]...)...) //nolint:gosec // fixed tool, the names are directories of this repository
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Env = append(os.Environ(), "HUSHSCRIPT_BASE_URL="+baseURL(), "HUSHSCRIPT_TOKEN="+mockToken)
		if err = cmd.Run(); err != nil {
			fmt.Printf("FAILED %s: %v\n", name, err)
			failed = true
		}
	}
	if info, statErr := os.Stat(tokenFile); statErr != nil || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		fmt.Println("FAILED agent-signup: the token file is missing, empty or not mode 0600")
		failed = true
	}
	if failed {
		return fmt.Errorf("an example failed")
	}
	return nil
}
