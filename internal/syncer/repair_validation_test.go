package syncer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

func TestBlockedLocalPathRemainsInRepair(t *testing.T) {
	for _, operation := range []string{"download", "delete"} {
		for _, mode := range []string{"poll", "repair", "reconcile"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				source, _ := auditSyncer(t)
				dest, db := auditSyncer(t)
				meta := auditMeta("folder/file", "remote bytes", 100)
				auditIndex(t, source, meta, "remote bytes")
				if operation == "delete" {
					meta.Deleted, meta.Hash, meta.Size = true, "", 0
					if err := os.Remove(filepath.Join(source.syncDir, meta.Name)); err != nil {
						t.Fatal(err)
					}
					if _, err := source.store.PutRemote(meta); err != nil {
						t.Fatal(err)
					}
				}
				peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
				defer peer.Close()
				outside := t.TempDir()
				if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside bytes"), 0600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(dest.syncDir, "folder")
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "poll":
					if _, err := dest.syncOnce(context.Background(), peer.URL); err != nil {
						t.Fatal(err)
					}
				case "repair":
					if err := dest.store.EnqueueChange(peer.URL, meta, "retry"); err != nil {
						t.Fatal(err)
					}
					items, err := dest.store.DueRepairs(peer.URL, 10)
					if err != nil || len(items) != 1 {
						t.Fatalf("queue: %v %v", items, err)
					}
					dest.repairOne(context.Background(), peer.URL, items[0])
				case "reconcile":
					if _, err := dest.reconcileOnce(context.Background(), peer.URL); err != nil {
						t.Fatal(err)
					}
				}
				count, err := dest.store.PendingRepairCount(peer.URL)
				if err != nil || count != 1 {
					t.Fatalf("lost blocked operation: pending=%d err=%v", count, err)
				}
				if got, err := os.ReadFile(filepath.Join(outside, "file")); err != nil || string(got) != "outside bytes" {
					t.Fatalf("outside modified: %q %v", got, err)
				}
				if got, err := dest.store.GetFile(meta.Name); err != nil || got != nil {
					t.Fatalf("indexed outside bytes: %+v %v", got, err)
				}
				// Reopen SQLite with the cursor already past the change. Recovery must
				// depend on the durable queue, without a new peer write or reconciliation.
				if err := dest.store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened := openNamespaceNode(t, dest.syncDir, db, dest.nodeID)
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := reopened.store.DeferRepair(peer.URL, meta.Name, -time.Second, "unblocked"); err != nil {
					t.Fatal(err)
				}
				items, err := reopened.store.DueRepairs(peer.URL, 10)
				if err != nil || len(items) != 1 {
					t.Fatalf("reopened queue: %v %v", items, err)
				}
				reopened.repairOne(context.Background(), peer.URL, items[0])
				if operation == "download" {
					assertNamespaceBytes(t, reopened, meta.Name, "remote bytes")
				} else if _, err := os.Stat(filepath.Join(reopened.syncDir, meta.Name)); !os.IsNotExist(err) {
					t.Fatalf("deletion did not apply: %v", err)
				}
				if got, err := reopened.store.GetFile(meta.Name); err != nil || store.CompareState(got, &meta) != 0 {
					t.Fatalf("recovered metadata: %+v %v", got, err)
				}
				if count, err := reopened.store.PendingRepairCount(peer.URL); err != nil || count != 0 {
					t.Fatalf("repair not drained: %d %v", count, err)
				}
			})
		}
	}
}

type readBoundary struct {
	io.ReadCloser
	before func() error
}

func (r *readBoundary) Read(p []byte) (int, error) {
	if r.before != nil {
		before := r.before
		r.before = nil
		if err := before(); err != nil {
			return 0, err
		}
	}
	return r.ReadCloser.Read(p)
}

func TestParentChangeDuringDownloadRemainsInRepair(t *testing.T) {
	for _, target := range []string{"outside", "private"} {
		for _, boundary := range []string{"before-temp", "before-publication"} {
			t.Run(target+"/"+boundary, func(t *testing.T) {
				source, _ := auditSyncer(t)
				dest, _ := auditSyncer(t)
				meta := auditMeta("folder/file", "remote bytes", 100)
				auditIndex(t, source, meta, "remote bytes")
				peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
				defer peer.Close()
				outside := t.TempDir()
				if target == "private" {
					outside = filepath.Join(dest.syncDir, ".birak", "hidden")
					if err := os.MkdirAll(outside, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside bytes"), 0600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(dest.syncDir, "folder")
				changeParent := func() error {
					unlock := fileops.Lock(dest.syncDir)
					defer unlock()
					return os.Symlink(outside, link)
				}
				transport := dest.downloadClient.Transport
				dest.downloadClient.Transport = auditTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					if boundary == "before-temp" {
						if err := changeParent(); err != nil {
							resp.Body.Close()
							return nil, err
						}
					}
					resp.Body = &readBoundary{ReadCloser: resp.Body, before: func() error {
						if boundary == "before-publication" {
							if err := changeParent(); err != nil {
								return err
							}
						}
						if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 {
							t.Errorf("staged download outside data root: %v %v", entries, err)
						}
						return nil
					}}
					return resp, nil
				})
				if _, err := dest.syncOnce(context.Background(), peer.URL); err != nil {
					t.Fatal(err)
				}
				// Transfers happen in the apply loop now. Drive one attempt
				// directly so the hooked transport still observes the download,
				// and so the queued item keeps its original backoff.
				if err := dest.applyChange(context.Background(), peer.URL, meta); err == nil {
					t.Fatal("publication through a changed parent succeeded")
				}
				if count, err := dest.store.PendingRepairCount(peer.URL); err != nil || count != 1 {
					t.Fatalf("path change erased accepted work: %d %v", count, err)
				}
				if got, err := dest.store.GetFile(meta.Name); err != nil || got != nil {
					t.Fatalf("indexed outside bytes after download: %+v %v", got, err)
				}
				if got, err := os.ReadFile(filepath.Join(outside, "file")); err != nil || string(got) != "outside bytes" {
					t.Fatalf("outside modified: %q %v", got, err)
				}
				dest.downloadClient.Transport = transport
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				items, err := dest.store.DueRepairs(peer.URL, 10)
				if err != nil || len(items) != 1 {
					t.Fatalf("queue: %v %v", items, err)
				}
				dest.repairOne(context.Background(), peer.URL, items[0])
				assertNamespaceBytes(t, dest, meta.Name, "remote bytes")
				if count, err := dest.store.PendingRepairCount(peer.URL); err != nil || count != 0 {
					t.Fatalf("queue not drained: %d %v", count, err)
				}
			})
		}
	}
}

func TestBadMetadataResponseCannotEraseQueuedRepair(t *testing.T) {
	for _, kind := range []string{"invalid-hash", "wrong-name"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := auditSyncer(t)
			wanted := auditMeta("file", "wanted bytes", 100)
			wrong := wanted
			wrong.Clock = 200
			if kind == "invalid-hash" {
				wrong.Hash = "invalid"
			} else {
				wrong.Name = "another-file"
				wrong.Deleted = true
				wrong.Hash = ""
				wrong.Size = 0
			}
			var healthy atomic.Bool
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
				switch r.URL.Path {
				case "/meta/file":
					response := wrong
					if healthy.Load() {
						response = wanted
					}
					_ = json.NewEncoder(w).Encode(response)
				case "/files/file":
					_, _ = w.Write([]byte("wanted bytes"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			if err := s.store.EnqueueChange(peer.URL, wanted, "accepted download"); err != nil {
				t.Fatal(err)
			}
			items, err := s.store.DueRepairs(peer.URL, 10)
			if err != nil || len(items) != 1 {
				t.Fatalf("queue: %v %v", items, err)
			}
			s.repairOne(context.Background(), peer.URL, items[0])
			if count, err := s.store.PendingRepairCount(peer.URL); err != nil || count != 1 {
				t.Fatalf("bad response erased accepted work: %d %v", count, err)
			}
			if meta, err := s.store.GetFile(wanted.Name); err != nil || meta != nil {
				t.Fatalf("bad response changed local state: %+v %v", meta, err)
			}
			healthy.Store(true)
			if err := s.store.DeferRepair(peer.URL, wanted.Name, -time.Second, "peer fixed"); err != nil {
				t.Fatal(err)
			}
			items, err = s.store.DueRepairs(peer.URL, 10)
			if err != nil || len(items) != 1 {
				t.Fatalf("retry queue: %v %v", items, err)
			}
			s.repairOne(context.Background(), peer.URL, items[0])
			assertNamespaceBytes(t, s, wanted.Name, "wanted bytes")
			if count, err := s.store.PendingRepairCount(peer.URL); err != nil || count != 0 {
				t.Fatalf("repair not drained: %d %v", count, err)
			}
		})
	}
}

func TestConfiguredExclusionCanDrainPreviouslyQueuedRepair(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	meta := auditMeta("private/file", "excluded bytes", 100)
	auditIndex(t, source, meta, "excluded bytes")
	peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
	defer peer.Close()
	if err := dest.store.EnqueueChange(peer.URL, meta, "accepted before configuration changed"); err != nil {
		t.Fatal(err)
	}
	dest.ignorePatterns = []string{"private"}
	items, err := dest.store.DueRepairs(peer.URL, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("queue: %v %v", items, err)
	}
	dest.repairOne(context.Background(), peer.URL, items[0])
	if count, err := dest.store.PendingRepairCount(peer.URL); err != nil || count != 0 {
		t.Fatalf("excluded repair remained queued: %d %v", count, err)
	}
	if _, err := os.Stat(filepath.Join(dest.syncDir, meta.Name)); !os.IsNotExist(err) {
		t.Fatalf("excluded content downloaded: %v", err)
	}
	if got, err := dest.store.GetFile(meta.Name); err != nil || got != nil {
		t.Fatalf("excluded metadata indexed: %+v %v", got, err)
	}
}
