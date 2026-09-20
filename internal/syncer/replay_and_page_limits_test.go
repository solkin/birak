package syncer

// A peer restart resets our cursor and replays its whole manifest, so almost
// every entry this node receives is one it already holds. These tests pin the
// cost and the failure modes of that path: what it must not re-read, what it
// must still verify, and how much of this node's memory a peer may spend.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

// An entry equal to the indexed local state is answered from the index. Reading
// the file again costs a full pass over the dataset under the shared commit
// lock, and turns an unrelated local read error into a permanent repair row for
// a name that is already converged.
func TestConvergedEntryIsAnsweredFromTheIndex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	s, _ := auditSyncer(t)
	meta := auditMeta("converged.txt", "same bytes", time.Now().UnixNano())
	auditIndex(t, s, meta, "same bytes")

	path := filepath.Join(s.syncDir, meta.Name)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	if err := s.applyChange(context.Background(), "http://peer", meta); err != nil {
		t.Fatalf("converged entry was rejected: %v", err)
	}
	n, err := s.store.PendingRepairCount("http://peer")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a converged name queued %d repair(s)", n)
	}
}

// The index decides convergence; the checksum scan owns corruption. A name the
// scan has quarantined must still take the full path, even when the peer's
// state compares equal to the stored metadata.
func TestQuarantinedNameStillRepairsFromAnEqualPeerState(t *testing.T) {
	s, _ := auditSyncer(t)
	body := "original bytes"
	meta := auditMeta("damaged.txt", body, time.Now().UnixNano())
	auditIndex(t, s, meta, body)

	// Same size and timestamp, different bytes: the integrity case.
	path := filepath.Join(s.syncDir, meta.Name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", len(body))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := s.watcher.Refresh(meta.Name); err == nil {
		t.Fatal("silent content change was not detected")
	}
	if !s.watcher.NeedsRepair(meta.Name) {
		t.Fatal("damaged name was not quarantined")
	}

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderMode, "644")
		w.Write([]byte(body))
	}))
	t.Cleanup(peer.Close)

	if err := s.applyChange(context.Background(), peer.URL, meta); err != nil {
		t.Fatalf("quarantined name was not repaired: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil || string(restored) != body {
		t.Fatalf("damaged bytes survived: %q, %v", restored, err)
	}
	if s.watcher.NeedsRepair(meta.Name) {
		t.Fatal("quarantine was not cleared after the repair")
	}
}

// A saturated clock can never be advanced past, so accepting one would leave
// every later local write at that name failing with "conflict clock exhausted".
func TestSaturatedConflictClockIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta store.FileMeta
	}{
		{"clock", store.FileMeta{Clock: math.MaxInt64}},
		{"mod_time", store.FileMeta{ModTime: math.MaxInt64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := auditMeta("poisoned.txt", "remote", 1)
			meta.Clock = tc.meta.Clock
			if tc.meta.ModTime != 0 {
				meta.ModTime = tc.meta.ModTime
			}
			if err := validateMetadata(meta); err == nil {
				t.Fatal("saturated conflict clock accepted from a peer")
			}
		})
	}

	// A local write at that name must remain possible afterwards.
	s, _ := auditSyncer(t)
	if _, err := s.store.PutLocal(store.FileMeta{
		Name: "poisoned.txt", ModTime: time.Now().UnixNano(), Size: 6,
		Hash: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatalf("local write refused: %v", err)
	}
}

// Decoding a peer reply is the one place a peer decides how much memory this
// node allocates. An unbounded page took the daemon down with it.
func TestOversizedPeerPageIsRefusedWhileItStreams(t *testing.T) {
	s, _ := auditSyncer(t)
	const entries = 100_000

	var page strings.Builder
	page.WriteByte('[')
	for i := 0; i < entries; i++ {
		if i > 0 {
			page.WriteByte(',')
		}
		fmt.Fprintf(&page, `{"name":"pad-%0300d","mod_time":1,"size":1,"hash":"%064d","version":%d}`, i, 0, i+1)
	}
	page.WriteByte(']')
	body := page.String()

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderEpoch, "peer-epoch")
		w.Write([]byte(body))
	}))
	t.Cleanup(peer.Close)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err := s.syncBatch(context.Background(), peer.URL)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatalf("a %d-entry page was accepted for a limit of %d", entries, s.opts.BatchLimit)
	}
	if grew := (after.TotalAlloc - before.TotalAlloc) / (1 << 20); grew > 64 {
		t.Fatalf("an oversized peer page allocated %d MiB", grew)
	}
	if cursor, cErr := s.store.GetCursor(peer.URL); cErr != nil || cursor != 0 {
		t.Fatalf("a refused page advanced the cursor to %d (%v)", cursor, cErr)
	}
}

// The bound must not refuse a legitimate full page of long names.
func TestFullPageOfLongNamesIsStillAccepted(t *testing.T) {
	s, _ := auditSyncer(t)
	changes := make([]store.FileMeta, 0, s.opts.BatchLimit)
	for i := 0; i < s.opts.BatchLimit; i++ {
		meta := auditMeta(strings.Repeat("d", 200)+"/"+strings.Repeat("n", 200)+fmt.Sprint(i), "x", 1)
		meta.Version = int64(i + 1)
		changes = append(changes, meta)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(server.HeaderProtocol, server.ProtocolVersion)
		w.Header().Set(server.HeaderEpoch, "peer-epoch")
		json.NewEncoder(w).Encode(changes)
	}))
	t.Cleanup(peer.Close)

	n, _, err := s.syncBatch(context.Background(), peer.URL)
	if err != nil {
		t.Fatalf("a legitimate full page was refused: %v", err)
	}
	if n != len(changes) {
		t.Fatalf("decoded %d of %d entries", n, len(changes))
	}
}
