package store

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestQuarantineAndDamagedGenerationSurviveReopen(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "node.db")
	s, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuarantined("file", "old-inode"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsQuarantined("file") || !s.IsDamagedObject("old-inode") {
		t.Fatal("lost durable quarantine")
	}
	if err := s.ClearQuarantine("file", "replacement-inode"); err != nil {
		t.Fatal(err)
	}
	if s.IsQuarantined("file") || !s.IsDamagedObject("old-inode") {
		t.Fatal("repair rehabilitated a displaced inode")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.IsQuarantined("file") || !s.IsDamagedObject("old-inode") {
		t.Fatal("damaged alias became readable after reopen")
	}
	if err := s.ClearQuarantine("alias", "old-inode"); err != nil {
		t.Fatal(err)
	}
	if s.IsDamagedObject("old-inode") {
		t.Fatal("verified repair did not clear the object fence")
	}
}

func TestFailedQuarantinePersistenceStillFencesLiveReads(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "node.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuarantined("file", "inode"); err == nil {
		t.Fatal("hidden persistence failure")
	}
	if !s.IsQuarantined("file") || !s.IsDamagedObject("inode") {
		t.Fatal("known bad bytes readable after storage error")
	}
	if err := s.ClearQuarantine("file", "inode"); err == nil {
		t.Fatal("hidden clear failure")
	}
	if !s.IsQuarantined("file") {
		t.Fatal("failed clear opened the quarantine")
	}
}
