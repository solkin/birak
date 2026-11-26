package store

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestEnqueueNewerVersionResetsBackoff(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	peer := "http://peer-1:9100"
	v1 := FileMeta{Name: "a.bin", ModTime: 100, Size: 5, Hash: "h1", Version: 1, Clock: 10}
	if _, err := st.EnqueueChanges(peer, []FileMeta{v1}, "initial"); err != nil {
		t.Fatal(err)
	}
	items, err := st.DueRepairsInLane(peer, false, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("DueRepairsInLane = %v, %v", items, err)
	}
	// Backoff far into the future.
	if err := st.DeferRepairItem(items[0], 15*time.Minute, "transient error"); err != nil {
		t.Fatal(err)
	}
	due, err := st.DueRepairsInLane(peer, false, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("expected 0 due items while backed off, got %d (%v)", len(due), err)
	}

	// Enqueue strictly newer version: attempts and next_attempt must reset immediately.
	v2 := FileMeta{Name: "a.bin", ModTime: 200, Size: 6, Hash: "h2", Version: 2, Clock: 20}
	if n, err := st.EnqueueChanges(peer, []FileMeta{v2}, "newer"); err != nil || n != 1 {
		t.Fatalf("EnqueueChanges v2 = %d, %v", n, err)
	}
	due, err = st.DueRepairsInLane(peer, false, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("expected newer version to be immediately due, got %d (%v)", len(due), err)
	}
	if due[0].Attempts != 0 {
		t.Fatalf("expected attempts reset to 0, got %d", due[0].Attempts)
	}
}
