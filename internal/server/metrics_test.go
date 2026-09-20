package server

// What you cannot measure you cannot run. These tests pin the series an
// operator has to alert on, and the fact that metrics are no less protected
// than the /status they are derived from.

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

type metricsProvider struct {
	w       *watcher.Watcher
	peers   []PeerStatus
	skipped int64
}

func (p metricsProvider) PeerStats() []PeerStatus        { return p.peers }
func (p metricsProvider) ScanStatus() watcher.ScanStatus { return p.w.Status() }
func (p metricsProvider) CheckStorage() error            { return p.w.CheckStorage() }
func (p metricsProvider) SkippedEntries() int64          { return p.skipped }

func TestMetricsExposeWhatOperatorsAlertOn(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := watcher.New(root, st, logger, 0, 0, nil)
	stats := metricsProvider{
		w:       w,
		skipped: 7,
		peers: []PeerStatus{{
			Peer: `http://peer-1:9100/"odd"`, Lag: 42, Healthy: false,
			ConsecutiveErrs: 3, Pending: 11, LastSuccessAgo: 5000, LastReconcileMS: 60000,
		}},
	}
	srv := New(st, root, "node", nil, Config{Stats: stats}, logger)

	out := httptest.NewRecorder()
	srv.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if out.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d", out.Code)
	}
	body := out.Body.String()

	for _, want := range []string{
		"birak_repairs_queued 0",
		"birak_repair_oldest_seconds 0",
		"birak_quarantined_files 0",
		"birak_skipped_entries_total 7",
		"birak_peer_lag{peer=",
		"birak_peer_healthy{peer=",
		"birak_peer_pending_repairs{peer=",
		"# TYPE birak_skipped_entries_total counter",
		"# TYPE birak_peer_lag gauge",
		"birak_commit_lock_held_seconds_total",
		"birak_commit_lock_acquisitions_total",
		"birak_commit_lock_worst_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}

	// A peer URL is operator input, not a safe label value.
	if !strings.Contains(body, `peer="http://peer-1:9100/\"odd\""`) {
		t.Errorf("peer label was not escaped:\n%s", body)
	}
	// Each series declares its type once, however many samples it carries.
	if n := strings.Count(body, "# TYPE birak_peer_lag "); n != 1 {
		t.Errorf("birak_peer_lag declared %d times", n)
	}
}

func TestMetricsRequireTheClusterSecret(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, root, "node", nil, Config{Secret: "secret"}, logger)

	out := httptest.NewRecorder()
	srv.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if out.Code != http.StatusUnauthorized {
		t.Fatalf("/metrics served without the cluster secret: %d", out.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set(HeaderSecret, "secret")
	out = httptest.NewRecorder()
	srv.Handler().ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("/metrics rejected a correct secret: %d", out.Code)
	}
}
