package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/birak/birak/internal/store"
)

func TestClusterResponsesAreNotCacheable(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.New(filepath.Join(t.TempDir(), "metadata.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutFile("file", 100, 5, strings.Repeat("a", 64), false); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(New(db, root, "source", nil, Config{Secret: "cluster-secret"}, logger).Handler())
	defer peer.Close()
	for _, path := range []string{"/changes?since=0", "/manifest", "/meta/file", "/files/file", "/files/missing", "/status"} {
		for _, authenticated := range []bool{true, false} {
			req, err := http.NewRequest(http.MethodGet, peer.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if authenticated {
				req.Header.Set(HeaderSecret, "cluster-secret")
			}
			resp, err := peer.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
				t.Errorf("cacheable cluster response: path=%s authenticated=%v status=%d", path, authenticated, resp.StatusCode)
			}
		}
	}
}
