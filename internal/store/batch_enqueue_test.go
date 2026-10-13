package store

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

// A page of changes is one transaction: every row lands with its own revision,
// newer state still wins a merge, and the counter tells an operator how many
// rows a replay actually wrote.
func TestEnqueueChangesWritesAPageAtOnce(t *testing.T) {
	s := limitStore(t)
	page := make([]FileMeta, 500)
	for i := range page {
		page[i] = queued(fmt.Sprintf("f-%03d", i))
	}
	written, err := s.EnqueueChanges("peer", page, "polled")
	if err != nil || written != len(page) {
		t.Fatalf("written=%d err=%v", written, err)
	}
	if got := s.RepairEnqueued(); got != int64(len(page)) {
		t.Fatalf("enqueue counter=%d, want %d", got, len(page))
	}
	if pending, _ := s.PendingRepairCount("peer"); pending != int64(len(page)) {
		t.Fatalf("pending=%d, want %d", pending, len(page))
	}
	items, err := s.DueRepairs("peer", 1000)
	if err != nil {
		t.Fatal(err)
	}
	revisions := make(map[int64]bool)
	for _, item := range items {
		if revisions[item.Revision] {
			t.Fatalf("revision %d assigned twice", item.Revision)
		}
		revisions[item.Revision] = true
	}

	// A later single enqueue continues the same revision sequence.
	if err := s.EnqueueChange("peer", queued("late"), "polled"); err != nil {
		t.Fatal(err)
	}
	items, _ = s.DueRepairs("peer", 1000)
	for _, item := range items {
		if item.Name == "late" && revisions[item.Revision] {
			t.Fatalf("revision %d reused after a batch", item.Revision)
		}
	}

	// An older state in a later page must not replace the queued newer one.
	newer := queued("f-000")
	newer.Clock, newer.ModTime = 50, 50
	if _, err := s.EnqueueChanges("peer", []FileMeta{newer}, "polled"); err != nil {
		t.Fatal(err)
	}
	older := queued("f-000")
	if _, err := s.EnqueueChanges("peer", []FileMeta{older}, "polled"); err != nil {
		t.Fatal(err)
	}
	items, _ = s.DueRepairs("peer", 1000)
	for _, item := range items {
		if item.Name == "f-000" && (item.Meta == nil || item.Meta.Clock != 50) {
			t.Fatalf("older state replaced the queued newer one: %+v", item.Meta)
		}
	}
}

// A page that does not fit keeps the rows before the cap and reports the cap,
// so the caller holds its cursor and re-reading the page later merges.
func TestEnqueueChangesStopsAtTheCap(t *testing.T) {
	s := limitStore(t)
	s.SetRepairLimit(10)
	page := make([]FileMeta, 15)
	for i := range page {
		page[i] = queued(fmt.Sprintf("f-%02d", i))
	}
	written, err := s.EnqueueChanges("peer", page, "polled")
	if !errors.Is(err, ErrRepairQueueFull) || written != 10 {
		t.Fatalf("written=%d err=%v, want 10 and a full queue", written, err)
	}
	if pending, _ := s.PendingRepairCount("peer"); pending != 10 {
		t.Fatalf("pending=%d, want 10", pending)
	}
	// Re-reading the same page changes nothing queued and still refuses the rest.
	written, err = s.EnqueueChanges("peer", page, "polled")
	if !errors.Is(err, ErrRepairQueueFull) || written != 0 {
		t.Fatalf("re-read written=%d err=%v", written, err)
	}
	if pending, _ := s.PendingRepairCount("peer"); pending != 10 {
		t.Fatalf("pending=%d after re-read, want 10", pending)
	}
}

func TestRepairLanesSplitLargeTransfers(t *testing.T) {
	s := limitStore(t)
	small := queued("icon.png")
	large := queued("app.apk")
	large.Size = LargeRepairBytes
	gone := queued("old.apk")
	gone.Size, gone.Deleted = 0, true
	if _, err := s.EnqueueChanges("peer", []FileMeta{small, large, gone}, "polled"); err != nil {
		t.Fatal(err)
	}
	lane := func(large bool) []string {
		items, err := s.DueRepairsInLane("peer", large, 10)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, item := range items {
			names = append(names, item.Name)
		}
		return names
	}
	if got := strings.Join(lane(true), ","); got != "app.apk" {
		t.Fatalf("large lane=%q", got)
	}
	if got := len(lane(false)); got != 2 {
		t.Fatalf("small lane holds %d items, want 2", got)
	}
	// A newer state that shrinks the file moves its row.
	shrunk := large
	shrunk.Size, shrunk.Clock, shrunk.ModTime = 10, 5, 5
	if _, err := s.EnqueueChanges("peer", []FileMeta{shrunk}, "polled"); err != nil {
		t.Fatal(err)
	}
	if got := len(lane(true)); got != 0 {
		t.Fatalf("large lane still holds %d items", got)
	}
}

// Rows queued by a release without lanes are sorted into them on upgrade.
func TestRepairLaneMigrationSortsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lanes.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	large := queued("app.apk")
	large.Size = 64 << 20
	if _, err := s.EnqueueChanges("peer", []FileMeta{large, queued("icon.png")}, "polled"); err != nil {
		t.Fatal(err)
	}
	// Model the old schema's state: no lane recorded, no migration marker.
	if _, err := s.db.Exec("UPDATE repair_queue SET large=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM node_meta WHERE key='repair_lanes_v1'"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = New(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items, err := s.DueRepairsInLane("peer", true, 10)
	if err != nil || len(items) != 1 || items[0].Name != "app.apk" {
		t.Fatalf("large lane after migration: %+v %v", items, err)
	}
}

func TestPartialDownloadRecords(t *testing.T) {
	s := limitStore(t)
	if records, err := s.PartialDownloads(); err != nil || len(records) != 0 {
		t.Fatalf("fresh store records=%v err=%v", records, err)
	}
	rec := PartialDownload{Meta: queued("app.apk"), Path: "dir/.birak-tmp-replica-1", Verified: 4096, Prefix: strings.Repeat("b", 64)}
	if err := s.SavePartialDownload(rec); err != nil {
		t.Fatal(err)
	}
	rec.Verified = 8192
	if err := s.SavePartialDownload(rec); err != nil {
		t.Fatal(err)
	}
	records, err := s.PartialDownloads()
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%v err=%v", records, err)
	}
	got := records[0]
	if got.Meta.Name != "app.apk" || got.Path != rec.Path || got.Verified != 8192 || got.Prefix != rec.Prefix || time.Since(got.Updated) > time.Minute {
		t.Fatalf("record round trip: %+v", got)
	}
	if err := s.DropPartialDownload("app.apk"); err != nil {
		t.Fatal(err)
	}
	if records, _ := s.PartialDownloads(); len(records) != 0 {
		t.Fatalf("dropped record survived: %+v", records)
	}

	queuedNames, err := s.QueuedNames([]string{"app.apk"})
	if err != nil || queuedNames["app.apk"] {
		t.Fatalf("queued=%v err=%v", queuedNames, err)
	}
	if err := s.EnqueueChange("peer", queued("app.apk"), "polled"); err != nil {
		t.Fatal(err)
	}
	if queuedNames, err = s.QueuedNames([]string{"app.apk", "other"}); err != nil || !queuedNames["app.apk"] || queuedNames["other"] {
		t.Fatalf("queued=%v err=%v", queuedNames, err)
	}
}

// A pass that finds a queued name again — reconciliation does, every second,
// while a new replica catches up — must not rewrite its row. A new revision
// would keep the worker applying that very state from clearing it.
func TestEnqueueOfAQueuedStateLeavesTheRowAlone(t *testing.T) {
	s := limitStore(t)
	meta := queued("app.apk")
	if _, err := s.EnqueueChanges("peer", []FileMeta{meta}, "polled"); err != nil {
		t.Fatal(err)
	}
	items, _ := s.DueRepairs("peer", 10)
	inFlight := items[0]
	before := s.RepairEnqueued()
	older := meta
	older.Clock, older.ModTime = 0, 0
	for _, again := range []FileMeta{meta, older} {
		if written, err := s.EnqueueChanges("peer", []FileMeta{again}, "reconcile"); err != nil || written != 0 {
			t.Fatalf("written=%d err=%v", written, err)
		}
	}
	if s.RepairEnqueued() != before {
		t.Fatal("unchanged rows were counted as written")
	}
	if err := s.ResolveRepairItem(inFlight); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.PendingRepairCount("peer"); pending != 0 {
		t.Fatal("the worker that applied the queued state could not clear it")
	}
}
