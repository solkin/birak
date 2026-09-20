package syncer

// Polling and manifest reconciliation discover work; one loop per peer applies
// it. These helpers give a test both halves of that exchange, and the tests
// below pin the split itself: a poll must move no bytes, and everything it
// found must still be durable if the process stops before any of it is applied.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

func writeJSONMeta(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// drainQueued applies everything currently due for a peer.
func drainQueued(t *testing.T, s *Syncer, peerURL string) {
	t.Helper()
	for pass := 0; pass < 10; pass++ {
		items, err := s.store.DueRepairs(peerURL, 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			return
		}
		for _, item := range items {
			if err := s.repairOne(context.Background(), peerURL, item); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// pullPeer is one complete exchange: read the peer's stream, then apply what
// reading it recorded.
func pullPeer(t *testing.T, s *Syncer, peerURL string) (int, error) {
	t.Helper()
	n, err := s.syncOnce(context.Background(), peerURL)
	if err != nil {
		return n, err
	}
	drainQueued(t, s, peerURL)
	return n, nil
}

// Reading the change stream must not transfer anything. This is what removes
// head-of-line blocking: one large, slow file can no longer delay the next page.
func TestPollingRecordsWorkWithoutTransferringIt(t *testing.T) {
	s, _ := auditSyncer(t)
	meta := auditMeta("wanted.txt", "payload", time.Now().UnixNano())

	var fileRequests atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderEpoch, "peer-epoch")
		switch {
		case strings.HasPrefix(r.URL.Path, "/files/"):
			fileRequests.Add(1)
			w.Header().Set(server.HeaderMode, "644")
			w.Write([]byte("payload"))
		case strings.HasPrefix(r.URL.Path, "/meta/"):
			writeJSONMeta(w, meta)
		default:
			writeJSONMeta(w, []store.FileMeta{meta})
		}
	}))
	t.Cleanup(peer.Close)

	if _, err := s.syncOnce(context.Background(), peer.URL); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if got := fileRequests.Load(); got != 0 {
		t.Fatalf("polling issued %d file transfers", got)
	}

	// The work is durable and the cursor has moved: reading and applying are
	// separate, and a crash here loses neither.
	count, err := s.store.PendingRepairCount(peer.URL)
	if err != nil || count != 1 {
		t.Fatalf("poll did not record its work: %d %v", count, err)
	}
	if cursor, err := s.store.GetCursor(peer.URL); err != nil || cursor != meta.Version {
		t.Fatalf("cursor = %d, want %d (%v)", cursor, meta.Version, err)
	}

	drainQueued(t, s, peer.URL)
	if got := fileRequests.Load(); got != 1 {
		t.Fatalf("apply issued %d file transfers, want 1", got)
	}
	assertNamespaceBytes(t, s, meta.Name, "payload")
	if count, err := s.store.PendingRepairCount(peer.URL); err != nil || count != 0 {
		t.Fatalf("queue did not drain: %d %v", count, err)
	}
}

// A page that cannot be recorded in full must leave the cursor alone. Accepting
// a change without being able to record it is exactly how a file goes missing.
func TestUnrecordablePageDoesNotAdvanceTheCursor(t *testing.T) {
	s, _ := auditSyncer(t)
	s.store.SetRepairLimit(1)
	first := auditMeta("a.txt", "one", 100)
	second := auditMeta("b.txt", "two", 200)
	second.Version = 2

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderEpoch, "peer-epoch")
		writeJSONMeta(w, []store.FileMeta{first, second})
	}))
	t.Cleanup(peer.Close)

	_, err := s.syncOnce(context.Background(), peer.URL)
	if err == nil {
		t.Fatal("a page that exceeded the queue cap was accepted")
	}
	if cursor, cErr := s.store.GetCursor(peer.URL); cErr != nil || cursor != 0 {
		t.Fatalf("cursor advanced past unrecorded work: %d (%v)", cursor, cErr)
	}
}

// The point of the split: a slow transfer must not hold up the change stream.
// While every download slot is occupied by a file that will not finish, polling
// must keep reading pages and the cursor must keep moving.
func TestSlowTransferDoesNotStallTheChangeStream(t *testing.T) {
	s, _ := auditSyncer(t)
	s.opts.BatchLimit = 1
	s.opts.MaxConcurrentDownloads = 1
	s.opts.RepairInterval = 5 * time.Millisecond

	const pages = 6
	var changes []store.FileMeta
	for i := 0; i < pages; i++ {
		meta := auditMeta(fmt.Sprintf("file-%02d", i), "payload", int64(100+i))
		meta.Version = int64(i + 1)
		changes = append(changes, meta)
	}

	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	var transfers atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderEpoch, "peer-epoch")
		switch {
		case strings.HasPrefix(r.URL.Path, "/files/"):
			// A transfer that never completes, but keeps the connection open.
			transfers.Add(1)
			select {
			case <-hung:
			case <-r.Context().Done():
			}
		case strings.HasPrefix(r.URL.Path, "/meta/"):
			name := strings.TrimPrefix(r.URL.Path, "/meta/")
			for _, meta := range changes {
				if meta.Name == name {
					writeJSONMeta(w, meta)
					return
				}
			}
			http.Error(w, "not found", http.StatusNotFound)
		default:
			var since int64
			fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)
			out := []store.FileMeta{}
			for _, meta := range changes {
				if meta.Version > since && len(out) < s.opts.BatchLimit {
					out = append(out, meta)
				}
			}
			writeJSONMeta(w, out)
		}
	}))
	t.Cleanup(peer.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Read the first page so the apply loop has something to start on.
	if _, err := s.syncOnce(ctx, peer.URL); err != nil {
		t.Fatal(err)
	}
	go s.repairPeer(ctx, peer.URL)

	// Wait until the single download slot is occupied by the stuck transfer.
	awaitRepairCondition(t, "a transfer to occupy the only slot", func() bool {
		return transfers.Load() > 0
	})

	// Reading the rest of the stream must not wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cursor, err := s.store.GetCursor(peer.URL); err != nil {
			t.Fatal(err)
		} else if cursor == int64(pages) {
			break
		}
		if time.Now().After(deadline) {
			cursor, _ := s.store.GetCursor(peer.URL)
			t.Fatalf("a stuck transfer held up the change stream: cursor %d of %d", cursor, pages)
		}
		if _, err := s.syncOnce(ctx, peer.URL); err != nil {
			t.Fatalf("poll failed while a transfer was stuck: %v", err)
		}
	}
	count, err := s.store.PendingRepairCount(peer.URL)
	if err != nil || count != pages {
		t.Fatalf("stream was read but its work was not recorded: %d %v", count, err)
	}
}
