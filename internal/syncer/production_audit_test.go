package syncer

// Regression tests for the production safety failures documented in
// docs/audits/sync-production-readiness.md. These always run in CI.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

type auditTransport func(*http.Request) (*http.Response, error)

func (f auditTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := f(r)
	if response != nil {
		if response.Header == nil {
			response.Header = make(http.Header)
		}
		response.Header.Set(server.HeaderProtocol, server.ProtocolVersion)
	}
	return response, err
}

func auditSyncer(t *testing.T) (*Syncer, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	st, err := store.New(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := watcher.New(dir, st, logger, time.Millisecond, time.Hour, nil)
	s := New(st, w, dir, "audit-node", nil, nil, logger, Options{
		PollInterval: time.Millisecond, BatchLimit: 100, MaxConcurrentDownloads: 1,
	})
	t.Cleanup(func() { s.client.CloseIdleConnections(); s.downloadClient.CloseIdleConnections() })
	return s, dbPath
}

func auditMeta(name, body string, stamp int64) store.FileMeta {
	h := sha256.Sum256([]byte(body))
	return store.FileMeta{Name: name, Hash: hex.EncodeToString(h[:]), Size: int64(len(body)), ModTime: stamp, Version: 1}
}

func auditIndex(t *testing.T, s *Syncer, meta store.FileMeta, body string) {
	t.Helper()
	path := filepath.Join(s.syncDir, meta.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(0, meta.ModTime)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.PutFile(meta.Name, meta.ModTime, meta.Size, meta.Hash, false); err != nil {
		t.Fatal(err)
	}
}

func auditBody(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func auditPeer(t *testing.T, changes []store.FileMeta) *httptest.Server {
	t.Helper()
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		if r.URL.Path == "/changes" {
			json.NewEncoder(w).Encode(changes)
			return
		}
		http.Error(w, "injected unavailable file", http.StatusServiceUnavailable)
	}))
	t.Cleanup(p.Close)
	return p
}

func TestAuditCancelledBatchDoesNotAdvancePastUnrecordedWork(t *testing.T) {
	s, _ := auditSyncer(t)
	first := auditMeta("first", "one", 100)
	second := auditMeta("second", "two", 200)
	second.Version = 2
	p := auditPeer(t, []store.FileMeta{first, second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.downloadClient.Transport = auditTransport(func(*http.Request) (*http.Response, error) {
		cancel() // Shutdown while a file in the batch is being fetched.
		return nil, context.Canceled
	})
	_, _ = s.syncOnce(ctx, p.URL)
	cursor, err := s.store.GetCursor(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.store.PendingRepairCount(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 0 && pending != 2 {
		t.Fatalf("shutdown lost work: cursor=%d, pending=%d; neither file was applied", cursor, pending)
	}
}

func TestAuditRepairPersistenceFailureMustBlockCursor(t *testing.T) {
	s, dbPath := auditSyncer(t)
	// Inject a failure only at the durable enqueue boundary. Other writes still
	// work, modelling a transient store failure before the cursor commit.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER audit_repair_failure BEFORE INSERT ON repair_queue
		BEGIN SELECT RAISE(FAIL, 'injected repair persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	p := auditPeer(t, []store.FileMeta{auditMeta("missing", "data", 100)})
	_, syncErr := s.syncOnce(context.Background(), p.URL)
	cursor, err := s.store.GetCursor(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.store.PendingRepairCount(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 0 || syncErr == nil {
		t.Fatalf("unpersisted failure acknowledged: cursor=%d pending=%d syncErr=%v", cursor, pending, syncErr)
	}
}

func TestAuditNewerLocalWriteDuringDownloadSurvives(t *testing.T) {
	s, _ := auditSyncer(t)
	auditIndex(t, s, auditMeta("shared", "initial", 100), "initial")
	requested, release := make(chan struct{}), make(chan struct{})
	s.downloadClient.Transport = auditTransport(func(r *http.Request) (*http.Response, error) {
		close(requested)
		select {
		case <-release:
			return auditBody("remote older"), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.applyChange(ctx, "http://peer.invalid", auditMeta("shared", "remote older", 200)) }()
	select {
	case <-requested:
	case <-ctx.Done():
		t.Fatal("download never started")
	}
	// Emulate a completed gateway write, already observed by the watcher.
	auditIndex(t, s, auditMeta("shared", "local newest", 300), "local newest")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.syncDir, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "local newest" {
		t.Fatalf("newer, indexed local write was destroyed by in-flight download: got %q", got)
	}
}

func TestAuditMergeIsIndependentOfArrivalOrder(t *testing.T) {
	states := []store.FileMeta{
		auditMeta("shared", "same bytes", 100),
		auditMeta("shared", "same bytes", 300),
		auditMeta("shared", "middle write", 200),
	}
	for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		s, _ := auditSyncer(t)
		for _, i := range order {
			body := "same bytes"
			if i == 2 {
				body = "middle write"
			}
			s.downloadClient.Transport = auditTransport(func(*http.Request) (*http.Response, error) { return auditBody(body), nil })
			if err := s.applyChange(context.Background(), "http://peer.invalid", states[i]); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.store.GetFile("shared")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Hash != states[1].Hash || got.ModTime != states[1].ModTime {
			t.Errorf("arrival order %v lost winning state: got %+v, want hash=%s mtime=300", order, got, states[1].Hash)
		}
	}
}

func TestAuditUnknownTombstonePreventsStaleResurrection(t *testing.T) {
	s, _ := auditSyncer(t)
	deleted := store.FileMeta{Name: "deleted", ModTime: 101, Version: 2, Deleted: true}
	if err := s.applyChange(context.Background(), "http://peer.invalid", deleted); err != nil {
		t.Fatal(err)
	}
	s.downloadClient.Transport = auditTransport(func(*http.Request) (*http.Response, error) { return auditBody("old bytes"), nil })
	if err := s.applyChange(context.Background(), "http://stale.invalid", auditMeta("deleted", "old bytes", 100)); err != nil {
		t.Fatal(err)
	}
	got, err := s.store.GetFile("deleted")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Deleted {
		t.Fatalf("already consumed deletion did not prevent resurrection: %+v", got)
	}
}

func TestAuditSlowHeadersHaveBoundedWait(t *testing.T) {
	if testing.Short() {
		t.Skip("uses the real production response-header timeout")
	}
	t.Parallel()
	s, _ := auditSyncer(t)
	requested := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		close(requested)
		<-r.Context().Done() // TCP connects but no HTTP headers ever arrive.
	}))
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.applyChange(ctx, peer.URL, auditMeta("stalled", "data", 100)) }()
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not arrive")
	}
	// Allow one watchdog tick beyond the promised stall bound.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled request succeeded")
		}
	case <-time.After(stallTimeout + stallTimeout/4 + time.Second):
		cancel()
		<-done
		t.Fatal("download still waits for HTTP headers after 76s; a stalled path also blocks its per-path lock")
	}
}

func TestAuditBodyStallWatchdogCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := watchStall(&progressReader{r: strings.NewReader("")}, 1024, 20*time.Millisecond, cancel)
	defer watch.stop()
	select {
	case <-ctx.Done():
		if !watch.fired() {
			t.Fatal("cancelled without recording stall")
		}
	case <-time.After(time.Second):
		t.Fatal("body watchdog did not cancel")
	}
}

func TestAuditFailedDeletionMustSurviveSourceGC(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	meta := auditMeta("blocked/file", "old bytes", 100)
	auditIndex(t, dest, meta, "old bytes")
	// A temporarily unreadable parent makes deletion fail while polling works.
	parent := filepath.Join(dest.syncDir, "blocked")
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })
	if _, err := os.Stat(filepath.Join(parent, "file")); !errors.Is(err, os.ErrPermission) {
		t.Skip("requires an unprivileged user to enforce directory permissions")
	}
	if _, err := source.store.PutFile(meta.Name, 101, 0, "", true); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
	defer peer.Close()
	if _, err := dest.syncOnce(context.Background(), peer.URL); err != nil {
		t.Fatal(err)
	}
	pending, err := dest.store.PendingRepairCount(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("fault not injected: pending=%d", pending)
	}
	if _, err := dest.syncOnce(context.Background(), peer.URL); err != nil {
		t.Fatal(err)
	}
	gate, err := source.store.MinAckedVersion(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Compress retention time; the dest remains online, continuously polling.
	if _, err := source.store.PurgeTombstones(time.Nanosecond, gate); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	items, err := dest.store.DueRepairs(peer.URL, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		dest.repairOne(context.Background(), peer.URL, item)
	}
	got, err := dest.store.GetFile(meta.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Deleted {
		t.Fatalf("online peer lost deletion after acknowledged source GC: gate=%d replica=%+v", gate, got)
	}
}

func TestUnindexedGatewayCommitDuringDownloadSurvives(t *testing.T) {
	s, _ := auditSyncer(t)
	now := time.Now()
	auditIndex(t, s, auditMeta("shared", "initial", now.Add(-time.Hour).UnixNano()), "initial")
	requested, release := make(chan struct{}), make(chan struct{})
	s.downloadClient.Transport = auditTransport(func(r *http.Request) (*http.Response, error) {
		close(requested)
		select {
		case <-release:
			return auditBody("remote"), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.applyChange(ctx, "http://peer.invalid", auditMeta("shared", "remote", now.Add(-time.Minute).UnixNano()))
	}()
	select {
	case <-requested:
	case <-ctx.Done():
		t.Fatal("download did not start")
	}
	scratch := filepath.Join(s.syncDir, ".birak-tmp-gateway")
	if err := os.WriteFile(scratch, []byte("gateway newest"), 0o600); err != nil {
		t.Fatal(err)
	}
	// This is the actual publication path used by HTTP, S3, WebDAV and multipart.
	// The watcher is intentionally not running, so SQLite still holds 'initial'.
	if err := fileops.Publish(s.syncDir, scratch, filepath.Join(s.syncDir, "shared")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.syncDir, "shared"))
	if err != nil || string(got) != "gateway newest" {
		t.Fatalf("unindexed commit overwritten: %q, %v", got, err)
	}
}

func TestOpenSFTPWriterBlocksReplicaUntilClose(t *testing.T) {
	s, _ := auditSyncer(t)
	path := filepath.Join(s.syncDir, "shared")
	f, closeWriter, err := fileops.OpenWriter(s.syncDir, path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWriter()
	if _, err := f.WriteString("upload"); err != nil {
		t.Fatal(err)
	}
	s.downloadClient.Transport = auditTransport(func(*http.Request) (*http.Response, error) { return auditBody("remote"), nil })
	meta := auditMeta("shared", "remote", time.Now().Add(time.Hour).UnixNano())
	if err := s.applyChange(context.Background(), "http://peer.invalid", meta); !errors.Is(err, fileops.ErrBusy) {
		t.Fatalf("open writer was not protected: %v", err)
	}
	if err := closeWriter(); err != nil {
		t.Fatal(err)
	}
	if err := s.applyChange(context.Background(), "http://peer.invalid", meta); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "remote" {
		t.Fatalf("closed writer still blocked convergence: %q %v", got, err)
	}
}

func TestQueuedDeletionAppliesWhilePeerOffline(t *testing.T) {
	s, _ := auditSyncer(t)
	meta := auditMeta("removed", "old bytes", 100)
	auditIndex(t, s, meta, "old bytes")
	deletion := store.FileMeta{Name: meta.Name, ModTime: 200, Deleted: true, Version: 2}
	if err := s.store.EnqueueChange("http://offline", deletion, "unlink failed"); err != nil {
		t.Fatal(err)
	}
	s.client.Transport = auditTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("peer offline")
	})
	items, err := s.store.DueRepairs("http://offline", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("queued deletion: %v, %v", items, err)
	}
	s.repairOne(context.Background(), "http://offline", items[0])
	if _, err := os.Stat(filepath.Join(s.syncDir, meta.Name)); !os.IsNotExist(err) {
		t.Fatalf("queued deletion was not applied: %v", err)
	}
	got, err := s.store.GetFile(meta.Name)
	if err != nil || got == nil || !got.Deleted || got.ModTime != deletion.ModTime {
		t.Fatalf("lost queued deletion state: %+v, %v", got, err)
	}
	stats, err := s.store.RepairQueueStats()
	if err != nil || stats.Total != 0 {
		t.Fatalf("successful deletion remains queued: %+v, %v", stats, err)
	}
}

func TestDuplicateNodeIdentityRejectedByAllReplicationPaths(t *testing.T) {
	s, _ := auditSyncer(t)
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderNodeID, s.nodeID)
		io.WriteString(w, "[]")
	}))
	defer p.Close()
	for _, path := range []string{"/changes?since=0", "/manifest", "/meta/file"} {
		if resp, err := s.doRequest(context.Background(), p.URL+path); err == nil {
			resp.Body.Close()
			t.Errorf("accepted duplicate identity at %s", path)
		}
	}
	if err := s.downloadAndApply(context.Background(), p.URL, auditMeta("file", "[]", 100)); err == nil {
		t.Fatal("accepted file from duplicate node identity")
	}
}
