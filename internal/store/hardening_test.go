package store

import (
	"fmt"
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

func TestGetFilesBatchOverThousandNames(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const total = 1200
	names := make([]string, total)
	for i := range total {
		name := fmt.Sprintf("f-%04d.bin", i)
		names[i] = name
		if _, err := st.PutLocal(FileMeta{Name: name, ModTime: 100, Size: 1, Hash: "h"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.GetFilesBatch(names)
	if err != nil {
		t.Fatalf("GetFilesBatch(%d): %v", total, err)
	}
	if len(got) != total {
		t.Fatalf("GetFilesBatch returned %d entries, want %d", len(got), total)
	}
}

func TestRecordAckEpochDeduplicatesAndSchemaIndexes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.RecordAckEpoch("peer-a", "ep1", 42); err != nil {
		t.Fatal(err)
	}
	var ts1 int64
	if err := st.db.QueryRow("SELECT updated_at FROM peer_acks WHERE peer_id = 'peer-a'").Scan(&ts1); err != nil {
		t.Fatal(err)
	}

	// Identical (peer, epoch, version) within ackDedupeTTL skips SQLite write.
	time.Sleep(5 * time.Millisecond)
	if err := st.RecordAckEpoch("peer-a", "ep1", 42); err != nil {
		t.Fatal(err)
	}
	var ts2 int64
	if err := st.db.QueryRow("SELECT updated_at FROM peer_acks WHERE peer_id = 'peer-a'").Scan(&ts2); err != nil {
		t.Fatal(err)
	}
	if ts2 != ts1 {
		t.Fatalf("unchanged ack updated SQLite timestamp: %d -> %d", ts1, ts2)
	}

	// Changing version (including rewind) writes immediately.
	if err := st.RecordAckEpoch("peer-a", "ep1", 10); err != nil {
		t.Fatal(err)
	}
	acks, err := st.PeerAcks()
	if err != nil || acks["peer-a"] != 10 {
		t.Fatalf("PeerAcks after rewind = %v, %v", acks, err)
	}

	// Verify redundant idx_files_name is absent and idx_repair_name exists.
	var count int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_files_name'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("idx_files_name count = %d, err = %v", count, err)
	}
	if err := st.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_repair_name'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("idx_repair_name count = %d, err = %v", count, err)
	}
}
