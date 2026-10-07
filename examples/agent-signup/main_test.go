package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheTokenFileIsPrivateNewOrExisting(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh")
	if err := keep(fresh, "hsr1:token"); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("old"), 0o644); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if err := keep(loose, "hsr1:token"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fresh, loose} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", path, info.Mode().Perm())
		}
		if text, _ := os.ReadFile(path); string(text) != "hsr1:token\n" { //nolint:gosec // a temp file
			t.Errorf("%s holds %q", path, text)
		}
	}
}

// The old file is replaced, never truncated in place: a failure before the new token is complete
// leaves the earlier token as it was, and no temporary file is left behind.
func TestAnEarlierTokenFileIsReplacedByARenameAndNoTemporaryFileRemains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("hsr1:earlier\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = keep(path, "hsr1:later"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the token file was rewritten in place, so it was truncated before the new token was complete")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("the directory holds %v (%v), want only the token file", entries, err)
	}
}

func TestAFailedKeepLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "inner"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := keep(target, "hsr1:token"); err == nil {
		t.Fatal("keep replaced a non-empty directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("the directory holds %v (%v), want only the target", entries, err)
	}
}

func TestOnlyATextThatPassesTheTokenCheckIsKept(t *testing.T) {
	for text, want := range map[string]bool{
		"hsr1:eu:pat.aaaa.bbbb": true,
		"":                      false,
		"   ":                   false,
		"not-a-token":           false,
		"hsr1:has space":        false,
		"hsr1:new\nline":        false,
		"hsr1:ünï":              false,
	} {
		if got := isToken(text); got != want {
			t.Errorf("isToken(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestTheTokenCheckIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("HUSHSCRIPT_BASE_URL", "ftp://not-a-base-url")
	t.Setenv("HUSHSCRIPT_LOG", "loud")
	t.Setenv("HUSHSCRIPT_TOKEN", "")
	if !isToken("hsr1:eu:pat.aaaa.bbbb") {
		t.Error("a well-formed token failed the check because of an unrelated setting")
	}
	if isToken("not-a-token") {
		t.Error("a text without the prefix passed")
	}
}
