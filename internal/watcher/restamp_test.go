package watcher

// Restoring unchanged bytes with lost timestamps retains their recorded rank
// and repairs the on-disk timestamps, before replication starts.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func restampWatcher(t *testing.T) (*Watcher, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	w := auditWatcher(t)
	w.logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return w, &logged
}

func TestRestoreWithoutTimestampsIsCorrectedAndReported(t *testing.T) {
	w, logged := restampWatcher(t)
	for i := range restampThreshold + 2 {
		path := filepath.Join(w.dir, "file-"+string(rune('a'+i))+".bin")
		if err := os.WriteFile(path, []byte("stable contents"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "modification times") {
		t.Fatalf("warned about a tree it had only just indexed:\n%s", logged.String())
	}
	before := make(map[string]int64)
	versions := make(map[string]int64)
	clocks := make(map[string]int64)
	for i := range restampThreshold + 2 {
		name := "file-" + string(rune('a'+i)) + ".bin"
		meta, err := w.store.GetFile(name)
		if err != nil || meta == nil {
			t.Fatalf("indexed %s: %v", name, err)
		}
		before[name], versions[name], clocks[name] = meta.ModTime, meta.Version, meta.Clock
	}

	// Restore: same bytes, new timestamps — what `cp -R` leaves behind.
	stamp := time.Now().Add(time.Hour)
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Chtimes(filepath.Join(w.dir, entry.Name()), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	// A fresh watcher on the same volume: this is a restart after the restore.
	restarted := New(w.dir, w.store, slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelWarn})), time.Millisecond, time.Hour, nil)
	if err := restarted.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, stamp := range before {
		meta, err := w.store.GetFile(name)
		if err != nil || meta == nil {
			t.Fatalf("restored %s: %v", name, err)
		}
		info, err := os.Stat(filepath.Join(w.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if meta.ModTime != stamp || meta.Version != versions[name] || meta.Clock != clocks[name] || info.ModTime().UnixNano() != stamp {
			t.Fatalf("restore promoted unchanged %s or failed to repair its timestamp: %+v; disk=%d", name, meta, info.ModTime().UnixNano())
		}
	}
	out := logged.String()
	for _, want := range []string{
		"restored modification times for unchanged indexed files",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("a restore without timestamps went unreported (missing %q):\n%s", want, out)
		}
	}
}

// One person running touch is not a restore, and must not cry wolf.
func TestATouchedFileIsNotReportedAsARestore(t *testing.T) {
	w, logged := restampWatcher(t)
	for i := range restampThreshold + 2 {
		path := filepath.Join(w.dir, "file-"+string(rune('a'+i))+".bin")
		if err := os.WriteFile(path, []byte("stable contents"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	stamp := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(w.dir, "file-a.bin"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	restarted := New(w.dir, w.store, slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelWarn})), time.Millisecond, time.Hour, nil)
	if err := restarted.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "modification times") {
		t.Fatalf("a single touched file was reported as a restore:\n%s", logged.String())
	}
}
