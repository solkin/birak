package syncer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

func TestNamespacePreservationFailureKeepsOriginal(t *testing.T) {
	for _, obstacle := range []string{"copy-occupied", "copy-directory", "ignored-child", "active-writer", "corrupt-child"} {
		t.Run(obstacle, func(t *testing.T) {
			nodes, peers := namespaceNodes(t, 2)
			incoming := auditMeta("a", "winner", 300)
			old := auditMeta("a/child", "preserve", 100)
			auditIndex(t, nodes[0], incoming, "winner")
			auditIndex(t, nodes[1], old, "preserve")
			local := nodes[1]
			copyPath := filepath.Join(local.syncDir, store.ConflictCopyName(old.Name, old.Hash))
			switch obstacle {
			case "copy-occupied":
				if err := os.WriteFile(copyPath, []byte("unrelated user bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "copy-directory":
				if err := os.Mkdir(copyPath, 0o700); err != nil {
					t.Fatal(err)
				}
			case "ignored-child":
				local.ignorePatterns = []string{"private"}
				if err := os.WriteFile(filepath.Join(local.syncDir, "a/private"), []byte("ignored bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "active-writer":
				f, _, err := fileops.OpenWriter(local.syncDir, filepath.Join(local.syncDir, old.Name), os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer fileops.AbortWriter(local.syncDir, f)
			case "corrupt-child":
				name := filepath.Join(local.syncDir, old.Name)
				info, err := os.Stat(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte("CORRUPT!"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			if err := local.applyChange(context.Background(), peers[0].URL, incoming); err == nil {
				t.Fatal("unsafe resolution succeeded")
			}
			want := "preserve"
			if obstacle == "corrupt-child" {
				want = "CORRUPT!"
			}
			if body, err := os.ReadFile(filepath.Join(local.syncDir, old.Name)); err != nil || string(body) != want {
				t.Fatalf("original lost: %q %v", body, err)
			}
			if meta, err := local.store.GetFile(old.Name); err != nil || meta == nil || meta.Deleted || meta.Hash != old.Hash {
				t.Fatalf("original metadata lost: %+v %v", meta, err)
			}
			if obstacle == "copy-occupied" {
				if body, err := os.ReadFile(copyPath); err != nil || string(body) != "unrelated user bytes" {
					t.Fatalf("copy collision destroyed user data: %q %v", body, err)
				}
			}
		})
	}
}

func TestSupersededDeletionPreservesUnseenLocalGeneration(t *testing.T) {
	s, _ := auditSyncer(t)
	local := auditMeta("a", "unique offline bytes", 150)
	auditIndex(t, s, local, "unique offline bytes")
	winner := auditMeta("a/child", "winner", 200)
	// The resolver on another peer has never seen this local generation.
	deletion := store.Superseded(local.Name, winner)
	if err := s.applyChange(context.Background(), "http://offline", deletion); err != nil {
		t.Fatal(err)
	}
	assertNamespaceBytes(t, s, store.ConflictCopyName(local.Name, local.Hash), "unique offline bytes")
	if _, err := os.Stat(filepath.Join(s.syncDir, local.Name)); !os.IsNotExist(err) {
		t.Fatalf("loser remains: %v", err)
	}
}

func TestEmptyDirectoryCanBecomeReplicatedFile(t *testing.T) {
	nodes, peers := namespaceNodes(t, 2)
	meta := auditMeta("a", "file", 100)
	auditIndex(t, nodes[0], meta, "file")
	if err := os.MkdirAll(filepath.Join(nodes[1].syncDir, "a/empty/deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	convergeNamespace(t, nodes, peers)
	assertNamespaceBytes(t, nodes[1], "a", "file")
}

func TestMalformedNamespaceMetadataRejected(t *testing.T) {
	s, _ := auditSyncer(t)
	winner := auditMeta("a/child", "winner", 100)
	valid := store.Superseded("a", winner)
	if err := s.validateChange(valid); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*store.FileMeta){
		func(m *store.FileMeta) { m.Deleted = false },
		func(m *store.FileMeta) { m.Size = 1 },
		func(m *store.FileMeta) { m.Hash = "invalid" },
		func(m *store.FileMeta) { m.SupersededBy = "a" },
		func(m *store.FileMeta) { m.SupersededBy = "unrelated" },
		func(m *store.FileMeta) { m.SupersededBy = "a/../../escape" },
		func(m *store.FileMeta) { m.ConflictOf = "a" },
	}
	for _, mutate := range mutations {
		meta := valid
		mutate(&meta)
		if err := s.validateChange(meta); err == nil {
			t.Fatalf("accepted malformed state: %+v", meta)
		}
	}
	copy := store.ConflictCopy(winner)
	if err := s.validateChange(copy); err != nil {
		t.Fatal(err)
	}
	copy.Name = "unrelated"
	if err := s.validateChange(copy); err == nil {
		t.Fatal("accepted forged conflict copy name")
	}
}

func TestNamespaceFailureRetriesAfterWriterCloses(t *testing.T) {
	source, _ := auditSyncer(t)
	local, _ := auditSyncer(t)
	meta := auditMeta("a/child", "winner", 300)
	auditIndex(t, source, meta, "winner")
	auditIndex(t, local, auditMeta("a", "old", 100), "old")
	peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
	defer peer.Close()
	f, closeWriter, err := fileops.OpenWriter(local.syncDir, filepath.Join(local.syncDir, "a"), os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fileops.AbortWriter(local.syncDir, f)
	if err := local.applyChange(context.Background(), peer.URL, meta); !errors.Is(err, fileops.ErrBusy) {
		t.Fatalf("writer not protected: %v", err)
	}
	if err := closeWriter(); err != nil {
		t.Fatal(err)
	}
	if err := local.applyChange(context.Background(), peer.URL, meta); err != nil {
		t.Fatal(err)
	}
	assertNamespaceBytes(t, local, meta.Name, "winner")
}

func TestSupersededDeletionDoesNotCleanIgnoredSiblings(t *testing.T) {
	s, _ := auditSyncer(t)
	s.ignorePatterns = []string{"private"}
	old := auditMeta("a/child", "preserve", 100)
	auditIndex(t, s, old, "preserve")
	private := filepath.Join(s.syncDir, "a/private")
	if err := os.WriteFile(private, []byte("keep ignored bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.applyChange(context.Background(), "http://offline", store.Superseded(old.Name, auditMeta("a", "winner", 200))); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(private); err != nil || string(body) != "keep ignored bytes" {
		t.Fatalf("ignored sibling lost: %q %v", body, err)
	}
}

func TestNamespaceCancellationAfterPreservationKeepsOriginal(t *testing.T) {
	nodes, peers := namespaceNodes(t, 2)
	incoming, old := auditMeta("a/child", "winner", 200), auditMeta("a", "preserve", 100)
	auditIndex(t, nodes[0], incoming, "winner")
	auditIndex(t, nodes[1], old, "preserve")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nodes[1].namespaceCheckpoint = func(step, name string) error {
		if step == "preserved" {
			cancel()
		}
		return nil
	}
	if err := nodes[1].applyChange(ctx, peers[0].URL, incoming); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	assertNamespaceBytes(t, nodes[1], "a", "preserve")
	assertNamespaceBytes(t, nodes[1], store.ConflictCopyName(old.Name, old.Hash), "preserve")
	nodes[1].namespaceCheckpoint = nil
	convergeNamespace(t, nodes, peers)
	assertNamespaceBytes(t, nodes[1], incoming.Name, "winner")
}

func TestNamespaceRepairSurvivesOfflineAndDatabaseReopen(t *testing.T) {
	source, _ := auditSyncer(t)
	source.nodeID = "source"
	local, database := auditSyncer(t)
	local.nodeID = "destination"
	old, incoming := auditMeta("a", "offline generation", 100), auditMeta("a/child", "winner", 200)
	auditIndex(t, local, old, "offline generation")
	auditIndex(t, source, incoming, "winner")
	peer := httptest.NewServer(server.New(source.store, source.syncDir, source.nodeID, nil, server.Config{}, source.logger).Handler())
	defer peer.Close()
	local.downloadClient.Transport = auditTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("injected connection loss") })
	if _, err := local.syncOnce(context.Background(), peer.URL); err != nil {
		t.Fatal(err)
	}
	state, err := local.store.GetPeerState(peer.URL)
	if err != nil || state.Version == 0 {
		t.Fatalf("failure not durably accepted: %+v %v", state, err)
	}
	if err := local.store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := openNamespaceNode(t, local.syncDir, database, local.nodeID)
	items, err := restarted.store.DueRepairs(peer.URL, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("lost queue after reopen: %+v %v", items, err)
	}
	restarted.repairOne(context.Background(), peer.URL, items[0])
	if stats, err := restarted.store.RepairQueueStats(); err != nil || stats.Total != 0 {
		t.Fatalf("repair not completed: %+v %v", stats, err)
	}
	assertNamespaceBytes(t, restarted, incoming.Name, "winner")
	assertNamespaceBytes(t, restarted, store.ConflictCopyName(old.Name, old.Hash), "offline generation")
	before := namespaceManifest(t, restarted)
	// The original stale state and duplicate winning delivery must not recreate
	// the loser or mint another copy/clock after a successful repair.
	for _, meta := range []store.FileMeta{old, incoming, old, incoming} {
		if err := restarted.applyChange(context.Background(), peer.URL, meta); err != nil {
			t.Fatal(err)
		}
	}
	after := namespaceManifest(t, restarted)
	if len(before) != len(after) {
		t.Fatalf("replay generated extra states: before=%+v after=%+v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("replay changed generation: before=%+v after=%+v", before[i], after[i])
		}
	}
}
