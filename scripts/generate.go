// Command generate regenerates openapi.json and everything under generated/ from the API spec and the
// conformance repository's hints.json. It runs only in the regen compose service (just regen).
//
//	go run ./scripts [specPathOrURL]
//
// The spec defaults to https://api.hushscript.com/openapi.json. hints.json is read from
// $CONFORMANCE_DIR/hints.json when that is set, otherwise from the conformance repository at the
// ref in the conformance-ref file.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Hushscript/hushscript-go/internal/gen"
)

const generatedFileMode = 0o644

func main() {
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Minute)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: go run ./scripts [specPathOrURL]")
	}
	source := gen.DefaultSpec
	if len(args) == 1 {
		source = args[0]
	}
	specText, err := loadText(ctx, source)
	if err != nil {
		return err
	}
	spec, err := gen.ParseSpec(specText)
	if err != nil {
		return err
	}
	hintsText, err := loadHints(ctx)
	if err != nil {
		return err
	}
	hints, err := gen.ParseHints(hintsText)
	if err != nil {
		return err
	}

	catalog, err := gen.EmitCatalog(spec)
	if err != nil {
		return err
	}
	scopes, err := gen.EmitScopes(spec)
	if err != nil {
		return err
	}
	hintsSource, err := gen.EmitHints(hints)
	if err != nil {
		return err
	}
	patched, changed, err := gen.PatchPathParameters(spec)
	if err != nil {
		return err
	}
	for _, operation := range changed {
		fmt.Fprintln(os.Stderr, "generate: declared the undeclared path parameter of", operation)
	}

	// The spec's own bytes, so key order and formatting survive and the regen diff stays reviewable.
	if !bytes.HasSuffix(specText, []byte("\n")) {
		specText = append(specText, '\n')
	}
	files := map[string][]byte{
		"openapi.json":         specText,
		"generated/catalog.go": catalog,
		"generated/scopes.go":  scopes,
		"generated/hints.go":   hintsSource,
	}
	for name, content := range files {
		if err := os.WriteFile(name, content, generatedFileMode); err != nil { //nolint:gosec // repository sources, meant to be world-readable
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return generateClient(ctx, patched)
}

// generateClient runs oapi-codegen, which tools/go.mod pins through its tool directive (kept out of the
// module a consumer resolves), on a private copy of the spec.
func generateClient(ctx context.Context, patched []byte) error {
	dir, err := os.MkdirTemp("", "hushscript-spec-")
	if err != nil {
		return fmt.Errorf("creating a temporary directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	specPath := filepath.Join(dir, "openapi.json")
	if err := os.WriteFile(specPath, patched, 0o600); err != nil { //nolint:gosec // the path is inside the directory this process just created
		return fmt.Errorf("writing the patched spec: %w", err)
	}
	command := exec.CommandContext(ctx, "go", "tool", "-modfile=tools/go.mod", "oapi-codegen", "-config", "oapi-codegen.yaml", specPath) //nolint:gosec // fixed tool name, the only variable is a path this process just created
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("oapi-codegen: %w", err)
	}
	return nil
}

func loadHints(ctx context.Context) ([]byte, error) {
	if dir := os.Getenv("CONFORMANCE_DIR"); dir != "" {
		return readFile(filepath.Join(dir, "hints.json"))
	}
	ref, err := readFile("conformance-ref")
	if err != nil {
		return nil, err
	}
	return fetch(ctx, "https://raw.githubusercontent.com/Hushscript/hushscript-sdk-conformance/"+strings.TrimSpace(string(ref))+"/hints.json")
}

func loadText(ctx context.Context, source string) ([]byte, error) {
	if strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://") {
		return fetch(ctx, source)
	}
	return readFile(source)
}

func readFile(path string) ([]byte, error) {
	content, err := os.ReadFile(path) //nolint:gosec // the generator reads the paths its operator names
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return content, nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody) //nolint:gosec // the operator names the spec URL
	if err != nil {
		return nil, fmt.Errorf("building the request for %s: %w", url, err)
	}
	response, err := http.DefaultClient.Do(request) //nolint:gosec // the operator names the spec URL
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %d", url, response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	return body, nil
}
