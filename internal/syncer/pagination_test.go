package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

func TestInvalidChangePageDoesNotApplyOrAdvance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cursor   int64
		versions []int64
	}{
		{"zero", 0, []int64{0}},
		{"negative", 0, []int64{-1}},
		{"repeated", 7, []int64{7}},
		{"overlap", 7, []int64{7, 8}},
		{"descending", 7, []int64{9, 8}},
		{"duplicate", 7, []int64{8, 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := auditSyncer(t)
			var healthy atomic.Bool
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
				w.Header().Set(server.HeaderEpoch, "source-epoch")
				w.Header().Set(server.HeaderMaxVersion, "10")
				versions := tc.versions
				if healthy.Load() {
					versions = []int64{8, 10} // Gaps are normal in the current-state stream.
				}
				var page []store.FileMeta
				for i, version := range versions {
					page = append(page, store.FileMeta{Name: fmt.Sprintf("file-%d", i), Deleted: true, Clock: 100, ModTime: 100, Version: version})
				}
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer peer.Close()
			before := store.PeerState{Version: tc.cursor, Epoch: "source-epoch"}
			if err := s.store.SetPeerState(peer.URL, before); err != nil {
				t.Fatal(err)
			}
			if n, err := s.syncOnce(context.Background(), peer.URL); err == nil || n != 0 {
				t.Fatalf("invalid change page accepted: n=%d err=%v", n, err)
			}
			if state, err := s.store.GetPeerState(peer.URL); err != nil || state != before {
				t.Fatalf("invalid page changed cursor: %+v %v", state, err)
			}
			if rows, err := s.store.ListManifest("", 10); err != nil || len(rows) != 0 {
				t.Fatalf("part of invalid page applied: %+v %v", rows, err)
			}
			if count, err := s.store.PendingRepairCount(peer.URL); err != nil || count != 0 {
				t.Fatalf("invalid page queued: %d %v", count, err)
			}
			healthy.Store(true)
			if n, err := pullPeer(t, s, peer.URL); err != nil || n != 2 {
				t.Fatalf("healthy retry: n=%d err=%v", n, err)
			}
			if state, err := s.store.GetPeerState(peer.URL); err != nil || state.Version != 10 {
				t.Fatalf("healthy cursor: %+v %v", state, err)
			}
			if rows, err := s.store.ListManifest("", 10); err != nil || len(rows) != 2 || !rows[0].Deleted || !rows[1].Deleted {
				t.Fatalf("healthy retry did not apply: %+v %v", rows, err)
			}
		})
	}
}

func TestInvalidManifestPageStopsBeforeRequeue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pages   [][]string
		pending int64
	}{
		{"repeated", [][]string{{"a"}, {"a"}}, 1},
		{"backwards", [][]string{{"b"}, {"a"}}, 1},
		{"descending", [][]string{{"b", "a"}}, 0},
		{"duplicate", [][]string{{"a", "a"}}, 0},
		{"empty-name", [][]string{{""}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := auditSyncer(t)
			var healthy atomic.Bool
			var requests atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
				var names []string
				if healthy.Load() {
					for _, name := range []string{"a", "b"} {
						if name > r.URL.Query().Get("after") {
							names = append(names, name)
						}
					}
				} else {
					i := int(requests.Add(1)) - 1
					if i >= len(tc.pages) {
						// Bound a broken client without depending on wall-clock timing.
						http.Error(w, "client continued after a broken page", http.StatusServiceUnavailable)
						return
					}
					names = tc.pages[i]
				}
				var page []store.FileMeta
				for _, name := range names {
					page = append(page, store.FileMeta{Name: name, Deleted: true, ModTime: 100, Clock: 100, Version: 1})
				}
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer peer.Close()
			if _, err := s.reconcileOnce(context.Background(), peer.URL); err == nil {
				t.Fatal("invalid manifest page accepted")
			}
			if got := requests.Load(); got != int32(len(tc.pages)) {
				t.Fatalf("continued requesting a non-progressing manifest: got=%d want=%d", got, len(tc.pages))
			}
			if count, err := s.store.PendingRepairCount(peer.URL); err != nil || count != tc.pending {
				t.Fatalf("invalid page changed accepted work: count=%d err=%v", count, err)
			}
			healthy.Store(true)
			if _, err := s.reconcileOnce(context.Background(), peer.URL); err != nil {
				t.Fatalf("healthy reconciliation failed: %v", err)
			}
			if count, err := s.store.PendingRepairCount(peer.URL); err != nil || count != 2 {
				t.Fatalf("healthy reconciliation did not recover: count=%d err=%v", count, err)
			}
		})
	}
}

func TestRepeatedChangePageUsesPollBackoff(t *testing.T) {
	s, _ := auditSyncer(t)
	s.opts.PollInterval = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var requests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) >= 5 {
			cancel() // Bound the old tight polling loop.
		}
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderEpoch, "source-epoch")
		w.Header().Set(server.HeaderMaxVersion, "1")
		_ = json.NewEncoder(w).Encode([]store.FileMeta{{Name: "file", Deleted: true, Version: 1, ModTime: 100, Clock: 100}})
	}))
	defer peer.Close()
	if err := s.store.SetPeerState(peer.URL, store.PeerState{Version: 1, Epoch: "source-epoch"}); err != nil {
		t.Fatal(err)
	}
	s.pollPeer(ctx, peer.URL)
	if n := requests.Load(); n < 1 || n > 4 {
		t.Fatalf("stale responses caused a tight loop: %d requests", n)
	}
	s.statsMu.Lock()
	st := *s.stats[peer.URL]
	s.statsMu.Unlock()
	if st.lastError == "" || st.consecutiveErrs == 0 || !st.lastSuccess.IsZero() {
		t.Fatalf("stale peer reported success: %+v", st)
	}
}

func TestValidPagesAllowConcurrentWritesAboveAdvertisedMaximum(t *testing.T) {
	s, _ := auditSyncer(t)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderMaxVersion, "1") // Server stamps headers before querying rows.
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		var page []store.FileMeta
		if since == 0 {
			page = []store.FileMeta{
				{Name: "file", Deleted: true, Clock: 100, ModTime: 100, Version: 1},
				{Name: "file", Deleted: true, Clock: 200, ModTime: 200, Version: 3},
			}
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer peer.Close()
	if n, err := pullPeer(t, s, peer.URL); err != nil || n != 2 {
		t.Fatalf("valid increasing versions rejected: n=%d err=%v", n, err)
	}
	if meta, err := s.store.GetFile("file"); err != nil || meta == nil || meta.Clock != 200 {
		t.Fatalf("latest version not applied: %+v %v", meta, err)
	}
}

func TestReconciliationTraversesRealManifestPages(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	const total = manifestPageSize + 3
	for i := 0; i < total; i++ {
		if _, err := source.store.PutRemote(store.FileMeta{Name: fmt.Sprintf("entry-%04d", i), Deleted: true, Clock: 100, ModTime: 100}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var cursors []string
	handler := server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cursors = append(cursors, r.URL.Query().Get("after"))
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	defer peer.Close()
	if _, err := dest.reconcileOnce(context.Background(), peer.URL); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), cursors...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "" || got[1] != "entry-0999" || got[2] != "entry-1002" {
		t.Fatalf("incorrect manifest traversal: %v", got)
	}
	items, err := dest.store.DueRepairs(peer.URL, total+1)
	if err != nil || len(items) != total {
		t.Fatalf("manifest entries lost: count=%d err=%v", len(items), err)
	}
	names := make(map[string]bool, total)
	for _, item := range items {
		if item.Meta == nil || !item.Meta.Deleted || names[item.Name] {
			t.Fatalf("invalid or duplicate queued tombstone: %+v", item)
		}
		names[item.Name] = true
	}
}

// A manifest comparison is a backstop, not a deadline. On a large tree it is
// paced: each pass consumes a budget of pages and the next one carries on where
// it stopped, so the comparison happens continuously instead of as an hourly
// burst — and an interrupted pass does not start over from the beginning.
func TestReconciliationResumesWhereItsBudgetRanOut(t *testing.T) {
	dest, _ := auditSyncer(t)
	const total = 5
	var manifest []store.FileMeta
	for i := 0; i < total; i++ {
		manifest = append(manifest, auditMeta(fmt.Sprintf("file-%02d", i), "body", int64(100+i)))
	}

	// One entry per page, however many were asked for: the page budget is about
	// requests, not entries, and a short page is a legitimate reply.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		after := r.URL.Query().Get("after")
		out := []store.FileMeta{}
		for _, entry := range manifest {
			if entry.Name > after {
				out = append(out, entry)
				break
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer peer.Close()

	dest.opts.ReconcilePageBudget = 1
	var queued int64
	for pass := 0; pass < total; pass++ {
		if _, err := dest.reconcileOnce(context.Background(), peer.URL); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		got, err := dest.store.PendingRepairCount(peer.URL)
		if err != nil {
			t.Fatal(err)
		}
		if got != queued+1 {
			t.Fatalf("pass %d queued %d entries in total, want %d", pass, got, queued+1)
		}
		queued = got
	}

	// The cycle completes and the next one starts from the beginning.
	if _, err := dest.reconcileOnce(context.Background(), peer.URL); err != nil {
		t.Fatal(err)
	}
	position, err := dest.store.NodeValue(reconcilePositionKey(peer.URL))
	if err != nil {
		t.Fatal(err)
	}
	if position != "" {
		t.Fatalf("finished cycle left position %q", position)
	}
}

// With no budget the whole manifest is compared in one pass, which is what a
// small deployment wants and what every other test here assumes.
func TestReconciliationWithoutABudgetComparesEverything(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	for i := 0; i < 5; i++ {
		auditIndex(t, source, auditMeta(fmt.Sprintf("file-%02d", i), "body", int64(100+i)), "body")
	}
	peer := httptest.NewServer(server.New(source.store, source.syncDir, "source", nil, server.Config{}, source.logger).Handler())
	defer peer.Close()

	if _, err := dest.reconcileOnce(context.Background(), peer.URL); err != nil {
		t.Fatal(err)
	}
	if got, err := dest.store.PendingRepairCount(peer.URL); err != nil || got != 5 {
		t.Fatalf("queued %d of 5 entries in one pass (%v)", got, err)
	}
}

// "Last reconciled" has to mean a finished comparison. While a paced pass
// reported itself as one, an operator watching that number would believe the
// cluster had been compared when only a slice of it had.
func TestOnlyAFinishedCycleCountsAsReconciled(t *testing.T) {
	dest, _ := auditSyncer(t)
	var manifest []store.FileMeta
	for i := range 3 {
		manifest = append(manifest, auditMeta(fmt.Sprintf("file-%02d", i), "body", int64(100+i)))
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		after := r.URL.Query().Get("after")
		out := []store.FileMeta{}
		for _, entry := range manifest {
			if entry.Name > after {
				out = append(out, entry)
				break
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer peer.Close()

	dest.opts.ReconcilePageBudget = 1
	for pass := range len(manifest) {
		completed, err := dest.reconcileOnce(context.Background(), peer.URL)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if completed {
			t.Fatalf("pass %d reported a finished cycle after one page of %d", pass, len(manifest))
		}
	}
	completed, err := dest.reconcileOnce(context.Background(), peer.URL)
	if err != nil || !completed {
		t.Fatalf("the pass that reached the end did not report a finished cycle: %v %v", completed, err)
	}
}
