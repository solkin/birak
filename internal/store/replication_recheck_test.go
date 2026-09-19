package store

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// A stale worker must never affect a newly inserted generation of the same key.
func TestReviewOldRepairMustNotResolveReinsertedRow(t *testing.T) {
	s := newTestStore(t)
	old := FileMeta{Name: "item", ModTime: 100, Deleted: true, Version: 1}
	if err := s.EnqueueChange("peer", old, "first failure"); err != nil {
		t.Fatal(err)
	}
	items, err := s.DueRepairs("peer", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("initial queue: %+v %v", items, err)
	}
	workerA, workerB := items[0], items[0]
	if err := s.ResolveRepairItem(workerA); err != nil {
		t.Fatal(err)
	}
	newer := FileMeta{Name: "item", ModTime: 200, Deleted: true, Version: 2}
	if err := s.EnqueueChange("peer", newer, "new failure"); err != nil {
		t.Fatal(err)
	}
	// Neither backoff nor completion of the old task may affect its replacement.
	if err := s.DeferRepairItem(workerB, time.Hour, "stale error"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveRepairItem(workerB); err != nil {
		t.Fatal(err)
	}
	items, err = s.DueRepairs("peer", 10)
	if err != nil || len(items) != 1 || items[0].Meta.ModTime != newer.ModTime {
		t.Fatalf("old revision erased a new queue generation: pending=%+v err=%v", items, err)
	}
}

func TestConflictClockAndIntentsSurviveReopen(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutRemote(FileMeta{Name: "file", ModTime: 100, Hash: "old", Clock: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutLocal(FileMeta{Name: "file", ModTime: 50, Hash: "new"}); err != nil {
		t.Fatal(err)
	}
	meta, _ := s.GetFile("file")
	if meta.Clock != 1001 || meta.ModTime != 50 {
		t.Fatalf("clock/mtime: %+v", meta)
	}
	s.BeginLocal([]string{"tree"})
	s.StageReplica(FileMeta{Name: "other", Clock: 2000})
	s.Close()
	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	meta, err = s.GetFile("file")
	if err != nil || meta.Clock != 1001 {
		t.Fatalf("reopened: %+v %v", meta, err)
	}
	trusted, err := s.HasLocalIntent("tree/child")
	if err != nil || !trusted {
		t.Fatalf("lost local intent: %v %v", trusted, err)
	}
	remote, err := s.ReplicaIntent("other")
	if err != nil || remote == nil || remote.Clock != 2000 {
		t.Fatalf("lost replica intent: %+v %v", remote, err)
	}
	if _, err = s.PutLocal(FileMeta{Name: "file", ModTime: 1, Deleted: true}); err != nil {
		t.Fatal(err)
	}
	deletion, _ := s.GetFile("file")
	if CompareState(deletion, meta) <= 0 {
		t.Fatal("deletion after timestamp rollback loses")
	}
	if _, err = s.PutLocal(FileMeta{Name: "file", ModTime: 1, Hash: "recreated"}); err != nil {
		t.Fatal(err)
	}
	recreated, _ := s.GetFile("file")
	if CompareState(recreated, deletion) <= 0 {
		t.Fatal("recreation loses to tombstone")
	}
}
