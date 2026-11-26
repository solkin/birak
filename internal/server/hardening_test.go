package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/birak/birak/internal/store"
)

func TestMetricsTokenRequiresAuthWithoutClusterSecret(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "node-1", nil, Config{
		Secret:       "",
		MetricsToken: "secret-metrics-token",
	}, logger)
	h := srv.Handler()

	// Missing bearer token must be refused even when cluster_secret is empty.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /metrics with metrics_token set = %d, want 401", rec.Code)
	}

	// Valid bearer token succeeds.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret-metrics-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated /metrics = %d, want 200", rec.Code)
	}
}

func TestMetricsPreservesCounterPrecisionAboveMillion(t *testing.T) {
	const want = 1000001.0
	var e exposition
	e.counter("birak_test_total", "test counter", want)
	out := e.String()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "birak_test_total ") {
			raw := strings.TrimPrefix(line, "birak_test_total ")
			got, err := strconv.ParseFloat(raw, 64)
			if err != nil || got != want {
				t.Fatalf("counter lost precision: raw=%q parsed=%v err=%v want=%v", raw, got, err, want)
			}
			return
		}
	}
	t.Fatalf("metric sample not found in output:\n%s", out)
}
