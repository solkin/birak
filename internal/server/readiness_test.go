package server

import (
	"context"
	"encoding/json"
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

type readinessProvider struct{ w *watcher.Watcher }

func (p readinessProvider) PeerStats() []PeerStatus        { return nil }
func (p readinessProvider) ScanStatus() watcher.ScanStatus { return p.w.Status() }
func (p readinessProvider) CheckStorage() error            { return p.w.CheckStorage() }

func TestReadinessRequiresScanAndBoundStorage(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	srv := New(st, root, "node", nil, Config{Secret: "secret", Stats: readinessProvider{w}}, logger)
	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path == "/status" {
			req.Header.Set(HeaderSecret, "secret")
		}
		out := httptest.NewRecorder()
		srv.Handler().ServeHTTP(out, req)
		return out
	}
	if got := request("/readyz").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("ready before first scan: %d", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-w.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("initial scan did not finish")
	}
	if got := request("/readyz").Code; got != http.StatusOK {
		t.Fatalf("not ready after scan: %d", got)
	}
	if err := os.Remove(filepath.Join(root, ".birak", "storage-id")); err != nil {
		t.Fatal(err)
	}
	if got := request("/readyz").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("missing storage sentinel reported ready: %d", got)
	}
	if got := request("/healthz").Code; got != http.StatusOK {
		t.Fatalf("storage failure changed process liveness: %d", got)
	}
	resp := request("/status")
	var status StatusResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Local == nil || status.Local.Ready || status.Local.LastError == "" {
		t.Fatalf("storage fault missing from status: %+v", status.Local)
	}
}

func TestChangesRecordsRewindAndRejectsStaleEpochAck(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "source", nil, Config{}, logger)
	for _, tc := range []struct {
		query string
		want  int64
	}{
		{"since=100&epoch=" + st.Incarnation(), 100},
		{"since=100&epoch=previous-source-incarnation", 0},
		{"since=50&epoch=" + st.Incarnation(), 50},
		{"since=0&epoch=" + st.Incarnation(), 0},
	} {
		req := httptest.NewRequest(http.MethodGet, "/changes?"+tc.query, nil)
		req.Header.Set(HeaderNodeID, "consumer")
		req.Header.Set(HeaderProtocol, ProtocolVersion)
		req.Header.Set(HeaderNodeEpoch, "consumer-incarnation")
		out := httptest.NewRecorder()
		srv.Handler().ServeHTTP(out, req)
		if out.Code != http.StatusOK || out.Header().Get(HeaderEpoch) != st.Incarnation() || out.Header().Get(HeaderNodeID) != "source" {
			t.Fatalf("invalid protocol response: %d %v", out.Code, out.Header())
		}
		acks, err := st.PeerAcks()
		if err != nil || acks["consumer"] != tc.want {
			t.Fatalf("%s: acks=%v err=%v, want %d", tc.query, acks, err, tc.want)
		}
	}
	bad := httptest.NewRecorder()
	srv.Handler().ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/changes?since=-1", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("negative cursor accepted: %d", bad.Code)
	}
}

func TestLegacyReplicationClientIsRejected(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "source", nil, Config{}, logger)
	for _, version := range []string{"", "1", "future"} {
		req := httptest.NewRequest("GET", "/changes", nil)
		req.Header.Set(HeaderNodeID, "legacy-consumer")
		req.Header.Set(HeaderProtocol, version)
		out := httptest.NewRecorder()
		srv.Handler().ServeHTTP(out, req)
		if out.Code != http.StatusUpgradeRequired {
			t.Fatalf("protocol %q accepted: %d", version, out.Code)
		}
	}
}
