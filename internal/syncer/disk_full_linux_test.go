//go:build linux

package syncer

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// The opt-in mount must be a dedicated, bounded tmpfs, never a host data disk.
// For example: docker run --tmpfs /limited:rw,size=16m,mode=1777 ...
// with BIRAK_TEST_FULL_VOLUME=/limited and an unprivileged test user.
func TestDiskFullReplicationRecoversAfterReopen(t *testing.T) {
	volume := os.Getenv("BIRAK_TEST_FULL_VOLUME")
	if volume == "" {
		t.Skip("requires a dedicated bounded tmpfs via BIRAK_TEST_FULL_VOLUME")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(volume, &fs); err != nil {
		t.Fatal(err)
	}
	capacity := uint64(fs.Bsize) * fs.Blocks
	if fs.Type != 0x01021994 || capacity < 1<<20 || capacity > 64<<20 {
		t.Fatalf("refuse to fill non-tmpfs or unbounded volume: type=%x capacity=%d", fs.Type, capacity)
	}
	if os.Geteuid() == 0 {
		t.Fatal("disk-full validation must run as an unprivileged user")
	}
	for _, target := range []string{"data", "data-mid-transfer", "metadata"} {
		t.Run(target, func(t *testing.T) {
			source, _ := auditSyncer(t)
			payload := strings.Repeat("verified remote bytes\n", 4096)
			remote := auditMeta("file", payload, 200)
			auditIndex(t, source, remote, payload)
			peer := httptest.NewServer(server.New(source.store, source.syncDir, "disk-full-source", nil, server.Config{}, source.logger).Handler())
			defer peer.Close()
			limited, err := os.MkdirTemp(volume, "replication-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(limited) })
			dataDir, metaDir := t.TempDir(), t.TempDir()
			if target != "metadata" {
				dataDir = limited
			} else {
				metaDir = limited
			}
			dbPath := filepath.Join(metaDir, "metadata.db")
			db, err := store.New(dbPath, source.logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			w := watcher.New(dataDir, db, source.logger, time.Millisecond, time.Hour, nil)
			dest := New(db, w, dataDir, "disk-full-destination", []string{peer.URL}, nil, source.logger, Options{BatchLimit: 10, MaxConcurrentDownloads: 1})
			t.Cleanup(func() { dest.client.CloseIdleConnections(); dest.downloadClient.CloseIdleConnections() })
			original := auditMeta("file", "acknowledged local bytes", 100)
			auditIndex(t, dest, original, "acknowledged local bytes")
			if err = w.CheckStorage(); err != nil {
				t.Fatal(err)
			}
			if err = db.SetPeerState(peer.URL, store.PeerState{Epoch: source.store.Incarnation()}); err != nil {
				t.Fatal(err)
			}

			filler, err := os.CreateTemp(volume, "space-pressure-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { filler.Close(); os.Remove(filler.Name()) })
			block := make([]byte, 64<<10)
			var filled uint64
			for {
				n, writeErr := filler.Write(block)
				filled += uint64(n)
				if writeErr != nil {
					if !errors.Is(writeErr, syscall.ENOSPC) {
						t.Fatalf("unexpected filler failure: %v", writeErr)
					}
					break
				}
				if filled > capacity {
					t.Fatal("volume did not enforce its configured size")
				}
			}
			if err = filler.Close(); err != nil {
				t.Fatal(err)
			}
			t.Logf("real ENOSPC on %s volume after %d bytes", target, filled)
			if target == "data-mid-transfer" {
				// The payload exceeds this allowance: its first chunks fit, then
				// writing the staging file must hit the real filesystem limit.
				if err := os.Truncate(filler.Name(), int64(filled)-(64<<10)); err != nil {
					t.Fatal(err)
				}
			}
			_, syncErr := dest.syncOnce(context.Background(), peer.URL)
			state, err := db.GetPeerState(peer.URL)
			if err != nil {
				t.Fatal(err)
			}
			count, err := db.PendingRepairCount(peer.URL)
			if err != nil {
				t.Fatal(err)
			}
			if target != "metadata" {
				if syncErr != nil || state.Version == 0 || count != 1 {
					t.Fatalf("data-full work not durably queued: err=%v cursor=%+v pending=%d", syncErr, state, count)
				}
				// Reading the stream only records the work. The transfer — and
				// so the full volume — is met when the queue is applied, which
				// has to happen while the volume is still full.
				items, err := db.DueRepairs(peer.URL, 10)
				if err != nil || len(items) != 1 {
					t.Fatalf("polled work was not queued: %+v %v", items, err)
				}
				if err := dest.repairOne(context.Background(), peer.URL, items[0]); err != nil {
					t.Fatalf("a full volume broke queue bookkeeping: %v", err)
				}
				// The failed attempt is deferred; wait out its first retry delay
				// so the row is visible again with the error it recorded.
				time.Sleep(minRepairBackoff + 200*time.Millisecond)
				items, err = db.DueRepairs(peer.URL, 10)
				if err != nil || len(items) != 1 || !strings.Contains(items[0].LastError, syscall.ENOSPC.Error()) {
					t.Fatalf("download did not encounter real ENOSPC: %+v %v", items, err)
				}
			} else {
				if syncErr == nil || !strings.Contains(strings.ToLower(syncErr.Error()), "full") || state.Version != 0 {
					t.Fatalf("metadata-full failure advanced cursor or was hidden: err=%v cursor=%+v", syncErr, state)
				}
			}
			assertNamespaceBytes(t, dest, original.Name, "acknowledged local bytes")
			before, err := db.GetFile(original.Name)
			if err != nil || store.CompareState(before, &original) != 0 {
				t.Fatalf("full volume changed metadata: %+v %v", before, err)
			}
			if err := db.Close(); err != nil {
				t.Logf("close while full: %v", err)
			}
			if err := os.Remove(filler.Name()); err != nil {
				t.Fatal(err)
			}
			reopened := openNamespaceNode(t, dataDir, dbPath, "disk-full-destination")
			if target != "metadata" {
				items, err := reopened.store.DueRepairs(peer.URL, 10)
				if err != nil || len(items) != 1 {
					t.Fatalf("queue lost on reopen: %v %v", items, err)
				}
				reopened.repairOne(context.Background(), peer.URL, items[0])
			} else {
				if _, err := reopened.syncOnce(context.Background(), peer.URL); err != nil {
					t.Fatalf("replay after disk-full: %v", err)
				}
				// Replaying the stream records the change; applying it is what
				// puts the bytes back.
				drainQueued(t, reopened, peer.URL)
			}
			assertNamespaceBytes(t, reopened, remote.Name, payload)
			actual, err := reopened.store.GetFile(remote.Name)
			if err != nil || store.CompareState(actual, &remote) != 0 {
				t.Fatalf("recovered state: %+v %v", actual, err)
			}
			if count, err := reopened.store.PendingRepairCount(peer.URL); err != nil || count != 0 {
				t.Fatalf("undrained queue: %d %v", count, err)
			}
			t.Logf("%s volume recovered from real ENOSPC using persisted state", target)
		})
	}
}
