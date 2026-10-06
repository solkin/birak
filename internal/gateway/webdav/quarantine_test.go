package webdav

import (
	"errors"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuarantineBlocksGETAndRange(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	if err := os.MkdirAll(filepath.Join(root, "bucket"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "bucket", "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("bucket/file"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("CORRUPT!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("bucket/file"); !errors.Is(err, watcher.ErrIntegrity) {
		t.Fatal(err)
	}
	g := New(root, nil, Config{}, logger)
	for _, ranged := range []bool{false, true} {
		req := httptest.NewRequest("GET", "/bucket/file", nil)
		if ranged {
			req.Header.Set("Range", "bytes=0-3")
		}
		out := httptest.NewRecorder()
		g.server.Handler.ServeHTTP(out, req)
		if out.Code < 400 {
			t.Fatalf("corrupt read succeeded: %d %q", out.Code, out.Body.String())
		}
	}
}
