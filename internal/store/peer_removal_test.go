package store

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func TestPruneCursorsRetainsAcceptedRepairsAfterReopen(t *testing.T) {
	for _, tc := range []struct {
		name string
		keep []string
	}{
		{"one-peer-removed", []string{"http://remaining"}},
		{"last-peer-removed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keep := tc.keep
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			path := filepath.Join(t.TempDir(), "store.db")
			s, err := New(path, logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if s != nil {
					s.Close()
				}
			})
			for _, peer := range []string{"http://removed", "http://remaining"} {
				if err := s.SetPeerState(peer, PeerState{Version: 12, Epoch: "old-process"}); err != nil {
					t.Fatal(err)
				}
			}
			for _, meta := range []FileMeta{
				{Name: "deleted", Deleted: true, Clock: 10, ModTime: 5, Version: 11},
				{Name: "file", Hash: strings.Repeat("a", 64), Size: 7, Clock: 11, ModTime: 6, Version: 12},
			} {
				if err := s.EnqueueChange("http://removed", meta, "accepted before source removal"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.DueRepairs("http://removed", 10)
			if err != nil || len(before) != 2 {
				t.Fatalf("initial work: %+v %v", before, err)
			}
			if err := s.PruneCursors(keep); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = New(path, logger)
			if err != nil {
				t.Fatal(err)
			}
			after, err := s.DueRepairs("http://removed", 10)
			if err != nil || len(after) != 2 {
				t.Fatalf("configuration cleanup discarded accepted work: %+v %v", after, err)
			}
			for i := range before {
				if after[i].Meta == nil || before[i].Meta == nil || *after[i].Meta != *before[i].Meta || after[i].Revision != before[i].Revision {
					t.Fatalf("queued operation changed: before=%+v after=%+v", before[i], after[i])
				}
			}
			stats, err := s.RepairQueueStats()
			if err != nil || stats.Total != 2 {
				t.Fatalf("unfinished work became invisible: %+v %v", stats, err)
			}
			if state, err := s.GetPeerState("http://removed"); err != nil || state.Version != 0 || state.Epoch != "" {
				t.Fatalf("stale cursor retained: %+v %v", state, err)
			}
			if len(keep) != 0 {
				if state, err := s.GetPeerState(keep[0]); err != nil || state.Version != 12 || state.Epoch != "old-process" {
					t.Fatalf("active cursor pruned: %+v %v", state, err)
				}
			}
		})
	}
}
