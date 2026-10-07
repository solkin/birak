package quorum

import (
	"io"
	"path/filepath"
	"sync"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

// HashiCorp Raft 1.7.3 flushes snapshot files and the directory containing the
// final snapshot, but not the new directory containing state.bin/meta.json.
// Flush those directory entries BEFORE Close renames it and reaps older
// snapshots. Doing this after Close could lose the previous recovery point.
// This wrapper intentionally depends on the pinned version's .tmp layout.
type durableSnapshots struct {
	*raft.FileSnapshotStore
	dir     string
	syncDir func(string) error
}

func newSnapshots(dir string) (*durableSnapshots, error) {
	path := filepath.Join(dir, "snapshots")
	if err := generation.MakeDir(path); err != nil {
		return nil, err
	}
	s, err := raft.NewFileSnapshotStore(dir, 2, io.Discard)
	if err != nil {
		return nil, err
	}
	return &durableSnapshots{FileSnapshotStore: s, dir: path, syncDir: generation.SyncDir}, nil
}

func (s *durableSnapshots) Create(version raft.SnapshotVersion, index, term uint64, configuration raft.Configuration, configIndex uint64, transport raft.Transport) (raft.SnapshotSink, error) {
	sink, err := s.FileSnapshotStore.Create(version, index, term, configuration, configIndex, transport)
	if err != nil {
		return nil, err
	}
	return &durableSink{SnapshotSink: sink, path: filepath.Join(s.dir, sink.ID()+".tmp"), syncDir: s.syncDir}, nil
}

type durableSink struct {
	raft.SnapshotSink
	path    string
	syncDir func(string) error
	once    sync.Once
	err     error
}

func (s *durableSink) Close() error {
	s.once.Do(func() {
		if s.err = s.syncDir(s.path); s.err != nil {
			s.SnapshotSink.Cancel()
			return
		}
		s.err = s.SnapshotSink.Close()
	})
	return s.err
}
