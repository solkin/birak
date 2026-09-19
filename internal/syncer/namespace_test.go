package syncer

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

// Tombstones describe files. The same path may already be an implicit directory
// of newer children, including on a fresh node receiving a parallel batch.
func TestFileTombstoneDoesNotDeleteDirectoryOrGetStuck(t *testing.T) {
	for _, child := range []string{"", "a/child", "a/deep/child"} {
		t.Run("child="+child, func(t *testing.T) {
			s, _ := auditSyncer(t)
			if child == "" {
				if err := os.Mkdir(filepath.Join(s.syncDir, "a"), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				auditIndex(t, s, auditMeta(child, "keep", 200), "keep")
			}
			deletion := store.FileMeta{Name: "a", ModTime: 100, Clock: 100, Deleted: true}
			if err := s.applyChange(context.Background(), "http://peer", deletion); err != nil {
				t.Fatalf("file tombstone cannot be acknowledged beside directory: %v", err)
			}
			info, err := os.Stat(filepath.Join(s.syncDir, "a"))
			if err != nil || !info.IsDir() {
				t.Fatalf("file tombstone removed a directory: %v", err)
			}
			if child != "" {
				body, err := os.ReadFile(filepath.Join(s.syncDir, child))
				if err != nil || string(body) != "keep" {
					t.Fatalf("child changed: %q %v", body, err)
				}
			}
			meta, err := s.store.GetFile("a")
			if err != nil || meta == nil || !meta.Deleted {
				t.Fatalf("tombstone not retained: %+v %v", meta, err)
			}
			if err = s.applyChange(context.Background(), "http://peer", deletion); err != nil {
				t.Fatalf("replay failed: %v", err)
			}
		})
	}
}

func TestFreshNodeReceivesNamespaceTransitionsInAnyOrder(t *testing.T) {
	source, _ := auditSyncer(t)
	auditIndex(t, source, auditMeta("a/child", "child", 200), "child")
	auditIndex(t, source, auditMeta("b", "parent", 200), "parent")
	for _, name := range []string{"a", "b/deep/old"} {
		if _, err := source.store.PutRemote(store.FileMeta{Name: name, ModTime: 100, Clock: 100, Deleted: true}); err != nil {
			t.Fatal(err)
		}
	}
	changes, err := source.store.ListManifest("", 100)
	if err != nil || len(changes) != 4 {
		t.Fatalf("source manifest: %+v %v", changes, err)
	}
	peer := httptest.NewServer(server.New(source.store, source.syncDir, "namespace-source", nil, server.Config{}, source.logger).Handler())
	defer peer.Close()
	check := func(t *testing.T, order []store.FileMeta, concurrency int) {
		s, _ := auditSyncer(t)
		s.opts.MaxConcurrentDownloads = concurrency
		applied, err := s.applyBatch(context.Background(), peer.URL, order)
		if err != nil || applied != len(order) {
			t.Fatalf("batch stuck: applied=%d/%d error=%v", applied, len(order), err)
		}
		for _, want := range changes {
			got, err := s.store.GetFile(want.Name)
			if err != nil || got == nil || store.CompareState(got, &want) != 0 {
				t.Fatalf("state mismatch: got=%+v want=%+v error=%v", got, want, err)
			}
		}
		for name, want := range map[string]string{"a/child": "child", "b": "parent"} {
			if body, err := os.ReadFile(filepath.Join(s.syncDir, name)); err != nil || string(body) != want {
				t.Fatalf("bytes for %s: %q %v", name, body, err)
			}
		}
		// Replay of queued tombstones must drain, even while a directory or a
		// regular ancestor now occupies part of their former file path.
		for _, meta := range changes {
			if meta.Deleted {
				if err := s.store.EnqueueChange(peer.URL, meta, "retry after reconnect"); err != nil {
					t.Fatal(err)
				}
			}
		}
		items, err := s.store.DueRepairs(peer.URL, 100)
		if err != nil || len(items) != 2 {
			t.Fatalf("queued retries: %+v %v", items, err)
		}
		for _, item := range items {
			s.repairOne(context.Background(), peer.URL, item)
		}
		stats, err := s.store.RepairQueueStats()
		if err != nil || stats.Total != 0 {
			t.Fatalf("repair did not drain: %+v %v", stats, err)
		}
	}
	// All 24 serial permutations give deterministic coverage of arrival order.
	var permutations func(int)
	permutations = func(start int) {
		if start == len(changes) {
			order := append([]store.FileMeta(nil), changes...)
			t.Run(fmt.Sprintf("order=%s,%s,%s,%s", order[0].Name, order[1].Name, order[2].Name, order[3].Name), func(t *testing.T) { check(t, order, 1) })
			return
		}
		for i := start; i < len(changes); i++ {
			changes[start], changes[i] = changes[i], changes[start]
			permutations(start + 1)
			changes[start], changes[i] = changes[i], changes[start]
		}
	}
	permutations(0)
	t.Run("parallel", func(t *testing.T) { check(t, changes, len(changes)) })
}
func TestFileTombstoneBelowRegularAncestorIsAlreadyAbsent(t *testing.T) {
	s, _ := auditSyncer(t)
	auditIndex(t, s, auditMeta("a", "keep", time.Now().UnixNano()), "keep")
	deletion := store.FileMeta{Name: "a/deep/old", ModTime: 100, Deleted: true}
	if err := s.applyChange(context.Background(), "http://peer", deletion); err != nil {
		t.Fatalf("already absent child stuck in repair: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(s.syncDir, "a"))
	if err != nil || string(body) != "keep" {
		t.Fatalf("ancestor changed: %q %v", body, err)
	}
	meta, err := s.store.GetFile(deletion.Name)
	if err != nil || meta == nil || !meta.Deleted {
		t.Fatalf("lost tombstone: %+v %v", meta, err)
	}
}

// Structural conflicts converge and keep the displaced generation as a normal
// replicated file, including on a third node that never saw the original bytes.
func TestConcurrentFileDirectoryConflictConvergesWithoutDataLoss(t *testing.T) {
	for _, parentWins := range []bool{false, true} {
		t.Run(fmt.Sprint(parentWins), func(t *testing.T) {
			nodes, peers := namespaceNodes(t, 3)
			parentTime, childTime := int64(100), int64(200)
			if parentWins {
				parentTime, childTime = childTime, parentTime
			}
			parent := auditMeta("a", "parent bytes", parentTime)
			child := auditMeta("a/child", "child bytes", childTime)
			auditIndex(t, nodes[0], parent, "parent bytes")
			auditIndex(t, nodes[1], child, "child bytes")
			convergeNamespace(t, nodes, peers)
			loser, winner, loserBody, winnerBody := parent, child, "parent bytes", "child bytes"
			if parentWins {
				loser, winner, loserBody, winnerBody = child, parent, "child bytes", "parent bytes"
			}
			for _, node := range nodes {
				assertNamespaceBytes(t, node, winner.Name, winnerBody)
				assertNamespaceBytes(t, node, store.ConflictCopyName(loser.Name, loser.Hash), loserBody)
				gone, err := node.store.GetFile(loser.Name)
				if err != nil || gone == nil || !gone.Deleted || gone.SupersededBy != winner.Name {
					t.Fatalf("unresolved loser: %+v %v", gone, err)
				}
			}
		})
	}
}
