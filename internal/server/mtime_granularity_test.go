package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
)

// A replica on a volume with one-second timestamps holds the source's
// nanoseconds in its index and whole seconds on disk. Compared exactly, it
// never vouched for its own bytes, and no download from it could resume.
func TestCoarseVolumeStillOffersAResumableRange(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, root, "node", nil, Config{}, logger)
	fileops.SetModTimeGranularity(root, time.Second)

	body := []byte("replicated bytes")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(root, "app.apk")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	onDisk := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, onDisk, onDisk); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutFile("app.apk", onDisk.Add(123_456_789).UnixNano(), int64(len(body)), hash, false); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/app.apk", nil))
	if w.Code != http.StatusOK || w.Header().Get("ETag") != strconv.Quote(hash) {
		t.Fatalf("status=%d etag=%q", w.Code, w.Header().Get("ETag"))
	}
}
