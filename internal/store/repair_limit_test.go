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
	"time"
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

// Queue summaries are remembered so a large backlog is not rescanned on every
// scrape — but a caller must still read back what it just wrote.
func TestQueueSummaryFollowsTheQueue(t *testing.T) {
	s := limitStore(t)
	for i := 0; i < 3; i++ {
		if err := s.EnqueueChange("peer", queued(fmt.Sprintf("f-%d", i)), "poll"); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.RepairQueueStats()
	if err != nil || stats.Total != 3 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if n, err := s.PendingRepairCount("peer"); err != nil || n != 3 {
		t.Fatalf("pending = %d, %v", n, err)
	}

	if err := s.ResolveRepair("peer", "f-1"); err != nil {
		t.Fatal(err)
	}
	stats, err = s.RepairQueueStats()
	if err != nil || stats.Total != 2 {
		t.Fatalf("a resolved item was still summarised: %+v, %v", stats, err)
	}
	if n, err := s.PendingRepairCount("peer"); err != nil || n != 2 {
		t.Fatalf("a resolved item was still counted: %d, %v", n, err)
	}

	if err := s.EnqueueChange("peer", queued("f-9"), "poll"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PendingRepairCount("peer"); err != nil || n != 3 {
		t.Fatalf("a new item was not counted: %d, %v", n, err)
	}
}

// The queue length is a counter, not a query: asking the database how many rows
// a peer has, on every new name, turned a bulk sync into O(n²). The counter has
// to survive a reopen and stay exact through every path that adds or removes a
// row.
func TestQueueLengthIsExactAndSurvivesReopen(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "queue.db")
	s, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if err := s.EnqueueChange("peer-a", queued(fmt.Sprintf("a-%d", i)), "poll"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if err := s.EnqueueChange("peer-b", queued(fmt.Sprintf("b-%d", i)), "poll"); err != nil {
			t.Fatal(err)
		}
	}
	// Re-enqueueing an existing name must not inflate the count.
	if err := s.EnqueueChange("peer-a", queued("a-0"), "again"); err != nil {
		t.Fatal(err)
	}
	// Deferring does not change it either.
	items, err := s.DueRepairs("peer-a", 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("due: %v %v", items, err)
	}
	if err := s.DeferRepairItem(items[0], time.Minute, "later"); err != nil {
		t.Fatal(err)
	}
	// Resolving does, and resolving a stale revision does not.
	if err := s.ResolveRepair("peer-a", "a-1"); err != nil {
		t.Fatal(err)
	}
	stale := items[0]
	stale.Revision = 999999
	if err := s.ResolveRepairItem(stale); err != nil {
		t.Fatal(err)
	}

	assertQueued := func(what string, store *Store) {
		t.Helper()
		if n, err := store.PendingRepairCount("peer-a"); err != nil || n != 9 {
			t.Fatalf("%s: peer-a holds %d entries, want 9 (%v)", what, n, err)
		}
		if n, err := store.PendingRepairCount("peer-b"); err != nil || n != 4 {
			t.Fatalf("%s: peer-b holds %d entries, want 4 (%v)", what, n, err)
		}
		rows, err := store.DueRepairs("peer-a", 100)
		if err != nil {
			t.Fatal(err)
		}
		// One of peer-a's entries is deferred, so it is not due.
		if len(rows) != 8 {
			t.Fatalf("%s: %d due rows, want 8", what, len(rows))
		}
	}
	assertQueued("before reopen", s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertQueued("after reopen", reopened)

	// And the cap is enforced against the reloaded count.
	reopened.SetRepairLimit(9)
	if err := reopened.EnqueueChange("peer-a", queued("a-new"), "poll"); !errors.Is(err, ErrRepairQueueFull) {
		t.Fatalf("cap was not enforced against the reloaded count: %v", err)
	}
}
