package watcher

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveIfOnlyIgnored_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "empty")
	os.MkdirAll(sub, 0o755)

	logger := slog.Default()
	patterns := []string{".DS_Store", "Thumbs.db"}

	if !removeIfOnlyIgnored(sub, patterns, logger) {
		t.Fatal("expected empty dir to be removed")
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("expected dir to be gone")
	}
}

func TestRemoveIfOnlyIgnored_OnlyIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "only-ignored")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".DS_Store"), []byte("apple"), 0o644)
	os.WriteFile(filepath.Join(sub, "Thumbs.db"), []byte("windows"), 0o644)

	logger := slog.Default()
	patterns := []string{".DS_Store", "Thumbs.db"}

	if !removeIfOnlyIgnored(sub, patterns, logger) {
		t.Fatal("expected dir with only ignored files to be removed")
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("expected dir to be gone")
	}
}

func TestRemoveIfOnlyIgnored_HasRealFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "has-real")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".DS_Store"), []byte("apple"), 0o644)
	os.WriteFile(filepath.Join(sub, "important.txt"), []byte("keep me"), 0o644)

	logger := slog.Default()
	patterns := []string{".DS_Store", "Thumbs.db"}

	if removeIfOnlyIgnored(sub, patterns, logger) {
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

func TestRemoveIfOnlyIgnored_HasSubdirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "has-subdir")
	os.MkdirAll(filepath.Join(sub, "child"), 0o755)

	logger := slog.Default()
	patterns := []string{".DS_Store"}

	if removeIfOnlyIgnored(sub, patterns, logger) {
		t.Fatal("expected dir with subdirectory NOT to be removed")
	}
}

func TestCleanEmptyParents_RecursiveCleanup(t *testing.T) {
	root := t.TempDir()
	// Create a/b/c structure.
	deepDir := filepath.Join(root, "a", "b", "c")
	os.MkdirAll(deepDir, 0o755)
	// Place .DS_Store at each level.
	os.WriteFile(filepath.Join(root, "a", ".DS_Store"), []byte("idx"), 0o644)
	os.WriteFile(filepath.Join(root, "a", "b", ".DS_Store"), []byte("idx"), 0o644)
	os.WriteFile(filepath.Join(root, "a", "b", "c", ".DS_Store"), []byte("idx"), 0o644)

	logger := slog.Default()
	patterns := []string{".DS_Store"}

	// Simulate deletion of a file in c/.
	filePath := filepath.Join(deepDir, "gone.txt")
	CleanEmptyParents(filePath, root, patterns, logger)

	// All dirs should be cleaned up since each only has .DS_Store.
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatal("a/ should have been removed")
	}
}

func TestCleanEmptyParents_StopsAtNonEmpty(t *testing.T) {
	root := t.TempDir()
	// Create parent/sub structure.
	os.MkdirAll(filepath.Join(root, "parent", "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "parent", "keep.txt"), []byte("keep"), 0o644)
	os.WriteFile(filepath.Join(root, "parent", "sub", ".DS_Store"), []byte("idx"), 0o644)

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

	// logs/ should be removed since *.log matches all remaining files.
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("logs/ should have been removed (only *.log files)")
	}
}
