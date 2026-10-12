package gateway

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/fileops"
)

func TestScratchCleanupRespectsStorageFence(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, ".birak-tmp-other-volume")
	if err := os.WriteFile(scratch, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	fileops.SetHooks(root, fileops.Hooks{Validate: func() error { return errors.New("storage unavailable") }})
	defer fileops.SetHooks(root, fileops.Hooks{})
	SweepTempFiles(root, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if body, err := os.ReadFile(scratch); err != nil || string(body) != "retain" {
		t.Fatalf("cleaned unavailable storage: %q %v", body, err)
	}
}
