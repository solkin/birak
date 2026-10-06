package syncer

import (
	"context"
	"errors"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/watcher"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuarantineRepairSurvivesRestart(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, database := auditSyncer(t)
	body := "healthy original"
	meta := auditMeta("damaged.apk", body, time.Now().UnixNano())
	auditIndex(t, source, meta, body)
	auditIndex(t, dest, meta, body)
	peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
	defer peer.Close()
	dest.watcher.SetRepairPeers([]string{peer.URL})
	path := filepath.Join(dest.syncDir, meta.Name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("!", len(body))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := dest.watcher.Refresh(meta.Name); !errors.Is(err, watcher.ErrIntegrity) {
		t.Fatalf("not quarantined: %v", err)
	}
	if count, err := dest.store.PendingRepairCount(peer.URL); err != nil || count != 1 {
		t.Fatalf("repair not durable: %d %v", count, err)
	}
	if err := dest.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openNamespaceNode(t, dest.syncDir, database, "reopened-destination")
	// Keep the scrub from masking the restart bug. This models the ordinary
	// period before a 10 TB tree's scrub returns to this already visited name.
	reopened.watcher.SetScrubRate(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reopened.watcher.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-reopened.watcher.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("initial sweep did not finish")
	}
	items, err := reopened.store.DueRepairs(peer.URL, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("reopened queue: %+v %v", items, err)
	}
	if err := reopened.repairOne(context.Background(), peer.URL, items[0]); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count, err := reopened.store.PendingRepairCount(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("known corruption survived restart and repair: bytes=%q queued=%d quarantined=%d", got, count, reopened.watcher.Status().Quarantined)
	}
}
