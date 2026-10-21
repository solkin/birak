package watcher

// A file or directory this process may not read is that entry's problem, not
// the node's. One chmod used to fail every sweep, keep a restarted node from
// ever opening readiness — and with it replication — and, for a directory, make
// startup itself fatal.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lockOut removes every permission from path for the rest of the test.
func lockOut(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not restrict access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads everything regardless of permission bits")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, info.Mode().Perm()) })
}

func assertReadyAndSkipped(t *testing.T, w *Watcher, want int) {
	t.Helper()
	status := w.Status()
	if !status.Ready || status.LastError != "" {
		t.Fatalf("an unreadable entry took the node out of rotation: %+v", status)
	}
	if status.Skipped[SkipUnreadable] != want {
		t.Fatalf("skipped[%s]=%d, want %d: %+v", SkipUnreadable, status.Skipped[SkipUnreadable], want, status)
	}
	select {
	case <-w.Ready():
	default:
		t.Fatal("sweep completed but did not release the syncer")
	}
}

func assertLive(t *testing.T, w *Watcher, names ...string) {
	t.Helper()
	for _, name := range names {
		meta, err := w.store.GetFile(name)
		if err != nil || meta == nil || meta.Deleted {
			t.Fatalf("%s advertised as deleted or lost: %+v %v", name, meta, err)
		}
	}
}

func TestUnreadableFileIsSkippedAndNodeStaysReady(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "secret.bin")
	if err := os.WriteFile(path, []byte("not for this process"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockOut(t, path, 0)

	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatalf("one unreadable file failed the sweep: %v", err)
	}
	assertReadyAndSkipped(t, w, 1)
	if want := SkipUnreadable + ": " + strconv.Quote("secret.bin"); len(w.Status().SkippedSamples) != 1 || w.Status().SkippedSamples[0] != want {
		t.Fatalf("samples=%q, want %q", w.Status().SkippedSamples, want)
	}
	if meta, err := w.store.GetFile("secret.bin"); err != nil || meta != nil {
		t.Fatalf("unreadable bytes reached the index: %+v %v", meta, err)
	}

	// The event path skips it the same way.
	w.processBatch(context.Background(), []string{"secret.bin"})
	if status := w.Status(); status.LastError != "" || status.SkippedTotal[SkipUnreadable] != 2 {
		t.Fatalf("event batch: %+v", status)
	}

	// A restarted process completes its initial scan, so replication starts.
	restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
	if err := restarted.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertReadyAndSkipped(t, restarted, 1)
}

// A directory without read and search permission cannot be listed, and the
// files indexed below it cannot even be stat'ed. That is not their deletion.
func TestUnsearchableDirectoryIsSkippedNotDeleted(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"no access", 0},
		{"listable but not searchable", 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := auditWatcher(t)
			dir := filepath.Join(w.dir, "locked")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a.bin", "b.bin"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.periodicScan(context.Background()); err != nil {
				t.Fatal(err)
			}
			lockOut(t, dir, tc.mode)

			if err := w.periodicScan(context.Background()); err != nil {
				t.Fatalf("an unreadable directory failed the sweep: %v", err)
			}
			status := w.Status()
			if !status.Ready || status.LastError != "" || status.Skipped[SkipUnreadable] == 0 {
				t.Fatalf("status: %+v", status)
			}
			assertLive(t, w, "locked/a.bin", "locked/b.bin")

			// The event path never infers a deletion from it either.
			w.processBatch(context.Background(), []string{"locked/a.bin"})
			if status := w.Status(); status.LastError != "" || status.HeldExternalDeletions != 0 {
				t.Fatalf("event batch: %+v", status)
			}
			assertLive(t, w, "locked/a.bin")

			// Nor does a restarted node, which still becomes ready.
			restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
			if err := restarted.CheckStorage(); err != nil {
				t.Fatalf("startup validation: %v", err)
			}
			if err := restarted.periodicScan(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-restarted.Ready():
			default:
				t.Fatal("restarted node never released the syncer")
			}
			assertLive(t, restarted, "locked/a.bin", "locked/b.bin")
		})
	}
}

// Startup validation walks the tree for unmapped backups. Refusing to start
// over one unreadable directory put the container into a restart loop.
func TestStartupSurvivesAnUnreadableDirectory(t *testing.T) {
	w := auditWatcher(t)
	dir := filepath.Join(w.dir, "locked")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inside.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockOut(t, dir, 0)
	if err := w.CheckStorage(); err != nil {
		t.Fatalf("startup failed over an unreadable directory: %v", err)
	}
}

// The root is different: a sync root this process cannot list is a storage
// fault, not one entry to skip.
func TestUnreadableRootIsStillAFault(t *testing.T) {
	w := auditWatcher(t)
	if err := os.WriteFile(filepath.Join(w.dir, "file.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	lockOut(t, w.dir, 0o300)
	err := w.periodicScan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("an unreadable root was not a fault: %v", err)
	}
	if status := w.Status(); status.Ready || status.LastError == "" {
		t.Fatalf("an unreadable root left the node ready: %+v", status)
	}
	assertLive(t, w, "file.bin")
}
