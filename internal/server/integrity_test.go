package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

func TestQuarantinedFileCannotBeDownloaded(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("file"); err != nil {
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
	if err := w.Refresh("file"); !errors.Is(err, watcher.ErrIntegrity) {
		t.Fatalf("not quarantined: %v", err)
	}
	if !w.NeedsRepair("file") {
		t.Fatal("quarantine was not recorded")
	}
	srv := New(st, root, "node", nil, Config{Secret: "synthetic-key", Stats: readinessProvider{w}}, logger)
	req := httptest.NewRequest(http.MethodGet, "/files/file", nil)
	req.Header.Set(HeaderSecret, "synthetic-key")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, req)
	if response.Code == http.StatusOK {
		t.Fatalf("known corrupt bytes served successfully: status=%d body=%q quarantine=%d", response.Code, response.Body.String(), w.Status().Quarantined)
	}
}
