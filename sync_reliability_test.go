package birak_test

// Regression tests for the replication guarantees: every scenario here is one
// in which a file used to get stuck on one node, or vanish, with nothing
// reporting it. Each test names the failure it locks out.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/syncer"
	"github.com/birak/birak/internal/watcher"
)

// scriptedPeer is a peer whose responses the test controls exactly, which is the
// only way to reproduce races that depend on the peer changing a file at a
// precise moment.
type scriptedPeer struct {
	// mu guards every field below: handlers run on their own goroutines while
	// the test rewrites the script mid-flight.
	mu sync.Mutex
	// changes is the peer's version stream.
	changes []store.FileMeta
	// manifest is what /manifest reports; nil means "same as changes".
	manifest []store.FileMeta
	// body is what /files returns for any name.
	body string
	// epoch identifies the peer's database incarnation.
	epoch string
	// fileStatus, when non-zero, is returned by /files instead of the body.
	// It is consumed once, modelling a transient failure.
	fileStatus int

	url       string
	downloads int
}

func (p *scriptedPeer) start(t *testing.T) string {
	t.Helper()
	if p.epoch == "" {
		p.epoch = "peer-epoch-1"
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	// snapshot returns a copy of the script so handlers never touch shared state
	// while serving.
	snapshot := func() (changes, manifest []store.FileMeta, body, epoch string) {
		p.mu.Lock()
		defer p.mu.Unlock()
		changes = append([]store.FileMeta(nil), p.changes...)
		if p.manifest != nil {
			manifest = append([]store.FileMeta(nil), p.manifest...)
		}
		return changes, manifest, p.body, p.epoch
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/changes", func(w http.ResponseWriter, r *http.Request) {
		changes, _, _, epoch := snapshot()
		var since, maxVer int64
		fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)
		out := []store.FileMeta{}
		for _, c := range changes {
			if c.Version > maxVer {
				maxVer = c.Version
			}
			if c.Version > since {
				out = append(out, c)
			}
		}
		w.Header().Set(server.HeaderEpoch, epoch)
		w.Header().Set(server.HeaderMaxVersion, fmt.Sprint(maxVer))
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		changes, manifest, _, epoch := snapshot()
		entries := manifest
		if entries == nil {
			entries = changes
		}
		after := r.URL.Query().Get("after")
		out := []store.FileMeta{}
		for _, e := range entries {
			if e.Name > after {
				out = append(out, e)
			}
		}
		w.Header().Set(server.HeaderEpoch, epoch)
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/meta/", func(w http.ResponseWriter, r *http.Request) {
		changes, manifest, _, epoch := snapshot()
		name := strings.TrimPrefix(r.URL.Path, "/meta/")
		entries := manifest
		if entries == nil {
			entries = changes
		}
		for i := range entries {
			if entries[i].Name == name {
				w.Header().Set(server.HeaderEpoch, epoch)
				json.NewEncoder(w).Encode(entries[i])
				return
			}
		}
		var newest *store.FileMeta
		for i := range changes {
			if changes[i].Name == name && (newest == nil || changes[i].Version > newest.Version) {
				newest = &changes[i]
			}
		}
		if newest != nil {
			w.Header().Set(server.HeaderEpoch, epoch)
			json.NewEncoder(w).Encode(newest)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.downloads++
		status := p.fileStatus
		p.fileStatus = 0
		body := p.body
		p.mu.Unlock()

		if status != 0 {
			http.Error(w, "unavailable", status)
			return
		}
		w.Header().Set(server.HeaderMode, "644")
		w.Write([]byte(body))
	})

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		mux.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	p.url = "http://" + ln.Addr().String()
	return p.url
}

// startSyncer wires a store, watcher and syncer against the given peers and
// returns the store and sync directory.
func startSyncer(t *testing.T, peers []string) (*store.Store, string) {
	t.Helper()
	base := t.TempDir()
	syncDir := filepath.Join(base, "sync")
	metaDir := filepath.Join(base, "meta")
	os.MkdirAll(syncDir, 0o755)
	os.MkdirAll(metaDir, 0o755)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(filepath.Join(metaDir, "birak.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	w := watcher.New(syncDir, st, logger, 100*time.Millisecond, 30*time.Second, defaultTestIgnore)
	syn := syncer.New(st, w, syncDir, "local", peers, defaultTestIgnore, logger, syncer.Options{
		PollInterval:           200 * time.Millisecond,
		BatchLimit:             1000,
		MaxConcurrentDownloads: 4,
		RepairInterval:         300 * time.Millisecond,
		ReconcileInterval:      1500 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
	go syn.Run(ctx)
	return st, syncDir
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A version that has already been superseded fails its hash check against the
// peer's current bytes. It must not hold the cursor: doing so froze every later
// change from that peer permanently.
func TestReliability_SupersededVersionDoesNotBlockStream(t *testing.T) {
	now := time.Now().UnixNano()
	peer := &scriptedPeer{
		body: "NEW", // the peer's disk already holds the newer content
		changes: []store.FileMeta{
			{Name: "a.txt", ModTime: now, Size: 3, Hash: sha256Hex("OLD"), Version: 1},
			{Name: "a.txt", ModTime: now + 1000, Size: 3, Hash: sha256Hex("NEW"), Version: 2},
			{Name: "b.txt", ModTime: now + 2000, Size: 3, Hash: sha256Hex("NEW"), Version: 3},
		},
	}
	peerURL := peer.start(t)
	st, syncDir := startSyncer(t, []string{peerURL})

	waitFor(t, 15*time.Second, "b.txt behind a superseded version", func() bool {
		return fileExists(syncDir, "b.txt")
	})
	if got := readFile(t, syncDir, "a.txt"); got != "NEW" {
		t.Fatalf("a.txt = %q, want the newest content", got)
	}
	if cur, _ := st.GetCursor(peerURL); cur != 3 {
		t.Fatalf("cursor = %d, want 3 — the stream must not be pinned", cur)
	}
}

// A peer that cannot serve a file it announced (half-finished write, differing
// ignore rules) used to have the change skipped and the cursor advanced, losing
// the file on this node forever.
func TestReliability_TransientNotFoundIsRetried(t *testing.T) {
	now := time.Now().UnixNano()
	peer := &scriptedPeer{
		body:       "NEW",
		fileStatus: http.StatusNotFound, // first download fails
		changes: []store.FileMeta{
			{Name: "a.txt", ModTime: now, Size: 3, Hash: sha256Hex("NEW"), Version: 1},
		},
	}
	peerURL := peer.start(t)
	_, syncDir := startSyncer(t, []string{peerURL})

	waitFor(t, 15*time.Second, "a.txt after a transient 404", func() bool {
		return fileExists(syncDir, "a.txt")
	})
	if got := readFile(t, syncDir, "a.txt"); got != "NEW" {
		t.Fatalf("a.txt = %q", got)
	}
}

// A peer that lost its meta directory restarts versioning at 1. A cursor left
// far ahead made every one of its changes invisible, with no error anywhere.
func TestReliability_PeerEpochChangeResetsCursor(t *testing.T) {
	now := time.Now().UnixNano()
	peer := &scriptedPeer{
		body:  "NEW",
		epoch: "epoch-after-rebuild",
		changes: []store.FileMeta{
			{Name: "after-reset.txt", ModTime: now, Size: 3, Hash: sha256Hex("NEW"), Version: 1},
		},
	}
	peerURL := peer.start(t)
	st, syncDir := startSyncer(t, []string{peerURL})

	// Pretend we had consumed 500 versions from this peer's previous incarnation.
	if err := st.SetPeerState(peerURL, store.PeerState{Version: 500, Epoch: "epoch-before-rebuild"}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 15*time.Second, "cursor reset after peer rebuild", func() bool {
		return fileExists(syncDir, "after-reset.txt")
	})
	ps, _ := st.GetPeerState(peerURL)
	if ps.Epoch != "epoch-after-rebuild" {
		t.Fatalf("epoch = %q, want the peer's new epoch", ps.Epoch)
	}
}

// A cursor that is legitimately up to date carries nothing, so anything already
// skipped is invisible to the change stream. Full reconciliation is the only
// mechanism that can still find it.
func TestReliability_ReconciliationRecoversSkippedFile(t *testing.T) {
	now := time.Now().UnixNano()
	entry := store.FileMeta{Name: "only-in-manifest.txt", ModTime: now, Size: 3, Hash: sha256Hex("NEW"), Version: 7}
	peer := &scriptedPeer{
		body: "NEW",
		// The version stream is empty: this file can only be discovered by
		// comparing manifests.
		changes:  []store.FileMeta{},
		manifest: []store.FileMeta{entry},
	}
	peerURL := peer.start(t)
	_, syncDir := startSyncer(t, []string{peerURL})

	waitFor(t, 20*time.Second, "reconciliation to pull a file the stream never carried", func() bool {
		return fileExists(syncDir, "only-in-manifest.txt")
	})
	if got := readFile(t, syncDir, "only-in-manifest.txt"); got != "NEW" {
		t.Fatalf("content = %q", got)
	}
}

// Reconciliation must propagate deletions too, not just content.
func TestReliability_ReconciliationAppliesMissedDeletion(t *testing.T) {
	peer := &scriptedPeer{body: "irrelevant", changes: []store.FileMeta{}}
	peerURL := peer.start(t)
	_, syncDir := startSyncer(t, []string{peerURL})

	writeFile(t, syncDir, "doomed.txt", "content")
	waitFor(t, 10*time.Second, "local file to be indexed", func() bool {
		return fileExists(syncDir, "doomed.txt")
	})

	// The peer holds a tombstone newer than our copy, reachable only through
	// the manifest.
	info, err := os.Stat(filepath.Join(syncDir, "doomed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	peer.mu.Lock()
	peer.manifest = []store.FileMeta{{
		Name:    "doomed.txt",
		ModTime: info.ModTime().UnixNano() + int64(time.Second),
		Deleted: true,
		Version: 3,
	}}
	peer.mu.Unlock()

	waitFor(t, 20*time.Second, "reconciliation to apply a missed deletion", func() bool {
		return !fileExists(syncDir, "doomed.txt")
	})
}

// Two nodes writing different bytes with the same mtime used to keep their own
// copy forever, with nothing detecting the split.
func TestReliability_EqualMtimeConvergesByHash(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1, addr2 := ln1.Addr().String(), ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	stamp := time.Now().Add(-time.Minute).Truncate(time.Second)
	writeFile(t, node1.syncDir, "conflict.txt", "FROM-NODE-1")
	writeFile(t, node2.syncDir, "conflict.txt", "FROM-NODE-2")
	os.Chtimes(filepath.Join(node1.syncDir, "conflict.txt"), stamp, stamp)
	os.Chtimes(filepath.Join(node2.syncDir, "conflict.txt"), stamp, stamp)

	waitFor(t, 25*time.Second, "nodes to converge on one copy", func() bool {
		a, errA := os.ReadFile(filepath.Join(node1.syncDir, "conflict.txt"))
		b, errB := os.ReadFile(filepath.Join(node2.syncDir, "conflict.txt"))
		return errA == nil && errB == nil && string(a) == string(b)
	})
}

// A replica created from a 0600 temp file is unreadable to any other user or
// sidecar, so the source mode has to travel with the content.
func TestReliability_ReplicaKeepsSourceMode(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1, addr2 := ln1.Addr().String(), ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	writeFile(t, node1.syncDir, "shared.txt", "data")
	os.Chmod(filepath.Join(node1.syncDir, "shared.txt"), 0o644)

	waitFor(t, 15*time.Second, "shared.txt to replicate", func() bool {
		return fileExists(node2.syncDir, "shared.txt")
	})

	src, _ := os.Stat(filepath.Join(node1.syncDir, "shared.txt"))
	dst, _ := os.Stat(filepath.Join(node2.syncDir, "shared.txt"))
	if src.Mode().Perm() != dst.Mode().Perm() {
		t.Fatalf("replica mode %v, source %v", dst.Mode().Perm(), src.Mode().Perm())
	}
}

// /status must expose per-peer progress: a stalled stream that is only visible
// as a file-count drift is a stall nobody notices.
func TestReliability_StatusReportsPeerProgress(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1, addr2 := ln1.Addr().String(), ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	writeFile(t, node1.syncDir, "x.txt", "data")

	var status server.StatusResponse
	waitFor(t, 15*time.Second, "peer status to become healthy", func() bool {
		resp, err := http.Get("http://" + addr1 + "/status")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		status = server.StatusResponse{}
		if json.NewDecoder(resp.Body).Decode(&status) != nil {
			return false
		}
		return len(status.Peers) == 1 && status.Peers[0].Healthy
	})
	if status.Epoch == "" {
		t.Fatal("/status must report the node epoch")
	}
	if status.Peers[0].Peer != "http://"+addr2 {
		t.Fatalf("unexpected peer in status: %+v", status.Peers[0])
	}
}

// With a cluster secret configured, an unauthenticated peer must not be able to
// read the change stream or any file.
func TestReliability_ClusterSecretRejectsUnauthenticated(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	root := t.TempDir()
	st, err := store.New(filepath.Join(t.TempDir(), "secret.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := server.New(st, root, "node", nil, server.Config{Secret: "s3cr3t"}, logger)
	ts := httptestServer(t, srv.Handler())

	for _, path := range []string{"/changes?since=0", "/manifest", "/files/x.txt", "/status"} {
		resp, err := http.Get(ts + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s returned %d without the secret, want 401", path, resp.StatusCode)
		}
	}

	// Liveness stays open so a probe needs no credentials.
	resp, err := http.Get(ts + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz returned %d, want 200", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts+"/changes?since=0", nil)
	req.Header.Set(server.HeaderSecret, "s3cr3t")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated request returned %d", resp.StatusCode)
	}
	if resp.Header.Get(server.HeaderEpoch) == "" {
		t.Fatal("cluster responses must carry the epoch header")
	}
}

func httptestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}
