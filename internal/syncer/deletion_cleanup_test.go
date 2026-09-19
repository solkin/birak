package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

func TestDeletionPreservesIgnoredSibling(t *testing.T) {
	s, _ := auditSyncer(t)
	s.ignorePatterns = []string{"*.private"}
	s.watcher = watcher.New(s.syncDir, s.store, s.logger, time.Millisecond, time.Hour, s.ignorePatterns)
	auditIndex(t, s, auditMeta("folder/replicated.txt", "replicated body", 100), "replicated body")
	local := filepath.Join(s.syncDir, "folder", "notes.private")
	if err := os.WriteFile(local, []byte("only local user data"), 0600); err != nil {
		t.Fatal(err)
	}
	deletion := store.FileMeta{Name: "folder/replicated.txt", Deleted: true, ModTime: 200, Clock: 200, Version: 2}
	if err := s.applyDeletion(context.Background(), deletion); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.syncDir, "folder", "replicated.txt")); !os.IsNotExist(err) {
		t.Fatalf("replica deletion not applied: %v", err)
	}
	body, err := os.ReadFile(local)
	if err != nil || string(body) != "only local user data" {
		t.Fatalf("deleting replicated sibling also deleted ignored local data: %q (%v)", body, err)
	}
}
