package syncer

// A file server runs for months. Anything that grows per operation — a
// goroutine per transfer, a map entry per name, a timer never stopped — is a
// slow outage rather than a bug you notice. These tests do the same work twice
// and insist the second round costs no more than the first.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
)

// settled returns the goroutine count and live heap after giving the runtime a
// chance to finish what the previous round started.
func settled() (goroutines int, heap uint64) {
	for range 3 {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return runtime.NumGoroutine(), stats.HeapAlloc
}

func TestRepeatedReplicationDoesNotAccumulate(t *testing.T) {
	s, _ := auditSyncer(t)
	s.opts.MaxConcurrentDownloads = 4

	const perRound = 150
	bodies := map[string]string{}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderEpoch, "peer-epoch")
		switch {
		case strings.HasPrefix(r.URL.Path, "/files/"):
			w.Header().Set(server.HeaderMode, "644")
			w.Write([]byte(bodies[strings.TrimPrefix(r.URL.Path, "/files/")]))
		case strings.HasPrefix(r.URL.Path, "/meta/"):
			name := strings.TrimPrefix(r.URL.Path, "/meta/")
			writeJSONMeta(w, auditMeta(name, bodies[name], 100))
		default:
			http.Error(w, "not used", http.StatusNotFound)
		}
	}))
	t.Cleanup(peer.Close)

	round := func(generation int) {
		for i := range perRound {
			name := fmt.Sprintf("file-%03d.bin", i)
			body := fmt.Sprintf("generation %d for %s", generation, name)
			bodies[name] = body
			meta := auditMeta(name, body, int64(1000+generation))
			if err := s.store.EnqueueChange(peer.URL, meta, "leak round"); err != nil {
				t.Fatal(err)
			}
		}
		drainQueued(t, s, peer.URL)
	}

	// A first round pays for everything that is allocated once: connections,
	// prepared statements, page caches. Measure from the second.
	round(1)
	round(2)
	baseGoroutines, baseHeap := settled()

	for generation := 3; generation <= 6; generation++ {
		round(generation)
	}
	afterGoroutines, afterHeap := settled()

	if grew := afterGoroutines - baseGoroutines; grew > 5 {
		t.Errorf("goroutines grew by %d over four rounds (%d -> %d)",
			grew, baseGoroutines, afterGoroutines)
	}
	// Four rounds of 150 files apiece: a per-file leak shows up as megabytes.
	if afterHeap > baseHeap+8<<20 {
		t.Errorf("live heap grew by %d KiB over four rounds (%d -> %d KiB)",
			(afterHeap-baseHeap)/1024, baseHeap/1024, afterHeap/1024)
	}
	t.Logf("goroutines %d -> %d, live heap %d -> %d KiB over four rounds of %d files",
		baseGoroutines, afterGoroutines, baseHeap/1024, afterHeap/1024, perRound)
}

// The per-name lock is created on demand. If it were not released, every name
// this node ever touched would stay in memory for the life of the process.
func TestPerNameLocksAreReleased(t *testing.T) {
	keyed := newKeyedMutex()
	for i := range 10000 {
		unlock := keyed.lock(fmt.Sprintf("name-%d", i))
		unlock()
	}
	keyed.mu.Lock()
	remaining := len(keyed.locks)
	keyed.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d per-name locks survived their holders", remaining)
	}

	// Held locks are of course retained, and released when the holder lets go.
	first := keyed.lock("shared")
	second := make(chan func(), 1)
	go func() { second <- keyed.lock("shared") }()
	keyed.mu.Lock()
	held := len(keyed.locks)
	keyed.mu.Unlock()
	if held != 1 {
		t.Fatalf("a held name is tracked %d times", held)
	}
	first()
	(<-second)()
	keyed.mu.Lock()
	remaining = len(keyed.locks)
	keyed.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d locks survived after every holder released", remaining)
	}
}

// Peer state is per configured peer and must not grow with traffic.
func TestPeerStateDoesNotGrowWithTraffic(t *testing.T) {
	s, _ := auditSyncer(t)
	for i := range 500 {
		s.withStat("http://peer", func(st *peerStat) { st.cursor = int64(i) })
	}
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if len(s.stats) != 1 {
		t.Fatalf("tracking %d peers after traffic from one", len(s.stats))
	}
}

// Download slots are per peer, not per transfer.
func TestDownloadSlotsAreBoundedByPeerCount(t *testing.T) {
	s, _ := auditSyncer(t)
	for i := range 200 {
		release, err := s.acquireDownload(context.Background(), "http://peer")
		if err != nil {
			t.Fatal(err)
		}
		release()
		_ = i
	}
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	if len(s.downloads) != 1 {
		t.Fatalf("%d download budgets for one peer", len(s.downloads))
	}
}
