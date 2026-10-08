package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovedStorageModeFailsBeforeOpeningDirectories(t *testing.T) {
	dir := t.TempDir()
	syncDir, metaDir := filepath.Join(dir, "sync"), filepath.Join(dir, "meta")
	path := filepath.Join(dir, "config.yaml")
	data := fmt.Sprintf("storage_mode: quorum\nsync_dir: %q\nmeta_dir: %q\n", syncDir, metaDir)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(path); err == nil || !strings.Contains(err.Error(), "unsupported storage_mode") {
		t.Fatalf("expected removed mode to fail startup, got %v", err)
	}
	for _, path := range []string{syncDir, metaDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("startup touched filesystem storage %s: %v", path, err)
		}
	}
}
