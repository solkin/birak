package syncer

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

func TestOppositeNamespaceResolutionsCannotDeleteNewerThirdNodeFile(t *testing.T) {
	nodes, peers := namespaceNodes(t, 3)
	bodies := []string{"first candidate", "second candidate", "third candidate"}
	sort.Slice(bodies, func(i, j int) bool { return auditMeta("", bodies[i], 1).Hash < auditMeta("", bodies[j], 1).Hash })
	old, middle, newest := auditMeta("a", bodies[0], 100), auditMeta("a/child", bodies[1], 100), auditMeta("a", bodies[2], 100)
	auditIndex(t, nodes[0], old, bodies[0])
	auditIndex(t, nodes[1], middle, bodies[1])
	auditIndex(t, nodes[2], newest, bodies[2])
	// Both resolutions happen before either node has heard the other's result.
	for _, i := range []int{0, 2} {
		if err := nodes[i].applyChange(context.Background(), peers[1].URL, middle); err != nil {
			t.Fatal(err)
		}
	}
	convergeNamespace(t, nodes, peers)
	for _, node := range nodes {
		assertNamespaceBytes(t, node, "a", bodies[2])
		assertNamespaceBytes(t, node, store.ConflictCopyName(middle.Name, middle.Hash), bodies[1])
		assertNamespaceBytes(t, node, store.ConflictCopyName(old.Name, old.Hash), bodies[0])
	}
}

func TestReplicaSupportsMaximumFilenameLength(t *testing.T) {
	nodes, peers := namespaceNodes(t, 2)
	name := strings.Repeat("f", 255)
	meta := auditMeta(name, "long name", 100)
	auditIndex(t, nodes[0], meta, "long name")
	convergeNamespace(t, nodes, peers)
	assertNamespaceBytes(t, nodes[1], name, "long name")
}

func namespaceNodes(t *testing.T, count int) ([]*Syncer, []*httptest.Server) {
	t.Helper()
	nodes := make([]*Syncer, count)
	peers := make([]*httptest.Server, count)
	for i := range nodes {
		nodes[i], _ = auditSyncer(t)
		nodes[i].nodeID = fmt.Sprintf("namespace-%d", i)
		nodes[i].opts.MaxConcurrentDownloads = 4
		peers[i] = httptest.NewServer(server.New(nodes[i].store, nodes[i].syncDir, nodes[i].nodeID, nil, server.Config{}, nodes[i].logger).Handler())
		t.Cleanup(peers[i].Close)
	}
	return nodes, peers
}

func assertNamespaceBytes(t *testing.T, node *Syncer, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(node.syncDir, name))
	if err != nil || string(got) != want {
		t.Fatalf("%s on %s: %q %v", name, node.nodeID, got, err)
	}
	meta, err := node.store.GetFile(name)
	if err != nil || meta == nil || meta.Deleted || meta.Hash != auditMeta(name, want, 1).Hash {
		t.Fatalf("metadata for %s: %+v %v", name, meta, err)
	}
}

func namespaceManifest(t *testing.T, node *Syncer) []store.FileMeta {
	t.Helper()
	files, err := node.store.ListManifest("", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range files {
		files[i].Version = 0
		files[i].DeletedAt = 0
	}
	return files
}

func convergeNamespace(t *testing.T, nodes []*Syncer, peers []*httptest.Server) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		for i, node := range nodes {
			for j, peer := range peers {
				if i == j {
					continue
				}
				if _, err := node.syncOnce(context.Background(), peer.URL); err != nil {
					t.Fatal(err)
				}
				items, err := node.store.DueRepairs(peer.URL, 1000)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					node.repairOne(context.Background(), peer.URL, item)
				}
			}
		}
		want, ready := namespaceManifest(t, nodes[0]), true
		for _, node := range nodes {
			stats, err := node.store.RepairQueueStats()
			if err != nil {
				t.Fatal(err)
			}
			if stats.Total != 0 || !reflect.DeepEqual(want, namespaceManifest(t, node)) {
				ready = false
			}
		}
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, node := range nodes {
		t.Logf("%s: %+v", node.nodeID, namespaceManifest(t, node))
	}
	t.Fatal("namespace did not converge or retained pending repairs")
}

func TestNamespacePermutationsRetainEveryConflictingContent(t *testing.T) {
	for _, names := range [][]string{{"a", "a/b", "a/c"}, {"a", "a/b", "a/b/c"}} {
		for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
			t.Run(fmt.Sprintf("%v/%v", names, order), func(t *testing.T) {
				nodes, peers := namespaceNodes(t, 3)
				var originals []store.FileMeta
				for i, name := range names {
					body := "content of " + name
					meta := auditMeta(name, body, int64(100+i))
					auditIndex(t, nodes[order[i]], meta, body)
					originals = append(originals, meta)
				}
				convergeNamespace(t, nodes, peers)
				for _, node := range nodes {
					for _, original := range originals {
						name := original.Name
						meta, err := node.store.GetFile(name)
						if err != nil {
							t.Fatal(err)
						}
						if meta == nil || meta.Deleted {
							name = store.ConflictCopyName(original.Name, original.Hash)
						}
						assertNamespaceBytes(t, node, name, "content of "+original.Name)
					}
					// The greatest live candidate must never disappear under a
					// synthetic tombstone from an earlier conflict resolution.
					assertNamespaceBytes(t, node, originals[2].Name, "content of "+originals[2].Name)
				}
			})
		}
	}
}

func TestNamespaceConcurrentPeerExchange(t *testing.T) {
	nodes, peers := namespaceNodes(t, 3)
	originals := []store.FileMeta{auditMeta("a", "parent", 200), auditMeta("a/b", "first child", 100), auditMeta("a/c", "winning child", 300)}
	bodies := []string{"parent", "first child", "winning child"}
	for i := range nodes {
		auditIndex(t, nodes[i], originals[i], bodies[i])
	}
	var group sync.WaitGroup
	failures := make(chan error, 6)
	for i, node := range nodes {
		for j, peer := range peers {
			if i == j {
				continue
			}
			group.Add(1)
			go func() {
				defer group.Done()
				_, err := node.syncOnce(context.Background(), peer.URL)
				if err != nil {
					failures <- err
				}
			}()
		}
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	convergeNamespace(t, nodes, peers)
	for _, node := range nodes {
		for i, original := range originals {
			name := original.Name
			meta, err := node.store.GetFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if meta == nil || meta.Deleted {
				name = store.ConflictCopyName(original.Name, original.Hash)
			}
			assertNamespaceBytes(t, node, name, bodies[i])
		}
		assertNamespaceBytes(t, node, originals[2].Name, bodies[2])
	}
}
