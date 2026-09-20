package syncer

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

// Real HTTP, SQLite and filesystem commits, with reproducible delivery order.
// The scheduler is driven explicitly: this checks convergence/recovery rather
// than measuring real-time poll/backoff latency (covered by the loop tests).
func TestPartitionedClusterMatchesAcknowledgedHistory(t *testing.T) {
	for _, seed := range []int64{7, 29, 101} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			type node struct {
				mu    sync.RWMutex
				syn   *Syncer
				db    string
				peer  *httptest.Server
				fault atomic.Int32
			}
			var partition atomic.Bool
			var rejected, brokenBodies, brokenPages atomic.Int32
			nodes := make([]*node, 3)
			for i := range nodes {
				syn, db := auditSyncer(t)
				syn.nodeID = fmt.Sprintf("model-%d", i)
				syn.opts.BatchLimit = 3
				syn.opts.MaxConcurrentDownloads = 2
				n := &node{syn: syn, db: db}
				nodes[i] = n
				n.peer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n.mu.RLock()
					defer n.mu.RUnlock()
					w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
					if partition.Load() && (i == 2 || r.Header.Get(server.HeaderNodeID) == "model-2") || n.fault.Load() == 1 {
						rejected.Add(1)
						http.Error(w, "injected partition", http.StatusServiceUnavailable)
						return
					}
					if n.fault.Load() == 2 && strings.HasPrefix(r.URL.Path, "/files/") {
						brokenBodies.Add(1)
						w.Header().Set("Content-Length", "1000")
						_, _ = w.Write([]byte("truncated transfer"))
						return
					}
					if n.fault.Load() == 3 && r.URL.Path == "/changes" {
						brokenPages.Add(1)
						_, _ = w.Write([]byte(`[{"name":`))
						return
					}
					server.New(n.syn.store, n.syn.syncDir, n.syn.nodeID, nil, server.Config{}, n.syn.logger).Handler().ServeHTTP(w, r)
				}))
				t.Cleanup(n.peer.Close)
			}

			// Retain every acknowledged local state, not just the final state of
			// whichever node happens to be chosen as a reference at the end.
			expected := make(map[string]store.FileMeta)
			bodies := make(map[string]string)
			mutate := func(index, step int, name string, deleted bool) {
				t.Helper()
				s := nodes[index].syn
				full := filepath.Join(s.syncDir, name)
				body := fmt.Sprintf("seed=%d node=%d mutation=%d", seed, index, step)
				// Frequent timestamp rollback and equal mtimes exercise the logical
				// clock, including trusted writes preserving size and mtime.
				stamp := time.Unix(1_700_000_000, int64(rng.Intn(4)))
				if err := fileops.Commit(s.syncDir, []string{full}, func() error {
					if deleted {
						err := os.Remove(full)
						if os.IsNotExist(err) {
							return nil
						}
						return err
					}
					if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
						return err
					}
					return os.Chtimes(full, stamp, stamp)
				}); err != nil {
					t.Fatalf("mutation %d node %d: %v", step, index, err)
				}
				meta, err := s.store.GetFile(name)
				if err != nil {
					t.Fatal(err)
				}
				if meta == nil { // Deleting a name that has never existed is a no-op.
					return
				}
				old, exists := expected[name]
				if !exists || store.CompareState(meta, &old) > 0 {
					expected[name] = *meta
				}
				if !meta.Deleted {
					bodies[meta.Hash] = body
				}
			}
			restart := func(index int) {
				t.Helper()
				n := nodes[index]
				n.mu.Lock()
				defer n.mu.Unlock()
				old := n.syn
				if err := old.store.Close(); err != nil {
					t.Fatal(err)
				}
				n.syn = openNamespaceNode(t, old.syncDir, n.db, old.nodeID)
				n.syn.opts.BatchLimit = 3
				n.syn.opts.MaxConcurrentDownloads = 2
				if n.syn.store.Incarnation() == old.store.Incarnation() {
					t.Fatal("restart reused stream incarnation")
				}
			}
			// Reading a peer's stream only records work; transfers happen when
			// the queue is applied. A pull is therefore both halves, so an
			// injected fault is still in force when the bytes are fetched.
			pull := func(dst, src int, allowFailure bool) {
				t.Helper()
				s, peer := nodes[dst].syn, nodes[src].peer.URL
				if _, err := s.syncOnce(ctx, peer); err != nil && !allowFailure {
					t.Fatalf("pull %d <- %d: %v", dst, src, err)
				}
				items, err := s.store.DueRepairs(peer, 100)
				if err != nil {
					t.Fatalf("pull %d <- %d: %v", dst, src, err)
				}
				for _, item := range items {
					if err := s.repairOne(ctx, peer, item); err != nil && !allowFailure {
						t.Fatalf("pull %d <- %d: %v", dst, src, err)
					}
				}
			}

			mutate(0, -1, "initial", false)
			nodes[0].fault.Store(2)
			pull(1, 0, false) // A truncated body must become durable work.
			nodes[0].fault.Store(0)
			restart(1)
			if count, err := nodes[1].syn.store.PendingRepairCount(nodes[0].peer.URL); err != nil || count != 1 {
				t.Fatalf("reopen lost failed transfer: pending=%d err=%v", count, err)
			}
			nodes[0].fault.Store(3)
			pull(2, 0, true)
			nodes[0].fault.Store(0)
			if cursor, err := nodes[2].syn.store.GetCursor(nodes[0].peer.URL); err != nil || cursor != 0 {
				t.Fatalf("broken page acknowledged: cursor=%d err=%v", cursor, err)
			}

			for step := range 90 {
				partition.Store(step >= 15 && step < 65)
				mutate(rng.Intn(3), step, fmt.Sprintf("file-%02d", rng.Intn(12)), rng.Intn(4) == 0)
				dst, src := rng.Intn(3), rng.Intn(3)
				if dst == src {
					src = (src + 1) % 3
				}
				nodes[src].fault.Store(int32(rng.Intn(4)))
				pull(dst, src, true)
				nodes[src].fault.Store(0)
				if step == 35 || step == 70 {
					restart(step % 3)
				}
			}
			partition.Store(false)

			// Restore all edges, drain small stream pages and reconcile independently
			// of the cursors. No more mutations or injected failures are permitted.
			drain := func() {
				t.Helper()
				for dst := range nodes {
					for src := range nodes {
						if dst == src {
							continue
						}
						s, peer := nodes[dst].syn, nodes[src].peer.URL
						for page := 0; ; page++ {
							n, err := s.syncOnce(ctx, peer)
							if err != nil || page > 30 {
								t.Fatalf("stream did not drain: pages=%d err=%v", page, err)
							}
							if n == 0 {
								break
							}
						}
						if _, err := s.reconcileOnce(ctx, peer); err != nil {
							t.Fatal(err)
						}
						items, err := s.store.DueRepairs(peer, 100)
						if err != nil {
							t.Fatal(err)
						}
						for _, item := range items {
							if err := s.repairOne(ctx, peer, item); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
			}
			// Drain until the cluster is quiescent rather than a fixed number of
			// times: applying is queued work now, so a round trades an inline
			// retry for a queued one and convergence can need another pass.
			quiet := false
			for round := 0; round < 20 && !quiet; round++ {
				drain()
				quiet = true
				for _, n := range nodes {
					stats, err := n.syn.store.RepairQueueStats()
					if err != nil {
						t.Fatal(err)
					}
					if stats.Total == 0 {
						continue
					}
					quiet = false
					if stats.Due == 0 {
						// Everything left is waiting out a retry delay rather
						// than failing; let the shortest one elapse.
						time.Sleep(minRepairBackoff)
					}
				}
			}
			versions := make([]int64, len(nodes))
			for i, n := range nodes {
				manifest, err := n.syn.store.ListManifest("", 100)
				if err != nil || len(manifest) != len(expected) {
					t.Fatalf("node %d unexpected manifest: %d names, want %d: %v", i, len(manifest), len(expected), err)
				}
				all := make(map[string]store.FileMeta, len(manifest))
				for _, meta := range manifest {
					all[meta.Name] = meta
				}
				for name, want := range expected {
					got := all[name]
					got.Version, got.DeletedAt, want.Version, want.DeletedAt = 0, 0, 0, 0
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("node %d %s: got %+v, history winner %+v", i, name, got, want)
					}
					data, err := os.ReadFile(filepath.Join(n.syn.syncDir, name))
					if want.Deleted {
						if !os.IsNotExist(err) {
							t.Fatalf("node %d resurrected %s: %v", i, name, err)
						}
					} else if err != nil || string(data) != bodies[want.Hash] {
						t.Fatalf("node %d wrong bytes for %s: %q %v", i, name, data, err)
					}
				}
				stats, err := n.syn.store.RepairQueueStats()
				if err != nil || stats.Total != 0 {
					t.Fatalf("node %d unresolved repairs: %+v %v", i, stats, err)
				}
				versions[i], err = n.syn.store.MaxVersion()
				if err != nil {
					t.Fatal(err)
				}
			}
			drain()
			for i, n := range nodes {
				if version, err := n.syn.store.MaxVersion(); err != nil || version != versions[i] {
					t.Fatalf("node %d changes after convergence: %d -> %d: %v", i, versions[i], version, err)
				}
			}
			if rejected.Load() == 0 || brokenBodies.Load() == 0 || brokenPages.Load() == 0 {
				t.Fatal("fault schedule failed to exercise every HTTP failure")
			}
			t.Logf("90 local mutations; 3 database reopens; injected 503=%d truncated bodies=%d broken pages=%d; exact convergence and stable replay", rejected.Load(), brokenBodies.Load(), brokenPages.Load())
		})
	}
}
