package watcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
)

func auditWatcher(t *testing.T) *Watcher {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "watcher.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(t.TempDir(), st, logger, time.Millisecond, time.Hour, nil)
}

func TestIndexedFileReplacedByDirectoryMakesScanUnready(t *testing.T) {
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
		t.Fatal("unsupported path conflict was reported as a healthy scan")
	}
	meta, err := w.store.GetFile("conflict")
	if err != nil || meta == nil || meta.Deleted {
		t.Fatalf("path conflict must not broadcast deletion: %+v, %v", meta, err)
	}
}

func TestAuditUnreadableDirectoryDoesNotBroadcastDeletion(t *testing.T) {
	w := auditWatcher(t)
	parent := filepath.Join(w.dir, "unreadable")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "valuable")
	if err := os.WriteFile(path, []byte("do not delete on every peer"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.periodicScan(context.Background())
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	if _, err := os.ReadDir(parent); !errors.Is(err, os.ErrPermission) {
		t.Skip("requires an unprivileged user to enforce directory permissions")
	}
	w.periodicScan(context.Background())
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	meta, err := w.store.GetFile("unreadable/valuable")
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil || meta.Deleted {
		t.Fatalf("existing file advertised as deleted after a read error: %+v", meta)
	}
}

func TestAuditMissingRootDoesNotBroadcastDeletion(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "valuable")
	if err := os.WriteFile(path, []byte("data on temporarily unavailable storage"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.periodicScan(context.Background())
	// Move the whole root aside, modelling an unavailable storage root. Restore
	// it for cleanup; no actual user data or mount is touched.
	away := w.dir + "-unavailable"
	if err := os.Rename(w.dir, away); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(away, w.dir)
	w.periodicScan(context.Background())
	meta, err := w.store.GetFile("valuable")
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil || meta.Deleted {
		t.Fatalf("unavailable sync root advertised as file deletion: %+v", meta)
	}
}

func TestAuditScanQuarantinesRewriteWithPreservedAttributes(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "restored")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.periodicScan(context.Background())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same-size replacement with preserved mtime, as with an offline restore.
	if err := os.WriteFile(path, []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	// The stat-only sweep deliberately cannot see this: size and timestamp are
	// unchanged. Re-reading bytes is the scrub's budgeted job, and scanFile is
	// the step it performs per name.
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatalf("sweep should ignore an unchanged size and timestamp: %v", err)
	}
	if w.NeedsRepair("restored") {
		t.Fatal("the cheap sweep claimed to have verified bytes it never read")
	}
	if err := w.scanFile("restored"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("scrub did not quarantine a same-attribute rewrite: %v", err)
	}
	want, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := w.store.GetFile("restored")
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil || meta.Hash == want || !w.NeedsRepair("restored") {
		t.Fatalf("unexplained same-attribute rewrite was not quarantined: indexed=%+v diskHash=%s status=%+v", meta, want, w.Status())
	}
	// The damaged name is unavailable and counted, but the node keeps serving:
	// readiness answers for the process and the volume, not for one file. When
	// no peer holds a healthy copy, the old contract removed this node from
	// service permanently.
	status := w.Status()
	if status.Quarantined != 1 || !status.Ready || status.LastError != "" {
		t.Fatalf("one damaged file took the whole node out of rotation: %+v", status)
	}
}

func TestAuditSameContentNewMtimeUpdatesConflictClock(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "same-content")
	if err := os.WriteFile(path, []byte("same bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := time.Unix(100, 0)
	if err := os.Chtimes(path, before, before); err != nil {
		t.Fatal(err)
	}
	w.periodicScan(context.Background())
	after := time.Unix(300, 0)
	if err := os.Chtimes(path, after, after); err != nil {
		t.Fatal(err)
	}
	w.processBatch([]string{"same-content"})
	w.periodicScan(context.Background())
	meta, err := w.store.GetFile("same-content")
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil || meta.ModTime != after.UnixNano() {
		t.Fatalf("new write time lost despite mtime being conflict clock: %+v", meta)
	}
}

func TestFailedInitialScanDoesNotOpenReadiness(t *testing.T) {
	w := auditWatcher(t)
	parent := filepath.Join(w.dir, "blocked")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0o700)
	if _, err := os.ReadDir(parent); !errors.Is(err, os.ErrPermission) {
		t.Skip("requires unprivileged user")
	}
	if err := w.periodicScan(context.Background()); err == nil {
		t.Fatal("incomplete scan succeeded")
	}
	select {
	case <-w.Ready():
		t.Fatal("failed initial scan released syncer")
	default:
	}
	if w.Status().Ready || w.Status().LastError == "" {
		t.Fatal("failed scan reported healthy")
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.Ready():
	default:
		t.Fatal("recovered scan did not release syncer")
	}
	if !w.Status().Ready {
		t.Fatal("recovered scan still unhealthy")
	}
}

func TestReplacementRootWithExistingMetadataFailsClosed(t *testing.T) {
	w := auditWatcher(t)
	if err := os.WriteFile(filepath.Join(w.dir, "valuable"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	away := w.dir + "-saved"
	if err := os.Rename(w.dir, away); err != nil {
		t.Fatal(err)
	}
	defer func() { os.RemoveAll(w.dir); os.Rename(away, w.dir) }()
	if err := os.Mkdir(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A fresh watcher must recover the binding from SQLite, not trust process memory.
	restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
	if err := restarted.periodicScan(context.Background()); err == nil {
		t.Fatal("replacement empty root was trusted")
	}
	meta, err := w.store.GetFile("valuable")
	if err != nil || meta == nil || meta.Deleted {
		t.Fatalf("replacement root broadcast deletion: %+v %v", meta, err)
	}
}
