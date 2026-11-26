package watcher

// The scrub is what actually guarantees stored bytes still match their
// checksums. It runs on a budget instead of a deadline, so these tests pin what
// it finds, that it remembers where it stopped, and that turning it off is a
// deliberate, visible choice rather than an accident.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rewritePreservingAttributes(t *testing.T, path, content string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestScrubQuarantinesBytesTheSweepCannotSee(t *testing.T) {
	w := auditWatcher(t)
	w.SetScrubRate(1 << 30) // verify as fast as the disk allows
	path := filepath.Join(w.dir, "silent.bin")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	rewritePreservingAttributes(t, path, "replaced")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.scrubLoop(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for !w.NeedsRepair("silent.bin") {
		if time.Now().After(deadline) {
			t.Fatalf("scrub did not find a same-attribute rewrite: %+v", w.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The damaged name is counted, and the node keeps serving the rest.
	if status := w.Status(); status.Quarantined != 1 || !status.Ready {
		t.Fatalf("unexpected status after quarantine: %+v", status)
	}
}

func TestScrubRemembersItsPositionAcrossRestarts(t *testing.T) {
	w := auditWatcher(t)
	w.SetScrubRate(1 << 30)
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		if err := os.WriteFile(filepath.Join(w.dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go w.scrubLoop(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for w.Status().LastScrubAgoMS < 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("scrub never completed a cycle: %+v", w.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	// A finished cycle restarts from the beginning, which is what makes the
	// position durable rather than a one-shot marker.
	position, err := w.store.NodeValue(scrubPositionKey)
	if err != nil {
		t.Fatal(err)
	}
	if position != "" {
		t.Fatalf("position after a full cycle = %q, want the start of the tree", position)
	}
}

// Disabling verification must be loud: it leaves silent corruption to be found
// by a peer, or not at all.
func TestScrubDisabledReturnsImmediately(t *testing.T) {
	w := auditWatcher(t)
	w.SetScrubRate(0)
	done := make(chan struct{})
	go func() {
		w.scrubLoop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a disabled scrub kept running")
	}
}

// The budget is what bounds the scrub's cost. A slow rate must actually slow it
// down, or "bytes per second" would be a setting that does nothing.
func TestScrubHonoursItsByteBudget(t *testing.T) {
	w := auditWatcher(t)
	blob := make([]byte, 1<<20)
	if err := os.WriteFile(filepath.Join(w.dir, "one.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 1 MiB at 2 MiB/s owes about half a second before the next name.
	w.SetScrubRate(2 << 20)
	started := time.Now()
	if !w.spendScrubBudget(context.Background(), 1<<20, started) {
		t.Fatal("budget wait was cancelled")
	}
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond {
		t.Fatalf("1 MiB at 2 MiB/s waited only %v", elapsed)
	}
}

func TestStreamingScrubHonorsCancelAndRate(t *testing.T) {
	w := auditWatcher(t)
	blob := make([]byte, 4*scrubChunkBytes) // 256 KiB (> scrubChunkBytes)
	for i := range blob {
		blob[i] = byte(i % 251)
	}
	path := filepath.Join(w.dir, "chunked.bin")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(blob)
	wantHash := hex.EncodeToString(wantSum[:])

	// Fast rate: streaming snapshot completes and matches exact SHA-256.
	w.SetScrubRate(1 << 30)
	info, gotHash, readBytes, err := w.snapshotRateLimited(context.Background(), path, time.Now())
	if err != nil {
		t.Fatalf("snapshotRateLimited fast: %v", err)
	}
	if gotHash != wantHash || readBytes != int64(len(blob)) || info.Size() != int64(len(blob)) {
		t.Fatalf("got hash=%s bytes=%d, want hash=%s bytes=%d", gotHash, readBytes, wantHash, len(blob))
	}

	// Slow rate + cancel: stops promptly mid-file instead of bursting full file.
	w.SetScrubRate(scrubChunkBytes) // 64 KiB/s -> 4 chunks would take 4s
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, _, err = w.snapshotRateLimited(ctx, path, started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled snapshotRateLimited err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled snapshotRateLimited took %v, expected prompt exit", elapsed)
	}
}
