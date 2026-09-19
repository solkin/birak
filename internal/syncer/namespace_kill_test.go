package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

func openNamespaceNode(t *testing.T, root, database, id string) *Syncer {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(database, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	node := New(st, w, root, id, nil, nil, logger, Options{PollInterval: time.Millisecond, BatchLimit: 100, MaxConcurrentDownloads: 4})
	t.Cleanup(func() { node.client.CloseIdleConnections(); node.downloadClient.CloseIdleConnections() })
	return node
}

func namespaceCutStates(direction string) (store.FileMeta, store.FileMeta, string, string) {
	oldName, newName := "a", "a/deep/child"
	if direction == "parent-wins" {
		oldName, newName = newName, oldName
	}
	return auditMeta(oldName, "preserve before replacement", 100), auditMeta(newName, "winning state", 200), "preserve before replacement", "winning state"
}

func TestNamespaceResolutionProcessCuts(t *testing.T) {
	if phase := os.Getenv("BIRAK_NAMESPACE_CUT"); phase != "" {
		root, database := os.Getenv("BIRAK_NAMESPACE_ROOT"), os.Getenv("BIRAK_NAMESPACE_DB")
		old, incoming, oldBody, _ := namespaceCutStates(os.Getenv("BIRAK_NAMESPACE_DIRECTION"))
		node := openNamespaceNode(t, root, database, "destination")
		auditIndex(t, node, old, oldBody)
		node.namespaceCheckpoint = func(step, name string) error {
			if step == phase {
				return syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			return nil
		}
		if err := node.applyChange(context.Background(), os.Getenv("BIRAK_NAMESPACE_PEER"), incoming); err != nil {
			t.Fatal(err)
		}
		t.Fatal("process did not reach requested cut")
	}
	for _, direction := range []string{"parent-wins", "child-wins"} {
		for _, phase := range []string{"copy-published", "preserved", "delete-intent", "unlinked", "resolved", "published"} {
			t.Run(direction+"/"+phase, func(t *testing.T) {
				old, incoming, oldBody, newBody := namespaceCutStates(direction)
				source, _ := auditSyncer(t)
				source.nodeID = "source"
				auditIndex(t, source, incoming, newBody)
				peer := httptest.NewServer(server.New(source.store, source.syncDir, source.nodeID, nil, server.Config{}, source.logger).Handler())
				defer peer.Close()
				root, database := t.TempDir(), filepath.Join(t.TempDir(), "node.db")
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestNamespaceResolutionProcessCuts$")
				cmd.Env = append(os.Environ(), "BIRAK_NAMESPACE_CUT="+phase, "BIRAK_NAMESPACE_ROOT="+root, "BIRAK_NAMESPACE_DB="+database, "BIRAK_NAMESPACE_DIRECTION="+direction, "BIRAK_NAMESPACE_PEER="+peer.URL)
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if ctx.Err() != nil || !errors.As(err, &exit) {
					t.Fatalf("missing SIGKILL: %v, context=%v\n%s", err, ctx.Err(), output)
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatalf("wrong termination: %v\n%s", err, output)
				}
				restarted := openNamespaceNode(t, root, database, "destination")
				// Run the real initial checksum/recovery scan with fresh memory and
				// reopened SQLite, before allowing any peer traffic.
				runCtx, stop := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- restarted.watcher.Run(runCtx) }()
				defer func() {
					stop()
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
				select {
				case <-restarted.watcher.Ready():
				case <-time.After(5 * time.Second):
					t.Fatal("recovery scan did not complete")
				}
				if err := restarted.applyChange(context.Background(), peer.URL, incoming); err != nil {
					t.Fatal(err)
				}
				assertNamespaceBytes(t, restarted, incoming.Name, newBody)
				assertNamespaceBytes(t, restarted, store.ConflictCopyName(old.Name, old.Hash), oldBody)
				for _, name := range []string{old.Name, incoming.Name, store.ConflictCopyName(old.Name, old.Hash)} {
					if pending, err := restarted.store.ReplicaIntent(name); err != nil || pending != nil {
						t.Fatalf("unrecovered intent %s: %+v %v", name, pending, err)
					}
				}
				third, _ := auditSyncer(t)
				third.nodeID = "third"
				restoredPeer := httptest.NewServer(server.New(restarted.store, root, restarted.nodeID, nil, server.Config{}, restarted.logger).Handler())
				defer restoredPeer.Close()
				thirdPeer := httptest.NewServer(server.New(third.store, third.syncDir, third.nodeID, nil, server.Config{}, third.logger).Handler())
				defer thirdPeer.Close()
				convergeNamespace(t, []*Syncer{source, restarted, third}, []*httptest.Server{peer, restoredPeer, thirdPeer})
				assertNamespaceBytes(t, third, incoming.Name, newBody)
				assertNamespaceBytes(t, third, store.ConflictCopyName(old.Name, old.Hash), oldBody)
			})
		}
	}
}
