package watcher

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveEmptyDirectory_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sub := filepath.Join(dir, "empty")
	os.MkdirAll(sub, 0o755)

	if !removeEmptyDirectory(r, filepath.Base(sub)) {
		t.Fatal("expected empty dir to be removed")
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("expected dir to be gone")
	}
}

func TestRemoveEmptyDirectory_OnlyIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sub := filepath.Join(dir, "only-ignored")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".DS_Store"), []byte("apple"), 0o644)
	os.WriteFile(filepath.Join(sub, "Thumbs.db"), []byte("windows"), 0o644)

	if removeEmptyDirectory(r, filepath.Base(sub)) {
		t.Fatal("ignored contents must prevent directory removal")
	}
	for name, want := range map[string]string{".DS_Store": "apple", "Thumbs.db": "windows"} {
		got, err := os.ReadFile(filepath.Join(sub, name))
		if err != nil || string(got) != want {
			t.Fatalf("ignored file changed: %s %q %v", name, got, err)
		}
	}
}

func TestRemoveEmptyDirectory_HasRealFile(t *testing.T) {
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sub := filepath.Join(dir, "has-real")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".DS_Store"), []byte("apple"), 0o644)
	os.WriteFile(filepath.Join(sub, "important.txt"), []byte("keep me"), 0o644)

	if removeEmptyDirectory(r, filepath.Base(sub)) {
		t.Fatal("expected dir with real files NOT to be removed")
	}
	// Both files should still be there.
	if _, err := os.Stat(filepath.Join(sub, ".DS_Store")); err != nil {
		t.Fatal(".DS_Store should still exist")
	}
	if _, err := os.Stat(filepath.Join(sub, "important.txt")); err != nil {
		t.Fatal("important.txt should still exist")
	}
}

func TestRemoveEmptyDirectory_HasSubdirectory(t *testing.T) {
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sub := filepath.Join(dir, "has-subdir")
	os.MkdirAll(filepath.Join(sub, "child"), 0o755)

	if removeEmptyDirectory(r, filepath.Base(sub)) {
		t.Fatal("expected dir with subdirectory NOT to be removed")
	}
}

func TestCleanEmptyParents_RecursiveCleanup(t *testing.T) {
	root := t.TempDir()
	// Create a/b/c structure.
	deepDir := filepath.Join(root, "a", "b", "c")
	os.MkdirAll(deepDir, 0o755)

	logger := slog.Default()
	patterns := []string{".DS_Store"}

	// Simulate deletion of a file in c/.
	filePath := filepath.Join(deepDir, "gone.txt")
	CleanEmptyParents(filePath, root, patterns, logger)

	// All empty parents should be removed.
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatal("a/ should have been removed")
	}
}

func TestCleanEmptyParents_StopsAtNonEmpty(t *testing.T) {
	root := t.TempDir()
	// Create parent/sub structure.
	os.MkdirAll(filepath.Join(root, "parent", "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "parent", "keep.txt"), []byte("keep"), 0o644)

	logger := slog.Default()
	patterns := []string{".DS_Store"}

	filePath := filepath.Join(root, "parent", "sub", "deleted.txt")
	CleanEmptyParents(filePath, root, patterns, logger)

	// sub/ should be removed.
	if _, err := os.Stat(filepath.Join(root, "parent", "sub")); !os.IsNotExist(err) {
		t.Fatal("parent/sub/ should have been removed")
	}
	// parent/ should still exist (has keep.txt).
	if _, err := os.Stat(filepath.Join(root, "parent")); os.IsNotExist(err) {
		t.Fatal("parent/ should still exist")
	}
	if _, err := os.Stat(filepath.Join(root, "parent", "keep.txt")); err != nil {
		t.Fatal("parent/keep.txt should still exist")
	}
}

func TestCleanEmptyParents_DoesNotRemoveRoot(t *testing.T) {
	root := t.TempDir()
	// Root only has an ignored file — it should NOT be removed.
	os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("idx"), 0o644)

	logger := slog.Default()
	patterns := []string{".DS_Store"}

	filePath := filepath.Join(root, "deleted.txt")
	CleanEmptyParents(filePath, root, patterns, logger)

	// Root should still exist.
	if _, err := os.Stat(root); os.IsNotExist(err) {
		t.Fatal("root should NOT have been removed")
	}
}

func TestCleanEmptyParents_DoesNotTouchSiblingWithSharedPrefix(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "sync")
	sibling := filepath.Join(parent, "sync-backup")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	CleanEmptyParents(filepath.Join(sibling, "deleted.txt"), root, nil, slog.Default())

	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("cleanup escaped root and removed sibling: %v", err)
	}
}

func TestCleanEmptyParents_GlobPattern(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "logs")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "app.log"), []byte("log data"), 0o644)
	os.WriteFile(filepath.Join(sub, "error.log"), []byte("error data"), 0o644)

	logger := slog.Default()
	patterns := []string{"*.log"}

	filePath := filepath.Join(sub, "deleted.txt")
	CleanEmptyParents(filePath, root, patterns, logger)

	for name, want := range map[string]string{"app.log": "log data", "error.log": "error data"} {
		got, err := os.ReadFile(filepath.Join(sub, name))
		if err != nil || string(got) != want {
			t.Fatalf("ignored file changed: %s %q %v", name, got, err)
		}
	}
}

func TestCleanEmptyParents_PreservesNestedBirakNamedFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "photos")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(sub, ".birak")
	if err := os.WriteFile(nested, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	CleanEmptyParents(filepath.Join(sub, "deleted.txt"), root, nil, slog.Default())

	if got, err := os.ReadFile(nested); err != nil || string(got) != "user data" {
		t.Fatalf("nested .birak file was removed or changed: data=%q err=%v", got, err)
	}
}

func TestShouldIgnore_ReservedStateDir(t *testing.T) {
	for _, path := range []string{
		".birak",
		".birak/multipart",
		".birak/multipart/upload-id/part-00001",
	} {
		if !ShouldIgnore(path, nil) {
			t.Errorf("ShouldIgnore(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"photos/.birak/file", "photos/.birak-backup/file"} {
		if ShouldIgnore(path, nil) {
			t.Fatalf("a non-reserved path was ignored: %s", path)
		}
	}
}

func TestShouldIgnore_ScratchFiles(t *testing.T) {
	for _, path := range []string{
		".birak-tmp-upload",
		"bucket/.birak-tmp-upload",
		"bucket/.birak-bak-rollback",
	} {
		if !ShouldIgnore(path, nil) {
			t.Errorf("ShouldIgnore(%q) = false, want true", path)
		}
	}
}

func TestWatcherShouldIgnore_UsesReservedAndConfiguredRules(t *testing.T) {
	w := &Watcher{ignorePatterns: []string{"*.log", "cache"}}

	for _, path := range []string{
		".birak/multipart/upload-id/part-00001-deadbeef",
		"bucket/.birak-tmp-upload",
		"bucket/.birak-bak-rollback",
		"bucket/debug.log",
		"bucket/cache/data.bin",
	} {
		if !w.shouldIgnore(path) {
			t.Errorf("watcher.shouldIgnore(%q) = false, want true", path)
		}
	}

	if w.shouldIgnore("bucket/photo.jpg") {
		t.Fatal("watcher ignored a normal user file")
	}
}

func TestCleanEmptyParentsDoesNotFollowOutsideSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	empty := filepath.Join(outside, "empty")
	if err := os.Mkdir(empty, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	CleanEmptyParents(filepath.Join(root, "alias", "empty", "deleted"), root, nil, slog.Default())
	if _, err := os.Stat(empty); err != nil {
		t.Fatalf("cleanup touched outside directory: %v", err)
	}
}
