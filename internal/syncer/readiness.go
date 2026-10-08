package syncer

import (
	"io/fs"
	"path/filepath"

	"github.com/birak/birak/internal/watcher"
)

func (s *Syncer) initializeAdmission() {
	flag, err := s.store.NodeValue("replica_initialized")
	if err != nil {
		return
	}
	if flag != "" {
		s.initialized.Store(flag == "1")
		return
	}
	version, err := s.store.MaxVersion()
	if err != nil {
		return
	}
	seeded := version > 0 || len(s.peers) == 0
	if !seeded {
		// A node explicitly seeded with local files is an independent origin.
		// The durable 0 marker distinguishes an interrupted fresh-node catch-up
		// from such a seed on its next restart, even after some files arrive.
		err := filepath.WalkDir(s.syncDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(s.syncDir, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			if watcher.ShouldIgnore(filepath.ToSlash(rel), s.ignorePatterns) || rel == ".birak" {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type().IsRegular() {
				seeded = true
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return
		}
	}
	value := "0"
	if seeded {
		value = "1"
	}
	if err := s.store.SetNodeValue("replica_initialized", value); err != nil {
		return
	}
	s.initialized.Store(seeded)
}

// ServingReady gates only initial admission to a load balancer. Once a replica
// has compared and applied a source's observed state, peer outages never revoke
// its autonomy. This is not a global freshness or majority certificate.
func (s *Syncer) ServingReady() bool {
	if s.initialized.Load() || len(s.peers) == 0 {
		return true
	}
	s.statsMu.Lock()
	var sources []string
	for peer, st := range s.stats {
		if !st.lastReconcile.IsZero() && !st.lastSuccess.IsZero() && st.epoch != "" && st.cursor >= st.peerMaxVersion && st.consecutiveErrs == 0 {
			sources = append(sources, peer)
		}
	}
	s.statsMu.Unlock()
	for _, peer := range sources {
		pending, err := s.store.PendingRepairCount(peer)
		if err != nil || pending != 0 {
			continue
		}
		if err := s.store.SetNodeValue("replica_initialized", "1"); err != nil {
			return false
		}
		s.initialized.Store(true)
		return true
	}
	return false
}
