package syncer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

func TestWatchStallTripsAfterFinalByteWithoutEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &progressReader{}
	// Simulate all 100 expected bytes already received, with peer hanging before EOF.
	src.n.Store(100)
	sw := watchStall(src, 100, 20*time.Millisecond, cancel)
	defer sw.stop()

	select {
	case <-ctx.Done():
		if !sw.fired() {
			t.Fatal("context cancelled without stall trip")
		}
	case <-time.After(time.Second):
		t.Fatal("watchStall hung indefinitely after receiving final byte without EOF")
	}
}

func TestCommitDeletionClearsQuarantine(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	w := watcher.New(dir, st, logger, 50*time.Millisecond, time.Hour, nil)
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	syn := New(st, w, dir, "node-1", nil, nil, logger, Options{})

	filePath := filepath.Join(dir, "bad.bin")
	if err := os.WriteFile(filePath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutLocal(store.FileMeta{
		Name:    "bad.bin",
		ModTime: time.Now().UnixNano(),
		Size:    7,
		Hash:    "deadbeef",
		Clock:   10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetQuarantined("bad.bin", "inode-key-1"); err != nil {
		t.Fatal(err)
	}
	if !st.IsQuarantined("bad.bin") {
		t.Fatal("expected file to be quarantined")
	}

	delMeta := store.FileMeta{
		Name:    "bad.bin",
		ModTime: time.Now().Add(time.Second).UnixNano(),
		Deleted: true,
		Clock:   20,
	}
	if err := syn.applyDeletion(context.Background(), delMeta); err != nil {
		t.Fatalf("applyDeletion: %v", err)
	}
	if st.IsQuarantined("bad.bin") {
		t.Fatal("quarantine entry was not cleared after replicated DELETE")
	}
}
