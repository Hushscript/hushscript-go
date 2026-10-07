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
