package syncer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

func TestWatchStallTripsAfterFinalByteWithoutEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &progressReader{}
	// Simulate all 100 expected bytes already received, with peer hanging before EOF.
	src.n.Store(100)
	sw := watchStall(src, 100, 20*time.Millisecond, cancel)
	defer sw.stop()

	select {
	case <-ctx.Done():
		if !sw.fired() {
			t.Fatal("context cancelled without stall trip")
		}
	case <-time.After(time.Second):
		t.Fatal("watchStall hung indefinitely after receiving final byte without EOF")
	}
}

func TestCommitDeletionClearsQuarantine(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	w := watcher.New(dir, st, logger, 50*time.Millisecond, time.Hour, nil)
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	syn := New(st, w, dir, "node-1", nil, nil, logger, Options{})

	filePath := filepath.Join(dir, "bad.bin")
	if err := os.WriteFile(filePath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutLocal(store.FileMeta{
		Name:    "bad.bin",
		ModTime: time.Now().UnixNano(),
		Size:    7,
		Hash:    "deadbeef",
		Clock:   10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetQuarantined("bad.bin", "inode-key-1"); err != nil {
		t.Fatal(err)
	}
	if !st.IsQuarantined("bad.bin") {
		t.Fatal("expected file to be quarantined")
	}

	delMeta := store.FileMeta{
		Name:    "bad.bin",
		ModTime: time.Now().Add(time.Second).UnixNano(),
		Deleted: true,
		Clock:   20,
	}
	if err := syn.applyDeletion(context.Background(), delMeta); err != nil {
		t.Fatalf("applyDeletion: %v", err)
	}
	if st.IsQuarantined("bad.bin") {
		t.Fatal("quarantine entry was not cleared after replicated DELETE")
	}
}

func TestProxy5xxNotReportedAsProtocolMismatch(t *testing.T) {
	syn, _ := auditSyncer(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Upstream proxy 502 carries no X-Birak-Protocol header.
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer proxy.Close()

	_, err := syn.syncOnce(context.Background(), proxy.URL)
	if err == nil {
		t.Fatal("expected error for 502 response")
	}
	if strings.Contains(err.Error(), "incompatible replication protocol") {
		t.Fatalf("proxy 502 misreported as protocol mismatch: %v", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected status 502 error, got: %v", err)
	}
}

func TestReconcileNotifiesApplyOnPageBudget(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	w := watcher.New(dir, st, logger, 50*time.Millisecond, time.Hour, nil)
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}

	peer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		rw.Header().Set(server.HeaderNodeID, "peer-remote")
		rw.Header().Set(server.HeaderEpoch, "ep-remote")
		if r.URL.Path == "/manifest" {
			page := []store.FileMeta{
				auditMeta("a.bin", "hello", time.Now().UnixNano()),
			}
			_ = json.NewEncoder(rw).Encode(page)
			return
		}
		http.NotFound(rw, r)
	}))
	defer peer.Close()

	syn := New(st, w, dir, "node-local", []string{peer.URL}, nil, logger, Options{
		ReconcilePageBudget: 1,
	})
	defer func() {
		syn.client.CloseIdleConnections()
		syn.downloadClient.CloseIdleConnections()
	}()

	completed, err := syn.reconcileOnce(context.Background(), peer.URL)
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if completed {
		t.Fatal("expected completed=false when paused on ReconcilePageBudget")
	}
	select {
	case <-syn.wake[peer.URL]:
	default:
		t.Fatal("expected apply loop to be notified when reconcileOnce paused on page budget with queued items")
	}
}
