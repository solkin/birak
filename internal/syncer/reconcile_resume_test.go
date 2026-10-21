package syncer

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// A catching-up replica fills its repair queue on every pass. Each pass used
// to start the manifest from the beginning again, re-reading the same pages
// over a slow link every second and never getting further.
func TestReconcileResumesAfterAFullQueue(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	names := make([]store.FileMeta, 2500)
	for i := range names {
		names[i] = auditMeta(fmt.Sprintf("f%05d", i), "body "+strconv.Itoa(i), 100)
	}
	var mu sync.Mutex
	var asked []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderNodeID, "source")
		after := r.URL.Query().Get("after")
		mu.Lock()
		asked = append(asked, after)
		mu.Unlock()
		page := []store.FileMeta{}
		for _, meta := range names {
			if meta.Name > after && len(page) < manifestPageSize {
				page = append(page, meta)
			}
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer peer.Close()

	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetRepairLimit(1500)
	root := t.TempDir()
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	node := New(st, w, root, "replica", []string{peer.URL}, nil, logger, Options{PollInterval: time.Second, BatchLimit: 100, MaxConcurrentDownloads: 1})
	defer node.client.CloseIdleConnections()

	if _, err := node.reconcileOnce(t.Context(), peer.URL); err == nil {
		t.Fatal("expected the repair queue to fill")
	}
	mu.Lock()
	asked = nil
	mu.Unlock()
	node.reconcileOnce(t.Context(), peer.URL)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 || asked[0] != names[manifestPageSize-1].Name {
		t.Fatalf("second pass started at %q; want after the first complete page %q", asked, names[manifestPageSize-1].Name)
	}
}
