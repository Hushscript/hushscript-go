package gen_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Hushscript/hushscript-go/generated"
	"github.com/Hushscript/hushscript-go/internal/gen"
)

func readRepoFile(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile("../../" + name) //nolint:gosec // test reads fixed repository files
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return content
}

func committedSpec(t *testing.T) map[string]any {
	t.Helper()
	spec, err := gen.ParseSpec(readRepoFile(t, "openapi.json"))
	if err != nil {
		t.Fatalf("parsing the committed spec: %v", err)
	}
	return spec
}

func parse(t *testing.T, text string) map[string]any {
	t.Helper()
	spec, err := gen.ParseSpec([]byte(text))
	if err != nil {
		t.Fatalf("ParseSpec(%s): %v", text, err)
	}
	return spec
}

func TestReproducesTheCommittedCatalogAndScopesFromTheCommittedSpec(t *testing.T) {
	spec := committedSpec(t)
	catalog, err := gen.EmitCatalog(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(catalog, readRepoFile(t, "generated/catalog.go")) {
		t.Error("generated/catalog.go differs from what the committed spec emits; run just regen")
	}
	scopes, err := gen.EmitScopes(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(scopes, readRepoFile(t, "generated/scopes.go")) {
		t.Error("generated/scopes.go differs from what the committed spec emits; run just regen")
	}
}

func TestReproducesTheCommittedHintsFromTheirOwnTable(t *testing.T) {
	table := map[string]string{}
	for _, slug := range generated.HintSlugs() {
		text, _ := generated.Hint(slug)
		table[slug] = text
	}
	emitted, err := gen.EmitHints(table)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(emitted, readRepoFile(t, "generated/hints.go")) {
		t.Error("generated/hints.go differs from what its own table emits")
	}
}

func TestCarriesEveryCatalogEntryOfTheCommittedSpec(t *testing.T) {
	spec := committedSpec(t)
	catalog, _ := spec["x-error-catalog"].([]any)
	if got, want := len(generated.ErrorSlugs()), len(catalog); got != want {
		t.Fatalf("catalog has %d slugs, spec has %d", got, want)
	}
	entry, ok := generated.LookupError("insufficient_balance")
	if !ok || entry.Code != 3001 || entry.Status != 402 {
		t.Errorf("insufficient_balance = %+v, %v", entry, ok)
	}
}

func TestMapsEveryOperationThatDeclaresAScope(t *testing.T) {
	spec := committedSpec(t)
	paths, _ := spec["paths"].(map[string]any)
	var declared []string
	for path, item := range paths {
		operations, _ := item.(map[string]any)
		for method, operation := range operations {
			object, isObject := operation.(map[string]any)
			if !isObject || object["x-required-scope"] == nil {
				continue
			}
			declared = append(declared, strings.ToUpper(method)+" "+path)
		}
	}
	got := generated.ScopedOperations()
	if len(got) != len(declared) {
		t.Fatalf("scope map has %d operations, spec declares %d", len(got), len(declared))
	}
	for _, key := range declared {
		if _, ok := generated.RequiredScope(key); !ok {
			t.Errorf("%s has no scope in the generated map", key)
		}
	}
}

func TestCarriesAHintForEverySlugOfSpecSection93(t *testing.T) {
	want := []string{
		"unauthorized", "pat_scope_missing", "insufficient_balance", "rate_limited",
		"too_many_active_jobs", "agent_purchase_capped", "agent_pack_locked",
		"pat_purchase_requires_app", "pat_purchase_requires_authentication", "no_default_card",
		"unsupported_media", "duration_unreadable", "duration_out_of_range", "size_out_of_range",
		"daily_limit", "policy_version_stale", "wrong_state", "resume_file_mismatch",
		"resume_file_required", "config_invalid",
	}
	if got := generated.HintSlugs(); len(got) != len(want) {
		t.Errorf("hints table has %d slugs, want %d: %v", len(got), len(want), got)
	}
	for _, slug := range want {
		if _, ok := generated.Hint(slug); !ok {
			t.Errorf("no hint for %s", slug)
		}
	}
	text, _ := generated.Hint("resume_file_mismatch")
	if !strings.Contains(text, "not the file the upload started with") {
		t.Errorf("resume_file_mismatch hint = %q", text)
	}
}

func TestKeepsPublicHintCopyFreeOfEmDashes(t *testing.T) {
	for _, slug := range generated.HintSlugs() {
		text, _ := generated.Hint(slug)
		if strings.Contains(text, "—") {
			t.Errorf("hint %s contains an em-dash", slug)
		}
	}
}

func TestEscapesQuotesAndNewlinesInEmittedStrings(t *testing.T) {
	emitted, err := gen.EmitHints(map[string]string{"a": "say \"hi\"\nthen go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(emitted), `"a": "say \"hi\"\nthen go",`) {
		t.Errorf("hint was not escaped:\n%s", emitted)
	}
}

func TestRejectsAMalformedCatalog(t *testing.T) {
	duplicate := `{"code":1000,"slug":"bad_request","status":400,"meaning":"m"}`
	cases := map[string]struct{ spec, want string }{
		"duplicate slug":      {`{"x-error-catalog":[` + duplicate + `,` + duplicate + `]}`, "duplicate error slug"},
		"no catalog":          {`{}`, "no x-error-catalog"},
		"string code":         {`{"x-error-catalog":[{"slug":"a","code":"1","status":400,"meaning":"m"}]}`, "a.code must be an integer"},
		"fractional code":     {`{"x-error-catalog":[{"slug":"a","code":1.5,"status":400,"meaning":"m"}]}`, "a.code must be an integer"},
		"missing status":      {`{"x-error-catalog":[{"slug":"a","code":1,"meaning":"m"}]}`, "a.status must be an integer"},
		"empty slug":          {`{"x-error-catalog":[{"slug":"","code":1,"status":400,"meaning":"m"}]}`, "slug must be a non-empty string"},
		"empty meaning":       {`{"x-error-catalog":[{"slug":"a","code":1,"status":400,"meaning":""}]}`, "a.meaning must be a non-empty string"},
		"entry not an object": {`{"x-error-catalog":[null]}`, "must be an object"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := gen.EmitCatalog(parse(t, tc.spec))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestSkipsOperationsWithoutAScopeAndRejectsAnEmptyOne(t *testing.T) {
	spec := parse(t, `{"paths":{"/a":{"get":{"x-required-scope":"s"},"post":{},"parameters":[]}}}`)
	emitted, err := gen.EmitScopes(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(emitted), `"GET /a": "s",`) || strings.Contains(string(emitted), "POST /a") {
		t.Errorf("unexpected scopes:\n%s", emitted)
	}
	for _, text := range []string{
		`{"paths":{"/a":{"get":{"x-required-scope":""}}}}`,
		`{"paths":{"/a":{"get":null}}}`,
		`{"paths":{"/a":null}}`,
		`{}`,
	} {
		if _, err := gen.EmitScopes(parse(t, text)); err == nil {
			t.Errorf("EmitScopes(%s) succeeded", text)
		}
	}
}

func TestRejectsEmptyOrMalformedHints(t *testing.T) {
	if _, err := gen.EmitHints(nil); err == nil {
		t.Error("EmitHints accepted an empty table")
	}
	if _, err := gen.EmitHints(map[string]string{"a": ""}); err == nil {
		t.Error("EmitHints accepted an empty hint")
	}
	for _, text := range []string{`{}`, `[]`, `{"a":1}`, `{"a":""}`, `nope`} {
		if _, err := gen.ParseHints([]byte(text)); err == nil {
			t.Errorf("ParseHints(%s) succeeded", text)
		}
	}
	hints, err := gen.ParseHints([]byte(`{"a":"text"}`))
	if err != nil || hints["a"] != "text" {
		t.Errorf("ParseHints = %v, %v", hints, err)
	}
}

func TestParseSpecRejectsWhatIsNotAnObject(t *testing.T) {
	for _, text := range []string{`nope`, `null`, `[]`} {
		if _, err := gen.ParseSpec([]byte(text)); err == nil {
			t.Errorf("ParseSpec(%s) succeeded", text)
		}
	}
}

func decodePatched(t *testing.T, text []byte) map[string]any {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal(text, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestDeclaresTheUndeclaredPathParameters(t *testing.T) {
	spec := parse(t, `{"paths":{"/a/{id}/b/{n}":{"parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"post":{}}}}`)
	text, changed, err := gen.PatchPathParameters(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "POST /a/{id}/b/{n} (n)" {
		t.Fatalf("changed = %v", changed)
	}
	patched := decodePatched(t, text)
	paths, _ := patched["paths"].(map[string]any)
	item, _ := paths["/a/{id}/b/{n}"].(map[string]any)
	post, _ := item["post"].(map[string]any)
	parameters, _ := post["parameters"].([]any)
	if len(parameters) != 1 {
		t.Fatalf("parameters = %v", parameters)
	}
	parameter, _ := parameters[0].(map[string]any)
	if parameter["name"] != "n" || parameter["in"] != "path" || parameter["required"] != true {
		t.Errorf("parameter = %v", parameter)
	}
}

func TestLeavesCompleteOperationsAlone(t *testing.T) {
	spec := parse(t, `{"components":{"parameters":{"Job":{"name":"job","in":"path","required":true}}},"paths":{"/a/{job}":{"get":{"parameters":[{"$ref":"#/components/parameters/Job"}]}}}}`)
	_, changed, err := gen.PatchPathParameters(spec)
	if err != nil || len(changed) != 0 {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
}

func TestRefusesADeclaredPathParameterThatIsNotInThePath(t *testing.T) {
	spec := parse(t, `{"paths":{"/a":{"get":{"parameters":[{"name":"ghost","in":"path","required":true}]}}}}`)
	if _, _, err := gen.PatchPathParameters(spec); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %v", err)
	}
}

func TestRefusesAnUnresolvableParameterReference(t *testing.T) {
	for _, text := range []string{
		`{"paths":{"/a/{x}":{"get":{"parameters":[{"$ref":"#/elsewhere/X"}]}}}}`,
		`{"paths":{"/a/{x}":{"get":{"parameters":[{"$ref":"#/components/parameters/X"}]}}}}`,
		`{"components":{"parameters":{}},"paths":{"/a/{x}":{"get":{"parameters":[{"$ref":"#/components/parameters/X"}]}}}}`,
		`{"paths":{"/a/{x}":{"get":{"parameters":[1]}}}}`,
		`{"paths":{"/a/{x}":{"get":{"parameters":[{"in":"path"}]}}}}`,
		`{"paths":{"/a/{x}":{"get":1}}}`,
		`{"paths":{"/a/{x}":1}}`,
		`{}`,
	} {
		if _, _, err := gen.PatchPathParameters(parse(t, text)); err == nil {
			t.Errorf("PatchPathParameters(%s) succeeded", text)
		}
	}
}

func TestEveryOperationOfTheCommittedSpecHasItsPathParametersAfterPatching(t *testing.T) {
	_, changed, err := gen.PatchPathParameters(committedSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || !strings.HasPrefix(changed[0], "POST /v1/agent/accounts/{signup_id}/claim") {
		t.Errorf("the spec's one known gap changed to %v", changed)
	}
}
