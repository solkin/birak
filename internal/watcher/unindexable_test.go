package watcher

// One file the node cannot index is that file's problem, not the node's. It
// used to fail every sweep: the node went unready the moment it appeared, and
// after a restart the syncer waited for an initial scan that never completed,
// so replication stopped too.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
)

func assertSkippedCollision(t *testing.T, w *Watcher, name string) {
	t.Helper()
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatalf("one colliding name failed the sweep: %v", err)
	}
	status := w.Status()
	if !status.Ready || status.LastError != "" {
		t.Fatalf("one colliding name took the node out of rotation: %+v", status)
	}
	if status.Skipped[SkipNameCollision] != 1 || status.SkippedTotal[SkipNameCollision] < 1 {
		t.Fatalf("collision not counted: %+v", status)
	}
	if want := SkipNameCollision + ": " + strconv.Quote(name); len(status.SkippedSamples) != 1 || status.SkippedSamples[0] != want {
		t.Fatalf("samples=%q, want %q", status.SkippedSamples, want)
	}
	if meta, err := w.store.GetFile(name); err != nil || meta != nil {
		t.Fatalf("colliding spelling reached the index: %+v %v", meta, err)
	}
	select {
	case <-w.Ready():
	default:
		t.Fatal("sweep completed but did not release the syncer")
	}
}

// The production case: 0.PNG appears next to an indexed 0.png. This needs a
// filesystem that can hold both.
func TestCaseCollidingFileLeavesNodeReady(t *testing.T) {
	w := auditWatcher(t)
	if err := os.WriteFile(filepath.Join(w.dir, "0.png"), []byte("indexed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "0.PNG"), []byte("stray"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(w.dir, "0.png")); string(data) != "indexed" {
		t.Skip("case-insensitive filesystem cannot hold both spellings")
	}
	assertSkippedCollision(t, w, "0.PNG")

	// The event path skips it the same way.
	w.processBatch(context.Background(), []string{"0.PNG"})
	if status := w.Status(); status.LastError != "" || status.SkippedTotal[SkipNameCollision] != 2 {
		t.Fatalf("event batch: %+v", status)
	}
	// The indexed spelling is untouched.
	if meta, err := w.store.GetFile("0.png"); err != nil || meta == nil || meta.Deleted {
		t.Fatalf("indexed spelling lost: %+v %v", meta, err)
	}
}

// A spelling reserved by deletion history collides on every filesystem.
func TestSpellingReservedByHistoryLeavesNodeReadyAcrossRestart(t *testing.T) {
	w := auditWatcher(t)
	original := filepath.Join(w.dir, "photo.png")
	if err := os.WriteFile(original, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "Photo.png"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertSkippedCollision(t, w, "Photo.png")

	// A restarted process completes its initial scan, so replication starts.
	restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
	assertSkippedCollision(t, restarted, "Photo.png")
}

func TestInvalidUTF8NameLeavesNodeReady(t *testing.T) {
	w := auditWatcher(t)
	name := "bad-\xff.png"
	if err := os.WriteFile(filepath.Join(w.dir, name), []byte("x"), 0o644); err != nil {
		t.Skipf("filesystem refuses invalid UTF-8 names: %v", err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatalf("an invalid name failed the sweep: %v", err)
	}
	status := w.Status()
	if !status.Ready || status.Skipped[SkipInvalidName] != 1 {
		t.Fatalf("invalid name: %+v", status)
	}
}

func TestSkipClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("x: %w", store.ErrNameCollision), SkipNameCollision},
		{fmt.Errorf("x: %w", fileops.ErrSpellingAlias), SkipNameCollision},
		{fmt.Errorf("x: %w", store.ErrInvalidName), SkipInvalidName},
		{errors.New("indexed file was replaced by a non-regular file"), ""},
		{&fs.PathError{Op: "open", Path: "x", Err: syscall.EACCES}, SkipUnreadable},
		{&fs.PathError{Op: "lstat", Path: "x", Err: syscall.EPERM}, SkipUnreadable},
		{fmt.Errorf("%w: %w", errUnreadable, syscall.EIO), SkipUnreadable},
		// The volume failing is never one name's problem.
		{&storageFault{&fs.PathError{Op: "open", Path: "x", Err: syscall.EACCES}}, ""},
		{&fs.PathError{Op: "read", Path: "x", Err: syscall.EIO}, ""},
	} {
		if got := unindexable(tc.err); got != tc.want {
			t.Errorf("unindexable(%v)=%q, want %q", tc.err, got, tc.want)
		}
	}
	if !vanished(&fs.PathError{Op: "readdir", Path: "x", Err: syscall.ENOENT}) ||
		!vanished(&fs.PathError{Op: "readdir", Path: "x", Err: syscall.ENOTDIR}) ||
		vanished(&fs.PathError{Op: "readdir", Path: "x", Err: syscall.EACCES}) {
		t.Fatal("vanished misclassifies walk errors")
	}
}

// --- the processing loop ----------------------------------------------------

func startProcessLoop(t *testing.T, w *Watcher) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.processLoop(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-w.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("initial scan did not complete")
	}
	return cancel, done
}

func writeTree(t *testing.T, root string, count int) []string {
	t.Helper()
	names := make([]string, 0, count)
	for i := range count {
		name := fmt.Sprintf("d%03d/f%05d.bin", i/100, i)
		path := filepath.Join(root, filepath.FromSlash(name))
		if i%100 == 0 {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

// Indexing a bulk upload takes longer than the readiness window. A sweep that
// comes due must run while it does, and leave the backlog's names to it.
func TestLongEventBatchDoesNotStarveTheSweep(t *testing.T) {
	w := auditWatcher(t)
	ticks := make(chan time.Time)
	w.scanTicks = ticks
	startProcessLoop(t, w)
	names := writeTree(t, w.dir, 400)

	w.statusMu.Lock()
	initial := w.lastScan
	w.statusMu.Unlock()
	w.work <- names
	awaitIndexed(t, w, 20)
	// The sweep comes due with most of the backlog still to index.
	go func() { ticks <- time.Now() }()

	var sweptAt int64 = -1
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		w.statusMu.Lock()
		last := w.lastScan
		w.statusMu.Unlock()
		count, err := w.store.FileCount()
		if err != nil {
			t.Fatal(err)
		}
		if sweptAt < 0 && last.After(initial) {
			sweptAt = count
		}
		if count == int64(len(names)) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if count, _ := w.store.FileCount(); count != int64(len(names)) {
		t.Fatalf("backlog indexed %d of %d names", count, len(names))
	}
	if sweptAt < 0 || sweptAt == int64(len(names)) {
		t.Fatalf("the sweep waited for the backlog (index held %d of %d names when it completed)", sweptAt, len(names))
	}
	if status := w.Status(); !status.Ready || status.LastError != "" {
		t.Fatalf("node unready after the backlog: %+v", status)
	}
}

// SIGTERM must not wait for tens of thousands of queued names to be indexed.
// Whatever is left is found by the next start's initial scan, like anything
// else that changed while the daemon was down.
func TestShutdownDoesNotWaitForTheBacklog(t *testing.T) {
	w := auditWatcher(t)
	cancel, done := startProcessLoop(t, w)
	names := writeTree(t, w.dir, 50_000)
	w.work <- names
	awaitIndexed(t, w, 10)

	stopped := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("processing loop did not stop within 10s of cancellation")
	}
	if took := time.Since(stopped); took > 3*time.Second {
		t.Fatalf("shutdown waited %v for the backlog", took)
	}
	if count, _ := w.store.FileCount(); count >= int64(len(names)) {
		t.Fatalf("the whole backlog was indexed (%d) before stopping", count)
	}
}

func TestProcessBatchStopsWhenCancelled(t *testing.T) {
	w := auditWatcher(t)
	names := writeTree(t, w.dir, 50)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.processBatch(ctx, names)
	if count, _ := w.store.FileCount(); count != 0 {
		t.Fatalf("a cancelled batch indexed %d names", count)
	}
}

func awaitIndexed(t *testing.T, w *Watcher, atLeast int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if count, _ := w.store.FileCount(); count >= atLeast {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fewer than %d names indexed", atLeast)
}

func TestBacklogKeepsOrderAndDeduplicates(t *testing.T) {
	b := newBacklog()
	b.add([]string{"a", "b", "a"})
	b.add([]string{"c", "b"})
	var got []string
	for b.len() > 0 {
		name := b.next()
		if b.has(name) {
			t.Fatalf("%s still queued after being taken", name)
		}
		got = append(got, name)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("order %v", got)
	}
	// A name taken can be queued again.
	b.add([]string{"a"})
	if !b.has("a") || b.len() != 1 {
		t.Fatal("re-queued name lost")
	}
	// Compaction keeps the remaining order.
	many := make([]string, 10_000)
	for i := range many {
		many[i] = fmt.Sprint(i)
	}
	b = newBacklog()
	b.add(many)
	for i := range 6000 {
		if got := b.next(); got != many[i] {
			t.Fatalf("next=%s, want %s", got, many[i])
		}
	}
	if b.len() != 4000 || b.next() != many[6000] {
		t.Fatal("compaction lost the order")
	}
}
