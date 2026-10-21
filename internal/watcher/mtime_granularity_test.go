package watcher

// A replica whose filesystem keeps coarser timestamps than its source never
// matched its own index exactly: every sweep re-read every file and stamped
// the source's nanoseconds onto it again, which the filesystem then dropped
// again.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

// replicaOnCoarseVolume indexes name the way a replica applied from a
// nanosecond source lands on a one-second volume: the index has the source's
// time, the disk has it truncated.
func replicaOnCoarseVolume(t *testing.T, w *Watcher, name, body string) (string, time.Time) {
	t.Helper()
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	fileops.SetModTimeGranularity(w.dir, time.Second)
	path := filepath.Join(w.dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	onDisk := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, onDisk, onDisk); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	if _, err := w.store.PutFile(name, onDisk.Add(123_456_789).UnixNano(), int64(len(body)), hex.EncodeToString(sum[:]), false); err != nil {
		t.Fatal(err)
	}
	return path, onDisk
}

func TestCoarseVolumeIsNotRestampedEverySweep(t *testing.T) {
	w := auditWatcher(t)
	path, onDisk := replicaOnCoarseVolume(t, w, "replica.bin", "same bytes")
	before, err := w.store.GetFile("replica.bin")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := w.periodicScan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.restamped.Load(); n != 0 {
		t.Fatalf("restamped %d unchanged files on a coarse volume", n)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(onDisk) {
		t.Fatalf("the file was touched: %v %v", info.ModTime(), err)
	}
	if after, err := w.store.GetFile("replica.bin"); err != nil || after.Version != before.Version {
		t.Fatalf("an unchanged file got a new version: %+v %v", after, err)
	}
	if !w.unchangedByStat("replica.bin") {
		t.Fatal("a commit would re-read an unchanged file")
	}
}

// Coarser comparison must not blind the node to a rewrite that keeps size and
// timestamp: that is still quarantined.
func TestCoarseVolumeStillCatchesARewriteWithTheSameStamp(t *testing.T) {
	w := auditWatcher(t)
	path, onDisk := replicaOnCoarseVolume(t, w, "replica.bin", "same bytes")
	if err := os.WriteFile(path, []byte("evil bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, onDisk, onDisk); err != nil {
		t.Fatal(err)
	}
	if err := w.scanFile("replica.bin"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("a same-stamp rewrite was not quarantined: %v", err)
	}
}

// The granularity is measured once, when the volume is first validated.
func TestVolumeGranularityIsMeasuredAtBinding(t *testing.T) {
	w := auditWatcher(t)
	fileops.SetModTimeGranularity(w.dir, 2*time.Second)
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	measured := fileops.ModTimeGranularity(w.dir)
	if measured >= 2*time.Second {
		t.Fatalf("binding did not measure the volume: %v", measured)
	}
	if _, err := os.Lstat(filepath.Join(w.dir, ".birak", "mtime-probe")); !os.IsNotExist(err) {
		t.Fatalf("probe left behind: %v", err)
	}
}
