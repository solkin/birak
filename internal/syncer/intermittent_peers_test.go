package syncer

// Nodes that restart, drop off the network, or sit behind slow links. Each of
// these used to cost a full replay, a full re-download, or a stalled queue.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// testPeer serves a node's real cluster API and records what was asked of it.
type testPeer struct {
	*httptest.Server
	handler atomic.Pointer[http.Handler]
	meta    atomic.Int64
	files   atomic.Int64
	// cutAfter, when positive, aborts the next /files response after that
	// many body bytes, the way a dropped connection does.
	cutAfter atomic.Int64
	// rewrite, when set, edits a /files request before it is served.
	rewrite atomic.Pointer[func(*http.Request)]

	mu     sync.Mutex
	ranges []string // Range and If-Range of every /files request
}

func newTestPeer(t *testing.T, node *Syncer) *testPeer {
	t.Helper()
	p := &testPeer{}
	p.serve(node)
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/meta/"):
			p.meta.Add(1)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			p.files.Add(1)
			if fn := p.rewrite.Load(); fn != nil {
				(*fn)(r)
			}
			p.mu.Lock()
			p.ranges = append(p.ranges, r.Header.Get("Range")+"|"+r.Header.Get("If-Range"))
			p.mu.Unlock()
			if cut := p.cutAfter.Swap(0); cut > 0 {
				w = &cuttingWriter{ResponseWriter: w, left: cut}
			}
		}
		(*p.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

// serve switches the peer to another process's view of a node, as a restart does.
func (p *testPeer) serve(node *Syncer) {
	h := server.New(node.store, node.syncDir, "peer", nil, server.Config{}, node.logger).Handler()
	p.handler.Store(&h)
}

func (p *testPeer) fileRequests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ranges...)
}

type cuttingWriter struct {
	http.ResponseWriter
	left int64
}

func (c *cuttingWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > c.left {
		b = b[:c.left]
	}
	n, err := c.ResponseWriter.Write(b)
	c.left -= int64(n)
	if c.left <= 0 {
		c.ResponseWriter.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	return n, err
}

// reopen models a restart of node: a new process, the same disk and database.
func reopen(t *testing.T, node *Syncer, dbPath string) *Syncer {
	t.Helper()
	node.client.CloseIdleConnections()
	node.downloadClient.CloseIdleConnections()
	if err := node.store.Close(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := watcher.New(node.syncDir, st, logger, time.Millisecond, time.Hour, nil)
	restarted := New(st, w, node.syncDir, node.nodeID, node.peers, nil, logger, node.opts)
	t.Cleanup(func() { restarted.client.CloseIdleConnections(); restarted.downloadClient.CloseIdleConnections() })
	return restarted
}

// pollUntilQuiet reads a peer's stream until it has nothing more to offer.
func pollUntilQuiet(t *testing.T, s *Syncer, peerURL string) {
	t.Helper()
	for range 1000 {
		n, err := s.syncOnce(context.Background(), peerURL)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("stream never ran dry")
}

// A peer restart resets every other node's cursor and replays its whole
// stream. For names a node already holds, that replay must cost a page read
// and an index lookup, not a queued row, a /meta request and a delete each.
func TestPeerRestartReplayOfConvergedNamesQueuesNothing(t *testing.T) {
	source, sourceDB := auditSyncer(t)
	dest, _ := auditSyncer(t)
	dest.opts.BatchLimit = 64
	const names = 300
	for i := range names {
		body := fmt.Sprintf("body %d", i)
		auditIndex(t, source, auditMeta(fmt.Sprintf("dir-%d/file-%03d.png", i%7, i), body, int64(100+i)), body)
	}
	peer := newTestPeer(t, source)

	pollUntilQuiet(t, dest, peer.URL)
	drainQueued(t, dest, peer.URL)
	if count, _ := dest.store.FileCount(); count != names {
		t.Fatalf("dest holds %d files before the restart, want %d", count, names)
	}
	enqueued, metas, files := dest.store.RepairEnqueued(), peer.meta.Load(), peer.files.Load()

	// The source restarts: a new incarnation, so every cursor on it resets.
	before, _ := dest.store.GetPeerState(peer.URL)
	source = reopen(t, source, sourceDB)
	peer.serve(source)
	pollUntilQuiet(t, dest, peer.URL)
	after, _ := dest.store.GetPeerState(peer.URL)
	if after.Epoch == before.Epoch || after.Version < int64(names) {
		t.Fatalf("the replay did not happen: before=%+v after=%+v", before, after)
	}

	if got := dest.store.RepairEnqueued() - enqueued; got != 0 {
		t.Fatalf("replaying %d converged names wrote %d repair rows", names, got)
	}
	if pending, _ := dest.store.PendingRepairCount(peer.URL); pending != 0 {
		t.Fatalf("replay left %d rows queued", pending)
	}
	if got := peer.meta.Load() - metas; got != 0 {
		t.Fatalf("replay asked the peer for metadata %d times", got)
	}
	if got := peer.files.Load() - files; got != 0 {
		t.Fatalf("replay downloaded %d files", got)
	}
}

// A node that has just applied a file sees it again in the stream of the node
// it came from — and in every other peer's stream that applied it too.
func TestAppliedFileIsNotQueuedAgainFromAnotherStream(t *testing.T) {
	origin, _ := auditSyncer(t)
	replica, _ := auditSyncer(t)
	for i := range 50 {
		body := fmt.Sprintf("origin %d", i)
		auditIndex(t, origin, auditMeta(fmt.Sprintf("img-%02d.png", i), body, int64(500+i)), body)
	}
	originPeer := newTestPeer(t, origin)
	replicaPeer := newTestPeer(t, replica)

	pollUntilQuiet(t, replica, originPeer.URL)
	drainQueued(t, replica, originPeer.URL)

	// The replica's stream now carries all 50 names. The origin reads it.
	pollUntilQuiet(t, origin, replicaPeer.URL)
	if got := origin.store.RepairEnqueued(); got != 0 {
		t.Fatalf("the origin queued %d of its own files back", got)
	}
	if replicaPeer.meta.Load() != 0 || replicaPeer.files.Load() != 0 {
		t.Fatalf("echo-back reached the peer: meta=%d files=%d", replicaPeer.meta.Load(), replicaPeer.files.Load())
	}
}

// Two peers announce the same file before either copy is applied. Whichever
// row runs second finds the state already here and needs nothing from its peer.
func TestQueuedStateAlreadyAppliedResolvesWithoutAskingThePeer(t *testing.T) {
	dest, _ := auditSyncer(t)
	meta := auditMeta("shared.png", "shared", 700)
	auditIndex(t, dest, meta, "shared")
	unused := newTestPeer(t, dest)
	if err := dest.store.EnqueueChange(unused.URL, meta, "polled"); err != nil {
		t.Fatal(err)
	}
	drainQueued(t, dest, unused.URL)
	if pending, _ := dest.store.PendingRepairCount(unused.URL); pending != 0 {
		t.Fatalf("row was not resolved: %d pending", pending)
	}
	if unused.meta.Load() != 0 || unused.files.Load() != 0 {
		t.Fatalf("resolved row still asked the peer: meta=%d files=%d", unused.meta.Load(), unused.files.Load())
	}
}

// A replica still catching up reconciles every second and finds every name it
// has not fetched yet. Those rows are already queued; passing over them again
// must not rewrite them.
func TestRepeatedReconciliationDoesNotRewriteQueuedRows(t *testing.T) {
	dest, _ := auditSyncer(t)
	page := make([]store.FileMeta, 200)
	for i := range page {
		page[i] = auditMeta(fmt.Sprintf("pending-%03d.png", i), "x", int64(100+i))
	}
	if n, err := dest.reconcilePage("http://peer", page); err != nil || n != len(page) {
		t.Fatalf("first pass queued %d (err=%v)", n, err)
	}
	written := dest.store.RepairEnqueued()
	for range 5 {
		if n, err := dest.reconcilePage("http://peer", page); err != nil || n != 0 {
			t.Fatalf("a repeated pass rewrote %d rows (err=%v)", n, err)
		}
	}
	if dest.store.RepairEnqueued() != written {
		t.Fatal("repeated passes wrote to the queue")
	}
}

// Work queued before a restart starts at once, not a repair interval later.
func TestQueuedWorkStartsWithTheSyncer(t *testing.T) {
	source, _ := auditSyncer(t)
	meta := indexBody(t, source, "queued.bin", []byte("queued before the restart"), 1000)
	handler := server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/changes" {
			// Nothing new in the stream: only the queue knows about this file.
			w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
			w.Write([]byte("[]"))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(peer.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "dest.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	w := watcher.New(dir, st, logger, time.Millisecond, time.Hour, nil)
	dest := New(st, w, dir, "dest", []string{peer.URL}, nil, logger, Options{
		PollInterval: time.Hour, RepairInterval: time.Hour, MaxConcurrentDownloads: 1,
	})
	t.Cleanup(func() { dest.client.CloseIdleConnections(); dest.downloadClient.CloseIdleConnections() })
	if err := st.EnqueueChange(peer.URL, meta, "polled"); err != nil {
		t.Fatal(err)
	}
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	watching, syncing := make(chan struct{}), make(chan struct{})
	go func() { defer close(watching); w.Run(ctx) }()
	go func() { defer close(syncing); dest.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-syncing; <-watching })
	awaitRepairCondition(t, "the queued file", func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "queued.bin"))
		return err == nil && string(data) == "queued before the restart"
	})
}

// --- resumable downloads ----------------------------------------------------

func randomBody(size int, seed int64) []byte {
	body := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(body)
	return body
}

func indexBody(t *testing.T, s *Syncer, name string, body []byte, stamp int64) store.FileMeta {
	t.Helper()
	sum := sha256.Sum256(body)
	meta := store.FileMeta{Name: name, Hash: hex.EncodeToString(sum[:]), Size: int64(len(body)), ModTime: stamp, Version: 1}
	path := filepath.Join(s.syncDir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(0, stamp), time.Unix(0, stamp)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.PutFile(name, stamp, meta.Size, meta.Hash, false); err != nil {
		t.Fatal(err)
	}
	return meta
}

func scratchFiles(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".birak-tmp-") {
			found = append(found, path)
		}
		return nil
	})
	return found
}

// repairOnce runs the queued row for name once, as the apply loop would.
func repairOnce(t *testing.T, s *Syncer, peerURL, name string) {
	t.Helper()
	items, err := s.store.DueRepairs(peerURL, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Name == name {
			if err := s.repairOne(context.Background(), peerURL, item); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("%s is not queued", name)
}

// due makes every queued row due now, skipping the retry backoff.
func due(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE repair_queue SET next_attempt=0"); err != nil {
		t.Fatal(err)
	}
}

func assertPublished(t *testing.T, s *Syncer, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(s.syncDir, filepath.FromSlash(name)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s: %d bytes published, want %d (err=%v)", name, len(got), len(want), err)
	}
	if left := scratchFiles(t, s.syncDir); len(left) != 0 {
		t.Fatalf("scratch left behind: %v", left)
	}
	if records, _ := s.store.PartialDownloads(); len(records) != 0 {
		t.Fatalf("partial download records left behind: %+v", records)
	}
}

func TestInterruptedDownloadResumesWhereItStopped(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "after a network error", true: "after a restart"}[restart], func(t *testing.T) {
			source, _ := auditSyncer(t)
			dest, destDB := auditSyncer(t)
			body := randomBody(3<<20, 1)
			meta := indexBody(t, source, "apps/big.apk", body, 1000)
			peer := newTestPeer(t, source)
			if err := dest.store.EnqueueChange(peer.URL, meta, "polled"); err != nil {
				t.Fatal(err)
			}

			cut := int64(len(body) / 2)
			peer.cutAfter.Store(cut)
			repairOnce(t, dest, peer.URL, meta.Name)
			records, err := dest.store.PartialDownloads()
			if err != nil || len(records) != 1 || records[0].Verified != cut {
				t.Fatalf("interrupted transfer kept %+v (err=%v), want %d bytes", records, err, cut)
			}
			if files := scratchFiles(t, dest.syncDir); len(files) != 1 {
				t.Fatalf("scratch files after the cut: %v", files)
			}

			if restart {
				dest = reopen(t, dest, destDB)
				// Startup removes every scratch file nobody owns.
				gateway.SweepTempFiles(dest.syncDir, 0, dest.logger)
				if files := scratchFiles(t, dest.syncDir); len(files) != 1 {
					t.Fatalf("startup cleanup removed the partial download: %v", files)
				}
			}
			due(t, destDB)
			repairOnce(t, dest, peer.URL, meta.Name)

			requests := peer.fileRequests()
			want := fmt.Sprintf("bytes=%d-|%s", cut, strconv.Quote(meta.Hash))
			if len(requests) != 2 || requests[0] != "|" || requests[1] != want {
				t.Fatalf("file requests %q, want a plain one and then %q", requests, want)
			}
			if count, saved := dest.ResumedDownloads(); count != 1 || saved != cut {
				t.Fatalf("resumed=%d saved=%d", count, saved)
			}
			assertPublished(t, dest, meta.Name, body)
			if pending, _ := dest.store.PendingRepairCount(peer.URL); pending != 0 {
				t.Fatalf("%d rows still queued", pending)
			}
		})
	}
}

// The prefix kept on disk is rehashed before it is trusted. A prefix that no
// longer matches its record is a full download, never a corrupt file.
func TestCorruptedPartialDownloadFallsBackToAFullDownload(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, destDB := auditSyncer(t)
	body := randomBody(2<<20, 2)
	meta := indexBody(t, source, "big.bin", body, 1000)
	peer := newTestPeer(t, source)
	if err := dest.store.EnqueueChange(peer.URL, meta, "polled"); err != nil {
		t.Fatal(err)
	}
	peer.cutAfter.Store(1 << 20)
	repairOnce(t, dest, peer.URL, meta.Name)
	files := scratchFiles(t, dest.syncDir)
	if len(files) != 1 {
		t.Fatalf("scratch files: %v", files)
	}
	f, err := os.OpenFile(files[0], os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("rot"), 4096); err != nil {
		t.Fatal(err)
	}
	f.Close()

	due(t, destDB)
	repairOnce(t, dest, peer.URL, meta.Name)
	if requests := peer.fileRequests(); len(requests) != 2 || requests[1] != "|" {
		t.Fatalf("file requests %q: a damaged prefix was resumed", requests)
	}
	assertPublished(t, dest, meta.Name, body)
}

// The peer rewrote the file while this node was away. If-Range makes it send
// the new file whole, and the transfer is dropped instead of being stitched
// onto the old prefix.
func TestPartialDownloadOfReplacedContentIsDropped(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, destDB := auditSyncer(t)
	body := randomBody(2<<20, 3)
	meta := indexBody(t, source, "big.bin", body, 1000)
	peer := newTestPeer(t, source)
	if err := dest.store.EnqueueChange(peer.URL, meta, "polled"); err != nil {
		t.Fatal(err)
	}
	peer.cutAfter.Store(1 << 20)
	repairOnce(t, dest, peer.URL, meta.Name)

	replacement := randomBody(2<<20, 4)
	newMeta := indexBody(t, source, "big.bin", replacement, 2000)
	due(t, destDB)
	// The queued row still names the old content; the peer's /meta names the
	// new one, and a download of it must not reuse the old prefix.
	repairOnce(t, dest, peer.URL, meta.Name)
	requests := peer.fileRequests()
	if len(requests) != 2 || requests[1] != "|" {
		t.Fatalf("file requests %q: old prefix offered for new content", requests)
	}
	assertPublished(t, dest, meta.Name, replacement)
	if local, _ := dest.store.GetFile(meta.Name); local == nil || local.Hash != newMeta.Hash {
		t.Fatalf("index holds %+v", local)
	}
}

// The same, seen by a transfer that still believes in the old content: If-Range
// names the old hash, the peer sends its new file whole, and its ETag ends the
// transfer before the bytes are read.
func TestResumeAgainstChangedContentIsRefusedByIfRange(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	meta := indexBody(t, source, "big.bin", randomBody(2<<20, 8), 1000)
	peer := newTestPeer(t, source)
	download := func() error {
		unlock := dest.paths.lock(meta.Name)
		defer unlock()
		return dest.downloadAndApply(context.Background(), peer.URL, meta)
	}
	peer.cutAfter.Store(1 << 20)
	if err := download(); err == nil {
		t.Fatal("cut transfer succeeded")
	}
	indexBody(t, source, "big.bin", randomBody(2<<20, 9), 2000)
	if err := download(); !errors.Is(err, errContentChanged) {
		t.Fatalf("download of replaced content: %v", err)
	}
	requests := peer.fileRequests()
	if want := fmt.Sprintf("bytes=%d-|%s", 1<<20, strconv.Quote(meta.Hash)); len(requests) != 2 || requests[1] != want {
		t.Fatalf("file requests %q, want a resume naming the old content", requests)
	}
	if records, _ := dest.store.PartialDownloads(); len(records) != 0 {
		t.Fatalf("partial of replaced content kept: %+v", records)
	}
	if left := scratchFiles(t, dest.syncDir); len(left) != 0 {
		t.Fatalf("scratch left behind: %v", left)
	}
	if _, err := os.Stat(filepath.Join(dest.syncDir, "big.bin")); !os.IsNotExist(err) {
		t.Fatalf("replaced content was published from a stale request: %v", err)
	}
}

// A peer that answers a resumed request with the whole file — an older build,
// or one whose copy no longer matches its index — is downloaded from scratch.
func TestPeerAnsweringAResumeWithTheWholeFileStartsOver(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, destDB := auditSyncer(t)
	body := randomBody(2<<20, 5)
	meta := indexBody(t, source, "big.bin", body, 1000)
	peer := newTestPeer(t, source)
	if err := dest.store.EnqueueChange(peer.URL, meta, "polled"); err != nil {
		t.Fatal(err)
	}
	peer.cutAfter.Store(1 << 20)
	repairOnce(t, dest, peer.URL, meta.Name)

	ignoreRange := func(r *http.Request) { r.Header.Del("If-Range"); r.Header.Del("Range") }
	peer.rewrite.Store(&ignoreRange)
	due(t, destDB)
	repairOnce(t, dest, peer.URL, meta.Name)
	if count, _ := dest.ResumedDownloads(); count != 0 {
		t.Fatalf("a whole-file answer was counted as a resume")
	}
	assertPublished(t, dest, meta.Name, body)
}

// Progress is made durable while a large transfer runs, not only when it
// fails visibly: a crash or power loss costs at most one checkpoint interval.
func TestLargeDownloadCheckpointsWhileItRuns(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	body := randomBody(checkpointBytes*2+(1<<20), 6)
	meta := indexBody(t, source, "huge.apk", body, 1000)
	sent := make(chan struct{})
	hold := make(chan struct{})
	handler := server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/files/") {
			handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body[:checkpointBytes+(4<<20)])
		w.(http.Flusher).Flush()
		close(sent)
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(peer.Close)
	t.Cleanup(func() { close(hold) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		unlock := dest.paths.lock(meta.Name)
		defer unlock()
		done <- dest.downloadAndApply(ctx, peer.URL, meta)
	}()
	<-sent
	awaitRepairCondition(t, "a checkpoint while the transfer runs", func() bool {
		records, _ := dest.store.PartialDownloads()
		return len(records) == 1 && records[0].Verified >= checkpointBytes
	})
	// Shutdown records the exact position reached: everything the peer sent.
	sentBytes := int64(checkpointBytes + (4 << 20))
	awaitRepairCondition(t, "every byte sent to reach the scratch file", func() bool {
		files := scratchFiles(t, dest.syncDir)
		if len(files) != 1 {
			return false
		}
		info, err := os.Stat(files[0])
		return err == nil && info.Size() == sentBytes
	})
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled download succeeded")
	}
	records, _ := dest.store.PartialDownloads()
	if len(records) != 1 || records[0].Verified != sentBytes {
		t.Fatalf("shutdown did not record the position reached (%d): %+v", sentBytes, records)
	}
}

// --- lanes ------------------------------------------------------------------

// A few large transfers on a slow link must not hold back the small files
// queued behind them.
func TestSmallFilesAreNotQueuedBehindLargeTransfers(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	dest.opts.RepairInterval = 10 * time.Millisecond
	dest.opts.MaxConcurrentDownloads = 1
	large := indexBody(t, source, "app.apk", randomBody(store.LargeRepairBytes+1, 7), 1000)
	var small []store.FileMeta
	for i := range 5 {
		body := fmt.Sprintf("icon %d", i)
		meta := auditMeta(fmt.Sprintf("icon-%d.png", i), body, 2000)
		auditIndex(t, source, meta, body)
		small = append(small, meta)
	}
	started, release := make(chan struct{}), make(chan struct{})
	handler := server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files/app.apk" {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(peer.Close)
	if err := dest.store.EnqueueChange(peer.URL, large, "polled"); err != nil {
		t.Fatal(err)
	}
	runRepairLoop(t, dest, peer.URL)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("large transfer did not start")
	}
	for _, meta := range small {
		if err := dest.store.EnqueueChange(peer.URL, meta, "polled"); err != nil {
			t.Fatal(err)
		}
	}
	dest.notifyApply(peer.URL)
	awaitRepairCondition(t, "small files while the large one is held", func() bool {
		pending, _ := dest.store.PendingRepairCount(peer.URL)
		return pending == 1
	})
	for i := range small {
		assertNamespaceBytes(t, dest, small[i].Name, fmt.Sprintf("icon %d", i))
	}
	close(release)
	awaitRepairCondition(t, "the large transfer", func() bool {
		pending, _ := dest.store.PendingRepairCount(peer.URL)
		return pending == 0
	})
}
