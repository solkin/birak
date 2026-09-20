package watcher

// An upload in flight is an ordinary, self-resolving state, not a storage
// fault. Treating it as one made a node with live write traffic report itself
// unready, which pulls it out of a load balancer exactly while it is in use.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/fileops"
)

func TestScanSkipsFileHeldByAWriterAndStaysReady(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "existing.bin")
	if err := os.WriteFile(path, []byte("published"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w.Status().Ready {
		t.Fatalf("baseline scan not ready: %+v", w.Status())
	}

	// A gateway is overwriting the file: staged, not yet published.
	f, commit, err := fileops.OpenWriter(w.dir, path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("in flight")); err != nil {
		t.Fatal(err)
	}

	scanErr := w.periodicScan(context.Background())
	status := w.Status()
	if err := commit(); err != nil {
		t.Fatal(err)
	}

	if scanErr != nil {
		t.Fatalf("scan reported an in-flight upload as an incomplete scan: %v", scanErr)
	}
	if !status.Ready || status.LastError != "" {
		t.Fatalf("node reported unready while a write was in flight: %+v", status)
	}

	// The writer's own commit indexes the result, so nothing is skipped for good.
	meta, err := w.store.GetFile("existing.bin")
	if err != nil || meta == nil || meta.Deleted || meta.Size != int64(len("in flight")) {
		t.Fatalf("published bytes were not indexed by the writer: %+v, %v", meta, err)
	}
}

func TestEventBatchForAFileBeingWrittenDoesNotFailReadiness(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "existing.bin")
	if err := os.WriteFile(path, []byte("published"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	f, commit, err := fileops.OpenWriter(w.dir, path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("in flight"))

	// A debounced batch names the file the writer currently holds.
	w.processBatch([]string{"existing.bin"})
	status := w.Status()
	if err := commit(); err != nil {
		t.Fatal(err)
	}

	if !status.Ready || status.LastError != "" {
		t.Fatalf("an event for a file being written marked the node faulty: %+v", status)
	}
}

// A genuine fault must still close readiness: the skip above is scoped to a
// writer this process owns, not to unreadable or unsupported entries.
func TestGenuineScanFaultStillFailsReadiness(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "conflict")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err == nil || w.Status().Ready {
		t.Fatal("a real storage fault was reported as a healthy scan")
	}
}
