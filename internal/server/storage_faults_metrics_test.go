package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// Held deletions, read errors and unreadable entries are series from the
// start, so an alert on them has something to watch before the first one.
func TestMetricsExposeHeldDeletionsAndReadErrors(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := watcher.New(root, st, logger, 0, 0, nil)
	srv := New(st, root, "node", nil, Config{Stats: metricsProvider{w: w}}, logger)

	out := httptest.NewRecorder()
	srv.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		"# TYPE birak_external_deletions_held gauge",
		"birak_external_deletions_held 0",
		"# TYPE birak_read_errors_total counter",
		"birak_read_errors_total 0",
		`birak_scan_skipped_files{reason="unreadable"} 0`,
		`birak_scan_skipped_total{reason="unreadable"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}
