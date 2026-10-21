package watcher

// Rebuilding the index is safe, and on a large tree it is long. Neither may
// happen silently: a lost metadata volume takes deletion history with it, and
// an hour-long first index must not look like a hang.

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
)

// syncBuffer is a log sink safe to read while the watcher writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func loggingWatcher(t *testing.T, dir string) (*Watcher, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "meta.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(dir, st, logger, time.Millisecond, time.Hour, nil), logs
}

const metadataLossWarning = "metadata database is new but the data volume was bound to an earlier one"

func TestNewMetadataOnABoundVolumeIsReported(t *testing.T) {
	dir := t.TempDir()
	first, firstLogs := loggingWatcher(t, dir)
	if err := first.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(firstLogs.String(), metadataLossWarning) {
		t.Fatal("binding a fresh volume was reported as metadata loss")
	}

	// The metadata volume is lost; the data volume is not.
	replaced, logs := loggingWatcher(t, dir)
	if err := replaced.CheckStorage(); err != nil {
		t.Fatalf("a new database must still adopt its volume: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, metadataLossWarning) || !strings.Contains(out, "tombstones") {
		t.Fatalf("metadata loss was not reported:\n%s", out)
	}
}

func TestLongScanReportsProgress(t *testing.T) {
	w, logs := loggingWatcher(t, t.TempDir())
	writeFiles(t, w.dir, "a/1", "a/2", "b/3")
	w.progressInterval = time.Nanosecond
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{
		`level=INFO msg="scan in progress" phase=walk`,
		`phase="deletion check"`,
		"files_seen=3",
		"indexed=3",
		`msg="scan finished" files_seen=3 indexed=3 checked=3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// A scan that finishes quickly says nothing.
	quiet, quietLogs := loggingWatcher(t, t.TempDir())
	if err := quiet.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(quietLogs.String(), "scan in progress") {
		t.Fatal("a short scan reported progress")
	}
}
