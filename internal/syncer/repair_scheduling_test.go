package syncer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

func awaitRepairCondition(t *testing.T, what string, ready func() bool) {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-timeout.C:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

func runRepairLoop(t *testing.T, s *Syncer, peer string) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.repairPeer(ctx, peer) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("repair loop did not stop with its workers")
		}
	})
	return cancel, done
}

func TestSlowRepairDoesNotBlockOtherNames(t *testing.T) {
	for _, arrival := range []string{"already-queued", "queued-during-download"} {
		t.Run(arrival, func(t *testing.T) {
			source, _ := auditSyncer(t)
			dest, _ := auditSyncer(t)
			dest.opts.RepairInterval = 10 * time.Millisecond
			dest.opts.MaxConcurrentDownloads = 2
			dest.opts.BatchLimit = 1
			slow := auditMeta("slow", "slow bytes", 200)
			auditIndex(t, source, slow, "slow bytes")
			var fast []store.FileMeta
			for i := 0; i < 6; i++ {
				meta := auditMeta(fmt.Sprintf("fast-%d", i), "fast bytes", 200)
				auditIndex(t, source, meta, "fast bytes")
				fast = append(fast, meta)
			}
			deletion := store.FileMeta{Name: "deleted", Deleted: true, Clock: 300, ModTime: 300, Version: 8}
			if _, err := source.store.PutRemote(deletion); err != nil {
				t.Fatal(err)
			}
			auditIndex(t, dest, auditMeta("deleted", "old bytes", 100), "old bytes")
			fast = append(fast, deletion)
			started, release := make(chan struct{}, 1), make(chan struct{})
			var slowRequests atomic.Int32
			handler := server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler()
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/files/slow" {
					handler.ServeHTTP(w, r)
					return
				}
				slowRequests.Add(1)
				w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
				_, _ = w.Write([]byte("s"))
				w.(http.Flusher).Flush()
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-release:
					_, _ = w.Write([]byte("low bytes"))
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(peer.Close)
			if err := dest.store.EnqueueChange(peer.URL, slow, "retry slow file"); err != nil {
				t.Fatal(err)
			}
			enqueueFast := func() {
				for _, meta := range fast {
					if err := dest.store.EnqueueChange(peer.URL, meta, "retry independent operation"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if arrival == "already-queued" {
				enqueueFast()
			}
			runRepairLoop(t, dest, peer.URL)
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("slow download did not start")
			}
			if arrival == "queued-during-download" {
				enqueueFast()
			}
			awaitRepairCondition(t, "independent repairs while slow file is still downloading", func() bool {
				count, err := dest.store.PendingRepairCount(peer.URL)
				return err == nil && count == 1
			})
			for i := 0; i < 6; i++ {
				assertNamespaceBytes(t, dest, fmt.Sprintf("fast-%d", i), "fast bytes")
			}
			if _, err := os.Stat(filepath.Join(dest.syncDir, "deleted")); !os.IsNotExist(err) {
				t.Fatalf("queued deletion blocked behind download: %v", err)
			}
			if slowRequests.Load() != 1 {
				t.Fatalf("one name started more than once: %d", slowRequests.Load())
			}
			if _, err := os.Stat(filepath.Join(dest.syncDir, "slow")); !os.IsNotExist(err) {
				t.Fatalf("partial slow file published: %v", err)
			}
			close(release)
			awaitRepairCondition(t, "slow repair completion", func() bool {
				count, err := dest.store.PendingRepairCount(peer.URL)
				return err == nil && count == 0
			})
			assertNamespaceBytes(t, dest, "slow", "slow bytes")
		})
	}
}

func TestPollingAndRepairShareDownloadLimit(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	dest.opts.RepairInterval = 10 * time.Millisecond
	dest.opts.MaxConcurrentDownloads = 2
	var metas []store.FileMeta
	releases := map[string]chan struct{}{}
	for _, name := range []string{"poll-a", "poll-b", "repair"} {
		meta := auditMeta(name, "bytes", 200)
		auditIndex(t, source, meta, "bytes")
		metas = append(metas, meta)
		releases[name] = make(chan struct{})
	}
	arrived := make(chan string, 10)
	var active, peak atomic.Int32
	handler := server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/files/") {
			handler.ServeHTTP(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/files/")
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		_, _ = w.Write([]byte("b"))
		w.(http.Flusher).Flush()
		arrived <- name
		select {
		case <-releases[name]:
			_, _ = w.Write([]byte("ytes"))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(peer.Close)
	ctx, cancel := context.WithCancel(context.Background())
	batchDone := make(chan error, 1)
	go func() { _, err := dest.applyBatch(ctx, peer.URL, metas[:2]); batchDone <- err }()
	t.Cleanup(func() { cancel(); <-batchDone })
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("poll downloads did not fill both slots")
		}
	}
	if err := dest.store.EnqueueChange(peer.URL, metas[2], "queued repair"); err != nil {
		t.Fatal(err)
	}
	stopRepair, repairDone := runRepairLoop(t, dest, peer.URL)
	select {
	case name := <-arrived:
		t.Fatalf("download limit exceeded while polling slots occupied: %s", name)
	case <-time.After(200 * time.Millisecond):
	}
	// A repair waiting for a polling slot must also be cancellable and stay
	// durable. Restarting only the repair loop must find that same operation.
	stopRepair()
	select {
	case <-repairDone:
	case <-time.After(3 * time.Second):
		t.Fatal("repair could not cancel while waiting for a download slot")
	}
	if count, err := dest.store.PendingRepairCount(peer.URL); err != nil || count != 1 {
		t.Fatalf("cancelled waiter lost work: %d %v", count, err)
	}
	runRepairLoop(t, dest, peer.URL)
	close(releases["poll-a"])
	select {
	case name := <-arrived:
		if name != "repair" {
			t.Fatalf("unexpected request: %s", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("repair did not use the released download slot")
	}
	close(releases["repair"])
	close(releases["poll-b"])
	awaitRepairCondition(t, "repair sharing the download budget", func() bool {
		count, err := dest.store.PendingRepairCount(peer.URL)
		return err == nil && count == 0
	})
	for _, meta := range metas {
		awaitRepairCondition(t, meta.Name+" publication", func() bool {
			got, err := dest.store.GetFile(meta.Name)
			return err == nil && got != nil && store.CompareState(got, &meta) == 0
		})
		assertNamespaceBytes(t, dest, meta.Name, "bytes")
	}
	if peak.Load() != 2 {
		t.Fatalf("peak downloads: %d", peak.Load())
	}
}

func TestCancelledParallelRepairsResumeAfterReopen(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, database := auditSyncer(t)
	dest.opts.RepairInterval = 5 * time.Millisecond
	dest.opts.MaxConcurrentDownloads = 2
	var metas []store.FileMeta
	for _, name := range []string{"a", "b"} {
		meta := auditMeta(name, "new bytes", 200)
		auditIndex(t, source, meta, "new bytes")
		auditIndex(t, dest, auditMeta(name, "old bytes", 100), "old bytes")
		metas = append(metas, meta)
	}
	var healthy atomic.Bool
	var active atomic.Int32
	started := make(chan struct{}, 2)
	handler := server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() || !strings.HasPrefix(r.URL.Path, "/files/") {
			handler.ServeHTTP(w, r)
			return
		}
		active.Add(1)
		defer active.Add(-1)
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		_, _ = w.Write([]byte("new"))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(peer.Close)
	for _, meta := range metas {
		if err := dest.store.EnqueueChange(peer.URL, meta, "interrupted repair"); err != nil {
			t.Fatal(err)
		}
	}
	stop, done := runRepairLoop(t, dest, peer.URL)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("parallel downloads did not start")
		}
	}
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled workers did not finish")
	}
	awaitRepairCondition(t, "HTTP cancellation", func() bool { return active.Load() == 0 })
	for _, meta := range metas {
		assertNamespaceBytes(t, dest, meta.Name, "old bytes")
		got, err := dest.store.GetFile(meta.Name)
		if err != nil || got == nil || got.StateClock() != 100 {
			t.Fatalf("partial download changed metadata: %+v %v", got, err)
		}
	}
	if files, err := filepath.Glob(filepath.Join(dest.syncDir, ".birak-tmp-*")); err != nil || len(files) != 0 {
		t.Fatalf("cancelled downloads left staging files: %v %v", files, err)
	}
	if err := dest.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openNamespaceNode(t, dest.syncDir, database, dest.nodeID)
	reopened.opts.RepairInterval = 5 * time.Millisecond
	reopened.opts.MaxConcurrentDownloads = 2
	if count, err := reopened.store.PendingRepairCount(peer.URL); err != nil || count != 2 {
		t.Fatalf("reopen lost queue: %d %v", count, err)
	}
	healthy.Store(true)
	runRepairLoop(t, reopened, peer.URL)
	awaitRepairCondition(t, "reopened parallel repairs", func() bool {
		count, err := reopened.store.PendingRepairCount(peer.URL)
		return err == nil && count == 0
	})
	for _, meta := range metas {
		assertNamespaceBytes(t, reopened, meta.Name, "new bytes")
	}
}

func TestRepairReplacementDuringDownloadIsNotLostOrDuplicated(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	dest.opts.RepairInterval = 5 * time.Millisecond
	dest.opts.MaxConcurrentDownloads = 2
	old := auditMeta("file", "old bytes", 100)
	auditIndex(t, source, old, "old bytes")
	started, release := make(chan struct{}, 1), make(chan struct{})
	var metadataReads atomic.Int32
	handler := server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler()
	var first atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/meta/file" {
			metadataReads.Add(1)
		}
		if r.URL.Path != "/files/file" || !first.CompareAndSwap(false, true) {
			handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		_, _ = w.Write([]byte("old"))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-release:
			_, _ = w.Write([]byte(" bytes"))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(peer.Close)
	if err := dest.store.EnqueueChange(peer.URL, old, "old generation"); err != nil {
		t.Fatal(err)
	}
	runRepairLoop(t, dest, peer.URL)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("old download did not start")
	}
	newer := auditMeta("file", "new bytes", 200)
	auditIndex(t, source, newer, "new bytes")
	if err := dest.store.EnqueueChange(peer.URL, newer, "new generation"); err != nil {
		t.Fatal(err)
	}
	other := auditMeta("other", "independent", 100)
	auditIndex(t, source, other, "independent")
	if err := dest.store.EnqueueChange(peer.URL, other, "independent"); err != nil {
		t.Fatal(err)
	}
	awaitRepairCondition(t, "other work past an active replacement", func() bool {
		meta, err := dest.store.GetFile(other.Name)
		return err == nil && meta != nil
	})
	if metadataReads.Load() != 1 {
		t.Fatalf("active name scheduled repeatedly: %d", metadataReads.Load())
	}
	close(release)
	awaitRepairCondition(t, "new generation after old worker finishes", func() bool {
		count, err := dest.store.PendingRepairCount(peer.URL)
		return err == nil && count == 0
	})
	assertNamespaceBytes(t, dest, "file", "new bytes")
	got, err := dest.store.GetFile("file")
	if err != nil || store.CompareState(got, &newer) != 0 {
		t.Fatalf("new generation lost: %+v %v", got, err)
	}
	if metadataReads.Load() != 2 {
		t.Fatalf("unexpected retry count: %d", metadataReads.Load())
	}
}

func TestRepairBookkeepingFailureDoesNotSpin(t *testing.T) {
	for _, operation := range []string{"resolve", "defer"} {
		t.Run(operation, func(t *testing.T) {
			s, database := auditSyncer(t)
			s.opts.RepairInterval = 50 * time.Millisecond
			s.opts.MaxConcurrentDownloads = 2
			meta := store.FileMeta{Name: "file", Deleted: true, Clock: 100, ModTime: 100, Version: 1}
			if operation == "defer" {
				meta = auditMeta("file", "bytes", 100)
			}
			var healthy atomic.Bool
			var requests atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
				if r.URL.Path == "/files/file" {
					_, _ = w.Write([]byte("bytes"))
					return
				}
				if operation == "defer" && !healthy.Load() {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(meta)
			}))
			t.Cleanup(peer.Close)
			if err := s.store.EnqueueChange(peer.URL, meta, "retry"); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", database)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			event := "DELETE"
			if operation == "defer" {
				event = "UPDATE"
			}
			if _, err := db.Exec("CREATE TRIGGER fail_repair BEFORE " + event + " ON repair_queue BEGIN SELECT RAISE(FAIL, 'injected bookkeeping failure'); END"); err != nil {
				t.Fatal(err)
			}
			stop, done := runRepairLoop(t, s, peer.URL)
			awaitRepairCondition(t, "first repair attempt", func() bool { return requests.Load() > 0 })
			time.Sleep(150 * time.Millisecond)
			stop()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("repair loop did not stop")
			}
			if n := requests.Load(); n > 5 {
				t.Fatalf("bookkeeping failure caused a busy loop: %d attempts", n)
			}
			if count, err := s.store.PendingRepairCount(peer.URL); err != nil || count != 1 {
				t.Fatalf("bookkeeping failure lost work: %d %v", count, err)
			}
			if _, err := db.Exec("DROP TRIGGER fail_repair"); err != nil {
				t.Fatal(err)
			}
			healthy.Store(true)
			runRepairLoop(t, s, peer.URL)
			awaitRepairCondition(t, "retry after bookkeeping recovery", func() bool {
				count, err := s.store.PendingRepairCount(peer.URL)
				return err == nil && count == 0
			})
		})
	}
}

func TestDownloadBudgetIsIndependentAcrossPeers(t *testing.T) {
	dest, _ := auditSyncer(t)
	dest.opts.MaxConcurrentDownloads = 1
	source, _ := auditSyncer(t)
	fast := auditMeta("fast", "fast bytes", 200)
	auditIndex(t, source, fast, "fast bytes")
	fastPeer := httptest.NewServer(server.New(source.store, source.syncDir, "healthy-source", nil, server.Config{}, source.logger).Handler())
	t.Cleanup(fastPeer.Close)
	started := make(chan struct{}, 1)
	slowPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		_, _ = w.Write([]byte("s"))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(slowPeer.Close)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	workers.Add(1)
	go func() {
		defer workers.Done()
		_ = dest.applyChange(ctx, slowPeer.URL, auditMeta("slow", "slow bytes", 200))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first peer did not start")
	}
	done := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); done <- dest.applyChange(ctx, fastPeer.URL, fast) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slow peer exhausted another peer's download budget")
	}
	assertNamespaceBytes(t, dest, fast.Name, "fast bytes")
}
