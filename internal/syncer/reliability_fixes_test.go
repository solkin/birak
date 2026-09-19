package syncer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

func TestIntegrityRepairStartsOnInitialScan(t *testing.T) {
	damaged, _ := auditSyncer(t)
	healthy, _ := auditSyncer(t)
	good := auditMeta("file", "healthy!", time.Now().Add(-time.Hour).UnixNano())
	auditIndex(t, damaged, good, "healthy!")
	auditIndex(t, healthy, good, "healthy!")
	peer := httptest.NewServer(server.New(healthy.store, healthy.syncDir, "healthy", nil, server.Config{}, healthy.logger).Handler())
	defer peer.Close()
	path := filepath.Join(damaged.syncDir, "file")
	if err := os.WriteFile(path, []byte("corrupt!"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(0, good.ModTime)
	os.Chtimes(path, stamp, stamp)
	syn := New(damaged.store, damaged.watcher, damaged.syncDir, "damaged", []string{peer.URL}, nil, damaged.logger, Options{PollInterval: 5 * time.Millisecond, BatchLimit: 10, MaxConcurrentDownloads: 1, RepairInterval: 5 * time.Millisecond})
	if err := damaged.watcher.Refresh("file"); !errors.Is(err, watcher.ErrIntegrity) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	if damaged.watcher.Status().Ready {
		t.Fatal("damaged node ready")
	}
	ctx, cancel := context.WithCancel(context.Background())
	wdone, sdone := make(chan error, 1), make(chan struct{})
	go func() { wdone <- damaged.watcher.Run(ctx) }()
	go func() { syn.Run(ctx); close(sdone) }()
	defer func() {
		cancel()
		<-sdone
		if err := <-wdone; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(path)
		stats, _ := damaged.store.RepairQueueStats()
		if string(body) == "healthy!" && stats.Total == 0 && !damaged.watcher.NeedsRepair("file") && damaged.watcher.Status().Ready {
			meta, _ := damaged.store.GetFile("file")
			if store.CompareState(meta, &good) != 0 {
				t.Fatalf("repair invented mutation: %+v", meta)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	body, _ := os.ReadFile(path)
	t.Fatalf("initial quarantine blocked recovery: %q stats=%+v", body, syn.PeerStats())
}
func TestProtocolMismatchDoesNotAdvanceCursor(t *testing.T) {
	for _, version := range []string{"", "1", "future"} {
		t.Run("version="+version, func(t *testing.T) {
			s, _ := auditSyncer(t)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(server.HeaderProtocol, version)
				w.Write([]byte(`[]`))
			}))
			defer peer.Close()
			if _, err := s.doRequest(context.Background(), peer.URL+"/changes"); err == nil {
				t.Fatal("accepted incompatible peer")
			}
			state, err := s.store.GetPeerState(peer.URL)
			if err != nil || state.Version != 0 {
				t.Fatalf("cursor changed: %+v %v", state, err)
			}
		})
	}
}
func TestLocalOverwriteClockIsTransitiveAcrossPeers(t *testing.T) {
	a, _ := auditSyncer(t)
	b, _ := auditSyncer(t)
	c, _ := auditSyncer(t)
	original := auditMeta("file", "old", time.Now().UnixNano())
	for _, s := range []*Syncer{a, b, c} {
		auditIndex(t, s, original, "old")
	}
	path := filepath.Join(a.syncDir, "file")
	os.WriteFile(path, []byte("new"), 0o600)
	stamp := time.Unix(0, original.ModTime).Add(-time.Hour)
	os.Chtimes(path, stamp, stamp)
	if err := a.watcher.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	newMeta, _ := a.store.GetFile("file")
	if store.CompareState(newMeta, &original) <= 0 {
		t.Fatal("local mutation did not advance")
	}
	b.downloadClient.Transport = auditTransport(func(*http.Request) (*http.Response, error) { return auditBody("new"), nil })
	if err := b.applyChange(context.Background(), "http://a", *newMeta); err != nil {
		t.Fatal(err)
	}
	bMeta, _ := b.store.GetFile("file")
	if bMeta.Clock != newMeta.Clock {
		t.Fatal("relay changed clock")
	}
	c.downloadClient.Transport = auditTransport(func(*http.Request) (*http.Response, error) { return auditBody("new"), nil })
	if err := c.applyChange(context.Background(), "http://b", *bMeta); err != nil {
		t.Fatal(err)
	}
	if err := c.applyChange(context.Background(), "http://stale", original); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(c.syncDir, "file"))
	if string(body) != "new" {
		t.Fatalf("stale peer reverted transitive overwrite: %q", body)
	}
}
