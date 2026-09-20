package watcher

// A backup restored with a copy that dropped modification times is the quietest
// way to corrupt this cluster: every restored file is indexed as a brand-new
// local write, outranks whatever the peers hold, and overwrites it — deletions
// included. Nothing about it looks like an error. The least this node can do is
// say so out loud, once, while there is still time to restore it properly.

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

func TestRestoreWithoutTimestampsIsReported(t *testing.T) {
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
	out := logged.String()
	for _, want := range []string{
		"unchanged contents but new modification times",
		"did not preserve timestamps",
		"cp -a",
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
