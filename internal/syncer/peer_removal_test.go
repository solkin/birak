package syncer

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

func TestReaddedPeerResumesAcceptedWorkWithoutNewChanges(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, db := auditSyncer(t)
	meta := auditMeta("file", "accepted bytes", 200)
	auditIndex(t, source, meta, "accepted bytes")
	auditIndex(t, dest, auditMeta("deleted", "stale bytes", 100), "stale bytes")
	peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
	t.Cleanup(peer.Close)
	// The source has no record of this deletion anymore. The queue is the only
	// surviving evidence that the stale destination file must be removed.
	deletion := store.FileMeta{Name: "deleted", Deleted: true, Clock: 201, ModTime: 201, Version: 2}
	for _, change := range []store.FileMeta{meta, deletion} {
		if err := dest.store.EnqueueChange(peer.URL, change, "accepted before peer removal"); err != nil {
			t.Fatal(err)
		}
	}
	if err := dest.store.PruneCursors(nil); err != nil {
		t.Fatal(err)
	}
	if err := dest.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openNamespaceNode(t, dest.syncDir, db, "destination")
	if err := reopened.store.PruneCursors([]string{peer.URL}); err != nil {
		t.Fatal(err)
	}
	reopened.opts.RepairInterval = 5 * time.Millisecond
	// Run only the real repair scheduler. Polling/reconciliation cannot hide a
	// discarded row, and the source will return 404 for the queued deletion.
	runRepairLoop(t, reopened, peer.URL)
	awaitRepairCondition(t, "retained work to finish after peer is re-added", func() bool {
		stats, err := reopened.store.RepairQueueStats()
		stored, metaErr := reopened.store.GetFile(deletion.Name)
		return err == nil && metaErr == nil && stats.Total == 0 && stored != nil && stored.Deleted
	})
	assertNamespaceBytes(t, reopened, meta.Name, "accepted bytes")
	if _, err := os.Stat(filepath.Join(reopened.syncDir, deletion.Name)); !os.IsNotExist(err) {
		t.Fatalf("accepted deletion lost during reconfiguration: %v", err)
	}
}
