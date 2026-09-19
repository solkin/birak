package store

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditVersionHighWaterSurvivesTombstoneGCAndRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "audit.db")
	s, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	var previous int64
	for range 5 {
		previous, err = s.PutFile("deleted", 100, 0, "", true)
		if err != nil {
			t.Fatal(err)
		}
	}
	epoch := s.Epoch()
	if _, err := s.PurgeTombstones(time.Nanosecond, NoAckGate); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	// Generate enough writes before peers next poll that max_version has caught
	// up with their old cursor; rewind detection can no longer help them.
	var first int64
	for i := range 6 {
		version, err := s.PutFile(fmt.Sprintf("new-%d", i), int64(200+i), 3, "hash", false)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = version
		}
	}
	changes, err := s.GetChanges(previous, 100)
	if err != nil {
		t.Fatal(err)
	}
	if s.Epoch() == epoch && first <= previous {
		t.Fatalf("same epoch reused versions after ordinary GC/restart: first=%d previous=%d; cursor sees only %d of 6 new files", first, previous, len(changes))
	}
}

func TestAuditAckResetPinsTombstonesAgain(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordAck("node-b", 100); err != nil {
		t.Fatal(err)
	}
	// Same node identity starts over after its local metadata was lost.
	if err := s.RecordAck("node-b", 0); err != nil {
		t.Fatal(err)
	}
	gate, err := s.MinAckedVersion(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if gate != 0 {
		t.Fatalf("fresh since=0 still acknowledges old work through version %d", gate)
	}
}

func TestAuditRepairAndCursorSurviveRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "audit.db")
	s, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if err := s.EnqueueRepair("peer", "failed", 7, "injected download failure"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerState("peer", PeerState{Version: 7, Epoch: "epoch"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.GetPeerState("peer")
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.DueRepairs("peer", 10)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 7 || state.Epoch != "epoch" || len(items) != 1 || items[0].Name != "failed" {
		t.Fatalf("recovery lost state: cursor=%+v repairs=%+v", state, items)
	}
}

func TestRepairMetadataAndRevisionSurviveRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "repair.db")
	s, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	deletion := FileMeta{Name: "deleted", ModTime: 101, Deleted: true, Version: 7}
	if err := s.EnqueueChange("peer", deletion, "unlink failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.DueRepairs("peer", 10)
	if err != nil || len(items) != 1 || items[0].Meta == nil || !items[0].Meta.Deleted || items[0].Meta.ModTime != 101 {
		t.Fatalf("lost deletion across restart: %+v, %v", items, err)
	}
	old := items[0]
	deletion.ModTime = 201
	deletion.Version = 1 // Source restored its metadata; conflict time still wins.
	if err := s.EnqueueChange("peer", deletion, "new deletion"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveRepairItem(old); err != nil {
		t.Fatal(err)
	}
	items, err = s.DueRepairs("peer", 10)
	if err != nil || len(items) != 1 || items[0].Meta.ModTime != 201 {
		t.Fatalf("old worker erased new state: %+v, %v", items, err)
	}
}

func TestFailedFileTransactionDoesNotConsumeVersion(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_file BEFORE INSERT ON files BEGIN SELECT RAISE(FAIL,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFile("bad", 1, 0, "", false); err == nil {
		t.Fatal("fault injection failed")
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_file"); err != nil {
		t.Fatal(err)
	}
	version, err := s.PutFile("good", 1, 0, "", false)
	if err != nil || version != 1 {
		t.Fatalf("failed transaction consumed version: %d, %v", version, err)
	}
	var synchronous int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("metadata durability is not FULL: %d, %v", synchronous, err)
	}
}

func TestRestoredBackupHasNewIncarnationAfterVersionCatchup(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "restore.db")
	s, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if _, err := s.PutFile("old", 100, 3, "old", false); err != nil {
		t.Fatal(err)
	}
	databaseEpoch := s.Epoch()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	previousIncarnation := s.Incarnation()
	previousVersion, err := s.PutFile("lost-in-restore", 200, 3, "new", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := s.PutFile(fmt.Sprintf("after-restore-%d", i), 300, 3, "new", false); err != nil {
			t.Fatal(err)
		}
	}
	max, err := s.MaxVersion()
	if err != nil || max <= previousVersion {
		t.Fatalf("fixture did not catch up past cursor: max=%d cursor=%d err=%v", max, previousVersion, err)
	}
	if s.Epoch() != databaseEpoch || s.Incarnation() == previousIncarnation {
		t.Fatalf("restore must retain DB identity but invalidate peer cursors: epoch=%q incarnation=%q", s.Epoch(), s.Incarnation())
	}
}
