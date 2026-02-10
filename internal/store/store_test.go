package store

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s, err := New(dbPath, logger)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutAndGetFile(t *testing.T) {
	s := newTestStore(t)

	ver, err := s.PutFile("hello.txt", 1000, 42, "abc123", false)
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if ver != 1 {
		t.Fatalf("expected version 1, got %d", ver)
	}

	f, err := s.GetFile("hello.txt")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if f == nil {
		t.Fatal("expected file, got nil")
	}
	if f.Name != "hello.txt" || f.ModTime != 1000 || f.Size != 42 || f.Hash != "abc123" || f.Deleted || f.Version != 1 {
		t.Fatalf("unexpected file: %+v", f)
	}
}

func TestPutFileUpdatesVersion(t *testing.T) {
	s := newTestStore(t)

	v1, _ := s.PutFile("a.txt", 1000, 10, "hash1", false)
	v2, _ := s.PutFile("b.txt", 2000, 20, "hash2", false)
	v3, _ := s.PutFile("a.txt", 3000, 30, "hash3", false)

	if v1 != 1 || v2 != 2 || v3 != 3 {
		t.Fatalf("expected versions 1,2,3 got %d,%d,%d", v1, v2, v3)
	}

	f, _ := s.GetFile("a.txt")
	if f.Version != 3 || f.Hash != "hash3" || f.ModTime != 3000 {
		t.Fatalf("expected updated file, got %+v", f)
	}
}

func TestGetChanges(t *testing.T) {
	s := newTestStore(t)

	s.PutFile("a.txt", 1000, 10, "h1", false)
	s.PutFile("b.txt", 2000, 20, "h2", false)
	s.PutFile("c.txt", 3000, 30, "h3", false)
	s.PutFile("a.txt", 4000, 40, "h4", false) // update a.txt -> version 4

	// Get all changes.
	changes, err := s.GetChanges(0, 100)
	if err != nil {
		t.Fatalf("GetChanges: %v", err)
	}
	// a.txt was updated in place, so we have 3 entries: b(v2), c(v3), a(v4).
	if len(changes) != 3 {
		t.Fatalf("expected 3 changes, got %d", len(changes))
	}

	// Get changes since version 2.
	changes, err = s.GetChanges(2, 100)
	if err != nil {
		t.Fatalf("GetChanges since 2: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes since v2, got %d", len(changes))
	}
	if changes[0].Name != "c.txt" || changes[1].Name != "a.txt" {
		t.Fatalf("unexpected order: %+v", changes)
	}

	// Get changes with limit.
	changes, _ = s.GetChanges(0, 2)
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes with limit, got %d", len(changes))
	}
}

func TestGetFileNotFound(t *testing.T) {
	s := newTestStore(t)
	f, err := s.GetFile("nonexistent.txt")
	if err != nil {
		t.Fatalf("GetFile error: %v", err)
	}
	if f != nil {
		t.Fatalf("expected nil, got %+v", f)
	}
}

func TestMaxVersion(t *testing.T) {
	s := newTestStore(t)

	v, _ := s.MaxVersion()
	if v != 0 {
		t.Fatalf("expected 0, got %d", v)
	}

	s.PutFile("a.txt", 1000, 10, "h1", false)
	s.PutFile("b.txt", 2000, 20, "h2", false)

	v, _ = s.MaxVersion()
	if v != 2 {
		t.Fatalf("expected 2, got %d", v)
	}
}

func TestCursors(t *testing.T) {
	s := newTestStore(t)

	v, _ := s.GetCursor("peer1")
	if v != 0 {
		t.Fatalf("expected 0, got %d", v)
	}

	s.SetCursor("peer1", 42)
	v, _ = s.GetCursor("peer1")
	if v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}

	s.SetCursor("peer1", 100)
	v, _ = s.GetCursor("peer1")
	if v != 100 {
		t.Fatalf("expected 100, got %d", v)
	}
}

func TestFileCount(t *testing.T) {
	s := newTestStore(t)

	count, _ := s.FileCount()
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}

	s.PutFile("a.txt", 1000, 10, "h1", false)
	s.PutFile("b.txt", 2000, 20, "h2", false)
	s.PutFile("c.txt", 3000, 30, "h3", true) // deleted

	count, _ = s.FileCount()
	if count != 2 {
		t.Fatalf("expected 2, got %d", count)
	}
}

func TestPurgeTombstones(t *testing.T) {
	s := newTestStore(t)

	oldTime := time.Now().Add(-48 * time.Hour).UnixNano()
	recentTime := time.Now().UnixNano()

	s.PutFile("old-deleted.txt", oldTime, 0, "", true)
	s.PutFile("recent-deleted.txt", recentTime, 0, "", true)
	s.PutFile("alive.txt", recentTime, 100, "h1", false)

	purged, err := s.PurgeTombstones(24 * time.Hour)
	if err != nil {
		t.Fatalf("PurgeTombstones: %v", err)
	}
	if purged != 1 {
		t.Fatalf("expected 1 purged, got %d", purged)
	}

	// old-deleted should be gone.
	f, _ := s.GetFile("old-deleted.txt")
	if f != nil {
		t.Fatalf("expected old-deleted to be purged, got %+v", f)
	}

	// recent-deleted should remain.
	f, _ = s.GetFile("recent-deleted.txt")
	if f == nil {
		t.Fatal("expected recent-deleted to remain")
	}

	// alive should remain.
	f, _ = s.GetFile("alive.txt")
	if f == nil || f.Deleted {
		t.Fatal("expected alive to remain")
	}
}

func TestConcurrentPutFile(t *testing.T) {
	s := newTestStore(t)

	// Hammer the store with concurrent writes from multiple goroutines.
	const goroutines = 10
	const writesPerGoroutine = 50
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < writesPerGoroutine; i++ {
				name := fmt.Sprintf("file-g%d-%d.txt", gid, i)
				_, err := s.PutFile(name, int64(i), int64(i*10), fmt.Sprintf("hash-%d-%d", gid, i), false)
				if err != nil {
					t.Errorf("PutFile failed: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// Verify: each goroutine wrote writesPerGoroutine unique files.
	count, _ := s.FileCount()
	expected := int64(goroutines * writesPerGoroutine)
	if count != expected {
		t.Fatalf("expected %d files, got %d", expected, count)
	}

	// Versions should be sequential 1..N.
	maxVer, _ := s.MaxVersion()
	if maxVer != expected {
		t.Fatalf("expected max version %d, got %d", expected, maxVer)
	}
}

func TestGetChangesEmpty(t *testing.T) {
	s := newTestStore(t)

	changes, err := s.GetChanges(0, 100)
	if err != nil {
		t.Fatalf("GetChanges on empty: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected 0 changes, got %d", len(changes))
	}
}

func TestDeleteAndRecreateSameFile(t *testing.T) {
	s := newTestStore(t)

	// Create.
	v1, _ := s.PutFile("cycle.txt", 1000, 10, "h1", false)
	// Delete.
	v2, _ := s.PutFile("cycle.txt", 2000, 0, "", true)
	// Recreate with different content.
	v3, _ := s.PutFile("cycle.txt", 3000, 20, "h2", false)

	if v1 >= v2 || v2 >= v3 {
		t.Fatalf("versions should be increasing: %d, %d, %d", v1, v2, v3)
	}

	f, _ := s.GetFile("cycle.txt")
	if f.Deleted {
		t.Fatal("file should not be deleted after recreate")
	}
	if f.Hash != "h2" {
		t.Fatalf("expected hash h2, got %s", f.Hash)
	}
	if f.Version != v3 {
		t.Fatalf("expected version %d, got %d", v3, f.Version)
	}

	// GetChanges should only return the latest state.
	changes, _ := s.GetChanges(0, 100)
	found := false
	for _, c := range changes {
		if c.Name == "cycle.txt" {
			if found {
				t.Fatal("duplicate entry in changes for cycle.txt")
			}
			found = true
			if c.Deleted {
				t.Fatal("changes entry should show latest state (not deleted)")
			}
		}
	}
	if !found {
		t.Fatal("cycle.txt not in changes")
	}
}

func TestAllFiles(t *testing.T) {
	s := newTestStore(t)

	s.PutFile("a.txt", 1000, 10, "h1", false)
	s.PutFile("b.txt", 2000, 20, "h2", false)
	s.PutFile("c.txt", 3000, 30, "h3", true) // deleted

	files, err := s.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 non-deleted files, got %d", len(files))
	}
	if _, ok := files["a.txt"]; !ok {
		t.Fatal("expected a.txt")
	}
	if _, ok := files["b.txt"]; !ok {
		t.Fatal("expected b.txt")
	}
}

func TestGetFilesBatch(t *testing.T) {
	s := newTestStore(t)

	s.PutFile("a.txt", 1000, 10, "h1", false)
	s.PutFile("b.txt", 2000, 20, "h2", false)
	s.PutFile("c.txt", 3000, 30, "h3", true) // deleted

	// Query existing and non-existing names.
	result, err := s.GetFilesBatch([]string{"a.txt", "c.txt", "missing.txt"})
	if err != nil {
		t.Fatalf("GetFilesBatch: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if result["a.txt"] == nil || result["a.txt"].Hash != "h1" {
		t.Fatal("expected a.txt with hash h1")
	}
	if result["c.txt"] == nil || !result["c.txt"].Deleted {
		t.Fatal("expected c.txt as deleted")
	}
	if result["missing.txt"] != nil {
		t.Fatal("expected missing.txt to be nil")
	}

	// Empty batch should return nil.
	empty, err := s.GetFilesBatch(nil)
	if err != nil {
		t.Fatalf("GetFilesBatch empty: %v", err)
	}
	if empty != nil {
		t.Fatalf("expected nil for empty batch, got %v", empty)
	}
}

func TestListNonDeleted(t *testing.T) {
	s := newTestStore(t)

	s.PutFile("a.txt", 1000, 10, "h1", false)
	s.PutFile("b.txt", 2000, 20, "h2", false)
	s.PutFile("c.txt", 3000, 30, "h3", true) // deleted
	s.PutFile("d.txt", 4000, 40, "h4", false)
	s.PutFile("e.txt", 5000, 50, "h5", false)

	// First page of 2.
	page1, err := s.ListNonDeleted("", 2)
	if err != nil {
		t.Fatalf("ListNonDeleted page 1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("expected 2 in page 1, got %d", len(page1))
	}
	if page1[0].Name != "a.txt" || page1[1].Name != "b.txt" {
		t.Fatalf("unexpected page 1: %+v", page1)
	}

	// Second page starting after "b.txt" — should skip deleted "c.txt".
	page2, err := s.ListNonDeleted("b.txt", 2)
	if err != nil {
		t.Fatalf("ListNonDeleted page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("expected 2 in page 2, got %d", len(page2))
	}
	if page2[0].Name != "d.txt" || page2[1].Name != "e.txt" {
		t.Fatalf("unexpected page 2: %+v", page2)
	}

	// Third page — empty.
	page3, err := s.ListNonDeleted("e.txt", 2)
	if err != nil {
		t.Fatalf("ListNonDeleted page 3: %v", err)
	}
	if len(page3) != 0 {
		t.Fatalf("expected 0 in page 3, got %d", len(page3))
	}
}

func TestCachedVersionCounter(t *testing.T) {
	s := newTestStore(t)

	// Initial max version should be 0.
	v, _ := s.MaxVersion()
	if v != 0 {
		t.Fatalf("expected 0, got %d", v)
	}

	s.PutFile("a.txt", 1000, 10, "h1", false) // v1
	s.PutFile("b.txt", 2000, 20, "h2", false) // v2

	v, _ = s.MaxVersion()
	if v != 2 {
		t.Fatalf("expected 2, got %d", v)
	}

	// Purge should not affect the version counter.
	s.PutFile("del.txt", 1, 0, "", true) // v3
	s.PurgeTombstones(0)

	v, _ = s.MaxVersion()
	if v != 3 {
		t.Fatalf("expected 3 after purge, got %d", v)
	}

	// Next put should continue from 4.
	v4, _ := s.PutFile("c.txt", 3000, 30, "h3", false)
	if v4 != 4 {
		t.Fatalf("expected version 4, got %d", v4)
	}
}
