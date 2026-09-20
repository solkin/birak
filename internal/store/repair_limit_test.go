package store

// An unbounded queue is not durability, it is a disk that fills up. The cap
// turns wholesale divergence into back-pressure: new names are refused, the
// caller's cursor stops, and the backlog is visible instead of growing.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func limitStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "limit.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func queued(name string) FileMeta {
	return FileMeta{Name: name, ModTime: 1, Size: 1, Hash: strings.Repeat("a", 64), Version: 1}
}

func TestRepairQueueCapRefusesNewNamesAndKeepsUpdatingOldOnes(t *testing.T) {
	s := limitStore(t)
	s.SetRepairLimit(3)

	for i := 0; i < 3; i++ {
		if err := s.EnqueueChange("peer", queued(fmt.Sprintf("f-%d", i)), "poll"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	err := s.EnqueueChange("peer", queued("f-3"), "poll")
	if !errors.Is(err, ErrRepairQueueFull) {
		t.Fatalf("a full queue accepted another name: %v", err)
	}

	// A name already queued must still be able to record newer state, or a full
	// queue would freeze the work it already holds.
	newer := queued("f-0")
	newer.ModTime = 99
	newer.Version = 99
	if err := s.EnqueueChange("peer", newer, "newer state"); err != nil {
		t.Fatalf("a queued name could not be updated: %v", err)
	}
	items, err := s.DueRepairs("peer", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("queue holds %d entries, want 3", len(items))
	}
	for _, item := range items {
		if item.Name == "f-0" && (item.Meta == nil || item.Meta.ModTime != 99) {
			t.Fatalf("newer state was not recorded: %+v", item.Meta)
		}
	}

	// Draining one entry makes room again.
	if err := s.ResolveRepair("peer", "f-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueChange("peer", queued("f-3"), "poll"); err != nil {
		t.Fatalf("queue did not recover after draining: %v", err)
	}
}

// The cap is per peer: one diverged source must not stop the others.
func TestRepairQueueCapIsPerPeer(t *testing.T) {
	s := limitStore(t)
	s.SetRepairLimit(1)
	if err := s.EnqueueChange("peer-a", queued("x"), "poll"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueChange("peer-a", queued("y"), "poll"); !errors.Is(err, ErrRepairQueueFull) {
		t.Fatalf("peer-a exceeded its cap: %v", err)
	}
	if err := s.EnqueueChange("peer-b", queued("y"), "poll"); err != nil {
		t.Fatalf("peer-b was blocked by peer-a's backlog: %v", err)
	}
}

func TestRepairQueueCapCanBeDisabled(t *testing.T) {
	s := limitStore(t)
	s.SetRepairLimit(0)
	for i := 0; i < 50; i++ {
		if err := s.EnqueueChange("peer", queued(fmt.Sprintf("f-%d", i)), "poll"); err != nil {
			t.Fatalf("enqueue %d with the cap disabled: %v", i, err)
		}
	}
}
