package config

import (
	"strings"
	"testing"
)

func TestMaxExternalDeletions(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.MaxExternalDeletions != 1000 {
		t.Fatalf("default sync.max_external_deletions=%d, want 1000", cfg.Sync.MaxExternalDeletions)
	}

	cfg, err = Load(writeYAML(t, "sync:\n  max_external_deletions: 50000\n"))
	if err != nil || cfg.Sync.MaxExternalDeletions != 50000 {
		t.Fatalf("from YAML: %d %v", cfg.Sync.MaxExternalDeletions, err)
	}

	t.Setenv("BIRAK_SYNC_MAX_EXTERNAL_DELETIONS", "0")
	cfg, err = Load("")
	if err != nil || cfg.Sync.MaxExternalDeletions != 0 {
		t.Fatalf("0 disables the breaker: %d %v", cfg.Sync.MaxExternalDeletions, err)
	}

	t.Setenv("BIRAK_SYNC_MAX_EXTERNAL_DELETIONS", "-1")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "sync.max_external_deletions must not be negative") {
		t.Fatalf("negative limit accepted: %v", err)
	}
	t.Setenv("BIRAK_SYNC_MAX_EXTERNAL_DELETIONS", "lots")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "BIRAK_SYNC_MAX_EXTERNAL_DELETIONS") {
		t.Fatalf("unparsable limit accepted: %v", err)
	}
}
