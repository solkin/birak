package watcher

// A disk that cannot read a file back has damaged it as surely as one that
// returns other bytes. The scrub used to log that and move on, leaving the
// node serving the file — and the sweep failed the whole node on it.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

// failReads makes every read of path fail the way a bad sector does.
func failReads(t *testing.T, path string) {
	t.Helper()
	previous := snapshotFile
	snapshotFile = func(p string) (os.FileInfo, string, error) {
		if p == path {
			return nil, "", &fs.PathError{Op: "read", Path: p, Err: syscall.EIO}
		}
		return previous(p)
	}
	t.Cleanup(func() { snapshotFile = previous })
}

func TestReadErrorQuarantinesAndQueuesRepair(t *testing.T) {
	w := auditWatcher(t)
	w.SetRepairPeers([]string{"http://peer"})
	w.SetScrubRate(1 << 30)
	writeFiles(t, w.dir, "bad.bin", "good.bin")
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	failReads(t, filepath.Join(w.dir, "bad.bin"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.scrubLoop(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !w.NeedsRepair("bad.bin") {
		if time.Now().After(deadline) {
			t.Fatalf("scrub did not quarantine an unreadable file: %+v", w.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	status := w.Status()
	if status.Quarantined != 1 || status.ReadErrors < 1 || !status.Ready {
		t.Fatalf("status after a read error: %+v", status)
	}
	if queued, err := w.store.PendingRepairCount("http://peer"); err != nil || queued != 1 {
		t.Fatalf("repair from the peer not queued: %d %v", queued, err)
	}
	// It is not served meanwhile.
	if _, err := fileops.OpenReader(w.dir, filepath.Join(w.dir, "bad.bin")); !errors.Is(err, fileops.ErrUnreadable) {
		t.Fatalf("an unreadable file is still served: %v", err)
	}
	assertLive(t, w, "bad.bin", "good.bin")
}

// The sweep reads a file whose timestamp moved. An I/O error there is the
// same damaged copy, not a reason to take the node out of rotation.
func TestReadErrorDuringSweepKeepsTheNodeReady(t *testing.T) {
	w := auditWatcher(t)
	writeFiles(t, w.dir, "bad.bin")
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.dir, "bad.bin")
	if err := os.Chtimes(path, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	failReads(t, path)
	if err := w.periodicScan(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("sweep over an unreadable file: %v", err)
	}
	status := w.Status()
	if !status.Ready || status.Quarantined != 1 || status.ReadErrors < 1 {
		t.Fatalf("status: %+v", status)
	}

	// A file nobody indexed yet has nothing to repair from; it is skipped.
	writeFiles(t, w.dir, "new.bin")
	failReads(t, filepath.Join(w.dir, "new.bin"))
	w.processBatch(context.Background(), []string{"new.bin"})
	if status := w.Status(); status.LastError != "" || status.SkippedTotal[SkipUnreadable] != 1 {
		t.Fatalf("unindexed unreadable file: %+v", status)
	}
}
