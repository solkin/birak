package watcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
)

var ErrIntegrity = errors.New("file checksum changed without a write timestamp")

func (w *Watcher) relativePaths(paths []string) ([]string, error) {
	realRoot, err := filepath.EvalSymlinks(w.dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(paths))
	seen := make(map[string]bool)
	for _, path := range paths {
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		// Resolve directory aliases, but preserve the final component: PUT or
		// DELETE of a file symlink changes the link, not its target. Missing
		// ancestors after a tree deletion still need their original names indexed.
		parent, err := fileops.ResolvePath(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		path = filepath.Join(parent, filepath.Base(path))
		name, err := filepath.Rel(realRoot, path)
		if err != nil {
			return nil, err
		}
		name = filepath.ToSlash(name)
		if name == "." || isOutsideSyncDir(name) {
			return nil, fmt.Errorf("invalid mutation path %q", path)
		}
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	return names, nil
}
func (w *Watcher) checkSourcesLocked(paths []string) error {
	// Walk directory symlink targets too; ordinary scans do not follow them.
	resolved := make([]string, 0, 2*len(paths))
	for _, path := range paths {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		resolved = append(resolved, real, path)
	}
	names, err := w.relativePaths(resolved)
	if err != nil {
		return err
	}
	return w.indexPathsLocked(names, false)
}

func (w *Watcher) beginCommitLocked(paths []string) error {
	names, err := w.relativePaths(paths)
	if err != nil {
		return err
	}
	// Observe the old destination before an overwrite, even if fsnotify has not
	// indexed it yet. Its clock is the lower bound for the acknowledged mutation.
	if err = w.indexPathsLocked(names, true); err != nil {
		return err
	}
	return w.store.BeginLocal(names)
}
func (w *Watcher) finishCommitLocked(paths []string) error {
	names, err := w.relativePaths(paths)
	if err != nil {
		return err
	}
	if err = w.indexPathsLocked(names, false); err != nil {
		return err
	}
	return w.store.EndLocal(names)
}
func (w *Watcher) indexPathsLocked(paths []string, allowIntegrity bool) error {
	names := make(map[string]bool)
	for _, name := range paths {
		if w.shouldIgnore(name) {
			continue
		}
		// Include previous children so overwriting/moving a tree emits deletions.
		previous, err := w.store.ListSubtree(name)
		if err != nil {
			return err
		}
		for _, meta := range previous {
			names[meta.Name] = true
		}
		err = filepath.WalkDir(filepath.Join(w.dir, filepath.FromSlash(name)), func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
				return nil
			}
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(w.dir, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if w.shouldIgnore(rel) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				names[rel] = true
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for name := range names {
		err := w.refreshFileLocked(name, nil)
		if allowIntegrity && errors.Is(err, ErrIntegrity) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (w *Watcher) prepareStorageLocked() error {
	if err := w.checkStorageLocked(); err != nil {
		return err
	}
	if err := fileops.RecoverLocked(w.dir); err != nil {
		return err
	}
	if !w.recovered {
		if err := fileops.CheckOrphanedBackups(w.dir, w.shouldIgnore); err != nil {
			return err
		}
		paths, err := w.store.LocalIntents()
		if err != nil {
			return err
		}
		if err = w.indexPathsLocked(paths, false); err != nil {
			return err
		}
		if err = w.store.EndLocal(paths); err != nil {
			return err
		}
		// Replica intents are resolved here too, not only when a name happens to
		// be indexed again. A cheap stat-only sweep may never touch an unchanged
		// name, which would leave an interrupted replica commit unresolved.
		replicas, err := w.store.ReplicaIntents()
		if err != nil {
			return err
		}
		for _, name := range replicas {
			if err := w.recoverReplicaLocked(name); err != nil {
				return err
			}
		}
		w.recovered = true
	}
	return nil
}
func (w *Watcher) SetRepairPeers(peers []string) {
	unlock := fileops.Lock(w.dir)
	defer unlock()
	w.repairPeers = append([]string(nil), peers...)
}
func (w *Watcher) NeedsRepair(name string) bool {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	return w.integrity[name]
}
func (w *Watcher) clearIntegrity(name string) {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	if w.integrity[name] {
		delete(w.integrity, name)
		w.requestRescan()
	}
}
func (w *Watcher) quarantine(meta *store.FileMeta) error {
	err := fmt.Errorf("%w: %s", ErrIntegrity, meta.Name)
	// The name is recorded and repaired from a peer; it is reported as a count
	// rather than as this node's last error, so one damaged file does not claim
	// the whole node is faulty.
	w.statusMu.Lock()
	w.integrity[meta.Name] = true
	w.statusMu.Unlock()
	for _, peer := range w.repairPeers {
		if qerr := w.store.EnqueueChange(peer, *meta, "local checksum mismatch"); qerr != nil {
			return errors.Join(err, qerr)
		}
	}
	return err
}
func (w *Watcher) recoverReplicaLocked(name string) error {
	meta, err := w.store.ReplicaIntent(name)
	if err != nil || meta == nil {
		return err
	}
	path := filepath.Join(w.dir, filepath.FromSlash(name))
	info, statErr := os.Lstat(path)
	missing := os.IsNotExist(statErr) || errors.Is(statErr, syscall.ENOTDIR)
	// Directories are implicit and are never removed by a file tombstone.
	matches := meta.Deleted && (missing || statErr == nil && info.IsDir())
	if !meta.Deleted && statErr == nil {
		info, hash, err := fileops.Snapshot(path)
		if err != nil {
			return err
		}
		matches = hash == meta.Hash && info.Size() == meta.Size && info.ModTime().UnixNano() == meta.ModTime
	}
	if matches {
		_, err = w.store.PutRemote(*meta)
		return err
	}
	if statErr != nil && !missing {
		return statErr
	}
	return w.store.ClearReplicaIntent(name)
}
