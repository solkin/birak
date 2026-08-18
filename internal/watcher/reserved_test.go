package watcher

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
)

func TestPeriodicScanDoesNotIndexReservedMultipartState(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "watcher.db"), logger)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	partPath := filepath.Join(root, ".birak", "multipart", "upload-id", "part-00001-deadbeef")
	if err := os.MkdirAll(filepath.Dir(partPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, []byte("staged bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(root, "bucket", "object.txt")
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, []byte("published"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".birak"), filepath.Join(root, "state-alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	external := filepath.Join(t.TempDir(), "external-secret.txt")
	if err := os.WriteFile(external, []byte("must not replicate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "external-alias.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(objectPath, filepath.Join(root, "safe-alias.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	onChange := func(events []FileEvent) {
		for _, event := range events {
			if _, err := st.PutFile(event.Name, event.ModTime, event.Size, event.Hash, event.Deleted); err != nil {
				t.Errorf("put file %q: %v", event.Name, err)
			}
		}
	}
	w := New(root, st, logger, time.Millisecond, time.Hour, nil, onChange)
	w.periodicScan()

	if got, err := st.GetFile("bucket/object.txt"); err != nil || got == nil || got.Deleted {
		t.Fatalf("published object was not indexed: file=%+v err=%v", got, err)
	}
	if got, err := st.GetFile(".birak/multipart/upload-id/part-00001-deadbeef"); err != nil || got != nil {
		t.Fatalf("reserved multipart part reached the index: file=%+v err=%v", got, err)
	}
	for _, hidden := range []string{"state-alias", "external-alias.txt"} {
		if got, err := st.GetFile(hidden); err != nil || got != nil {
			t.Fatalf("unsafe symlink %q reached the index: file=%+v err=%v", hidden, got, err)
		}
	}
	if got, err := st.GetFile("safe-alias.txt"); err != nil || got == nil || got.Deleted {
		t.Fatalf("safe in-root symlink was not indexed: file=%+v err=%v", got, err)
	}
	if count, err := st.FileCount(); err != nil || count != 2 {
		t.Fatalf("indexed file count=%d err=%v, want 2", count, err)
	}
}
