package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/store"
)

func TestRecoverReplicaHandlesDirectoryAtIntentPath(t *testing.T) {
	w := auditWatcher(t)
	if err := w.store.StageReplica(store.FileMeta{
		Name:    "clash",
		ModTime: 1000,
		Size:    5,
		Hash:    "abcd",
		Clock:   10,
	}); err != nil {
		t.Fatal(err)
	}
	// Put a directory at the staged replica path before recovery runs.
	if err := os.MkdirAll(filepath.Join(w.dir, "clash"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.CheckStorage(); err != nil {
		t.Fatalf("CheckStorage failed when replica intent path became a directory: %v", err)
	}
	intent, err := w.store.ReplicaIntent("clash")
	if err != nil || intent != nil {
		t.Fatalf("expected stale replica intent to be cleared, got %v, %v", intent, err)
	}
}
