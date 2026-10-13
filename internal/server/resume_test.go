package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// A resumed download names the content it started with in If-Range. A peer
// serves the rest only of exactly that content; anything else gets the whole
// file, and the downloader starts over.
func TestFileRangeIsServedOnlyOfTheIndexedContent(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, root, "node", nil, Config{}, logger)

	body := []byte(strings.Repeat("0123456789", 100))
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(root, "app.apk")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutFile("app.apk", stamp.UnixNano(), int64(len(body)), hash, false); err != nil {
		t.Fatal(err)
	}

	get := func(ifRange string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/files/app.apk", nil)
		if ifRange != "" {
			req.Header.Set("Range", "bytes=600-")
			req.Header.Set("If-Range", ifRange)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	plain := get("")
	if plain.Code != http.StatusOK || plain.Header().Get("ETag") != strconv.Quote(hash) {
		t.Fatalf("plain GET: status=%d etag=%q", plain.Code, plain.Header().Get("ETag"))
	}
	rest := get(strconv.Quote(hash))
	if rest.Code != http.StatusPartialContent || rest.Body.String() != string(body[600:]) ||
		rest.Header().Get("Content-Range") != "bytes 600-999/1000" {
		t.Fatalf("resume of the same content: status=%d range=%q", rest.Code, rest.Header().Get("Content-Range"))
	}
	other := get(strconv.Quote(strings.Repeat("f", 64)))
	if other.Code != http.StatusOK || other.Body.Len() != len(body) {
		t.Fatalf("resume of other content: status=%d len=%d", other.Code, other.Body.Len())
	}

	// Bytes that no longer match the index are not vouched for.
	if err := os.Chtimes(path, stamp.Add(time.Second), stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	unindexed := get(strconv.Quote(hash))
	if unindexed.Code != http.StatusOK || unindexed.Header().Get("ETag") != "" {
		t.Fatalf("unindexed bytes: status=%d etag=%q", unindexed.Code, unindexed.Header().Get("ETag"))
	}
}

type intermittentProvider struct{ metricsProvider }

func (intermittentProvider) ResumedDownloads() (int64, int64) { return 2, 3 << 20 }

func TestMetricsExposeReplayResumeAndSkips(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := watcher.New(root, st, logger, 0, time.Hour, nil)
	// A spelling reserved by history collides on any filesystem.
	if err := os.WriteFile(filepath.Join(root, "photo.png"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRemote(store.FileMeta{Name: "photo.png", Deleted: true, Clock: 1, ModTime: 1, Hash: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "photo.png"), filepath.Join(root, "Photo.png")); err != nil {
		t.Fatal(err)
	}
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-w.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("initial scan did not complete")
	}
	if _, err := st.EnqueueChanges("peer", []store.FileMeta{{Name: "x", Size: 1, Hash: strings.Repeat("b", 64), ModTime: 1}}, "polled"); err != nil {
		t.Fatal(err)
	}
	srv := New(st, root, "node", nil, Config{Stats: intermittentProvider{metricsProvider{w: w}}}, logger)

	out := httptest.NewRecorder()
	srv.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		"birak_local_ready 1",
		`birak_scan_skipped_files{reason="name_collision"} 1`,
		`birak_scan_skipped_files{reason="invalid_name"} 0`,
		`birak_scan_skipped_total{reason="name_collision"} 1`,
		"# TYPE birak_scan_skipped_total counter",
		"birak_repairs_enqueued_total 1",
		"birak_downloads_resumed_total 2",
		"birak_download_resumed_bytes_total 3.145728e+06",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}

	status := httptest.NewRecorder()
	srv.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(status.Body.String(), `"skipped":{"name_collision":1}`) ||
		!strings.Contains(status.Body.String(), `name_collision: \"Photo.png\"`) {
		t.Errorf("/status does not name the skipped file:\n%s", status.Body.String())
	}
}
