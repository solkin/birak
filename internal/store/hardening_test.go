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

func TestPutLocalBatchAtomicAndGaplessVersions(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.PutLocalBatch(nil); err != nil {
		t.Fatalf("PutLocalBatch(nil): %v", err)
	}

	const n = 10
	batch := make([]FileMeta, n)
	for i := range n {
		batch[i] = FileMeta{
			Name:    fmt.Sprintf("dir/file-%02d.bin", i),
			ModTime: int64(1000 + i),
			Size:    int64(64 + i),
			Hash:    fmt.Sprintf("%064x", i+1),
		}
	}
	if err := st.PutLocalBatch(batch); err != nil {
		t.Fatalf("PutLocalBatch: %v", err)
	}
	maxVer, err := st.MaxVersion()
	if err != nil || maxVer != n {
		t.Fatalf("MaxVersion = %d (%v), want %d", maxVer, err, n)
	}
	count, err := st.FileCount()
	if err != nil || count != n {
		t.Fatalf("FileCount = %d (%v), want %d", count, err, n)
	}
	for i, want := range batch {
		got, err := st.GetFile(want.Name)
		if err != nil || got == nil {
			t.Fatalf("GetFile(%s) = %v, %v", want.Name, got, err)
		}
		if got.Version != int64(i+1) || got.Hash != want.Hash {
			t.Fatalf("entry %s = %+v, want version=%d hash=%s", want.Name, got, i+1, want.Hash)
		}
	}

	// A failing entry mid-batch must roll back the whole transaction and keep versions gapless.
	bad := []FileMeta{
		{Name: "dir/ok-before-fail.bin", ModTime: 2000, Size: 1, Hash: fmt.Sprintf("%064x", 99)},
		{Name: "dir/bad-clock.bin", ModTime: 2001, Size: 1, Hash: fmt.Sprintf("%064x", 100), BigClock: "not-a-clock"},
	}
	if err := st.PutLocalBatch(bad); err == nil {
		t.Fatal("PutLocalBatch with invalid clock succeeded")
	}
	if got, err := st.GetFile("dir/ok-before-fail.bin"); err != nil || got != nil {
		t.Fatalf("partial batch leaked entry: %+v (%v)", got, err)
	}
	if maxVer, err := st.MaxVersion(); err != nil || maxVer != n {
		t.Fatalf("MaxVersion after rollback = %d (%v), want %d", maxVer, err, n)
	}
	nextVer, err := st.PutLocal(FileMeta{Name: "dir/after.bin", ModTime: 3000, Size: 1, Hash: fmt.Sprintf("%064x", 101)})
	if err != nil || nextVer != n+1 {
		t.Fatalf("next version after rollback = %d (%v), want %d", nextVer, err, n+1)
	}
}
