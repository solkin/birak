package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// identityNode is a syncer whose single peer is served by handler.
func identityNode(t *testing.T, id string, handler func(*store.Store) http.Handler) (*Syncer, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	peer := httptest.NewServer(handler(st))
	t.Cleanup(peer.Close)
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	node := New(st, w, root, id, []string{peer.URL}, nil, logger, Options{PollInterval: 10 * time.Millisecond, BatchLimit: 100, MaxConcurrentDownloads: 1})
	t.Cleanup(func() { node.client.CloseIdleConnections(); node.downloadClient.CloseIdleConnections() })
	return node, peer.URL
}

// The same peer list on every node makes each node its own peer. That URL is
// this process, not a failing peer: it used to stay unhealthy forever.
func TestPeerThatIsThisNodeIsRetired(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	node, url := identityNode(t, "node-a", func(st *store.Store) http.Handler {
		return server.New(st, t.TempDir(), "node-a", nil, server.Config{}, logger).Handler()
	})
	if _, err := node.syncOnce(context.Background(), url); !errors.Is(err, errSelfPeer) {
		t.Fatalf("own URL not recognized: %v", err)
	}
	done := make(chan struct{})
	go func() { node.pollPeer(context.Background(), url); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the node kept polling itself")
	}
	if peers := node.PeerStats(); len(peers) != 0 {
		t.Fatalf("own URL still reported as a peer: %+v", peers)
	}
}

// Another node with the same node_id is a real fault: it stays visible, as an
// error, a status flag and a metric, instead of a generic polling warning.
func TestDuplicateNodeIDIsReported(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	node, url := identityNode(t, "node-a", func(*store.Store) http.Handler {
		other, err := store.New(filepath.Join(t.TempDir(), "other.db"), logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { other.Close() })
		return server.New(other, t.TempDir(), "node-a", nil, server.Config{}, logger).Handler()
	})
	if _, err := node.syncOnce(context.Background(), url); !errors.Is(err, errDuplicateNodeID) {
		t.Fatalf("duplicate identity not recognized: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { node.pollPeer(ctx, url); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		peers := node.PeerStats()
		if len(peers) == 1 && peers[0].DuplicateNodeID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("duplicate node_id not reported: %+v", peers)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// Peer lists that disagree replicate in one direction only. The polled node
// sees a poller it does not poll, and says so.
func TestNodePollingWithoutBeingPolledIsReported(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var api *server.Server
	node, url := identityNode(t, "node-a", func(st *store.Store) http.Handler {
		other, err := store.New(filepath.Join(t.TempDir(), "b.db"), logger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { other.Close() })
		api = server.New(other, t.TempDir(), "node-b", nil, server.Config{}, logger)
		return api.Handler()
	})
	// node-b has no peers, yet node-a polls it: changes flow b → a only.
	api.SetStats(stubPeers{})
	if _, err := node.syncOnce(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	status := httptest.NewRecorder()
	api.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(status.Body.String(), `"unconfigured_pollers":["node-a"]`) {
		t.Fatalf("one-way replication not reported: %s", status.Body.String())
	}
	if ids, _ := node.PeerNodeIDs(); !ids["node-b"] {
		t.Fatalf("the polled peer's identity was not recorded: %v", ids)
	}

	// Until its own peers have answered, a node cannot tell a one-way list
	// from a peer it has not reached yet, and reports nothing.
	api.SetStats(stubPeers{incomplete: true})
	status = httptest.NewRecorder()
	api.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if strings.Contains(status.Body.String(), "unconfigured_pollers") {
		t.Fatalf("pollers reported before this node's peers answered: %s", status.Body.String())
	}
	if _, complete := node.PeerNodeIDs(); complete {
		t.Fatal("a peer list is complete before its peer was polled successfully")
	}
}

type stubPeers struct{ incomplete bool }

func (stubPeers) PeerStats() []server.PeerStatus { return nil }
func (p stubPeers) PeerNodeIDs() (map[string]bool, bool) {
	return map[string]bool{}, !p.incomplete
}
