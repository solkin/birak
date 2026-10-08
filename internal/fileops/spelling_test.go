package fileops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSpellingProtectsUnindexedPhysicalAlias(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "App.apk")
	if err := os.WriteFile(original, []byte("unindexed original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSpelling(dir, original); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "app.apk")
	info, err := os.Stat(alias)
	if os.IsNotExist(err) {
		t.Skip("physical alias requires a case-insensitive filesystem")
	}
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(base, info) {
		t.Fatal("fixture is not a physical alias")
	}
	if err := CheckSpelling(dir, alias); err == nil {
		t.Fatal("unindexed physical alias was allowed")
	}
	body, err := os.ReadFile(original)
	if err != nil || string(body) != "unindexed original" {
		t.Fatalf("original changed: %q %v", body, err)
	}
}

func TestCheckSpellingAllowsAbsentNamespaceAndRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	// File-to-directory replacements must reach normal namespace conflict logic.
	for _, path := range []string{filepath.Join(dir, "new", "child"), filepath.Join(dir, "file", "child")} {
		if err := CheckSpelling(dir, path); err != nil {
			t.Fatalf("absent namespace %s: %v", path, err)
		}
	}
	if err := CheckSpelling(dir, filepath.Join(dir, "..", "escape")); err == nil {
		t.Fatal("escape accepted")
	}
}
