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

// indexOptions is what the caller already guarantees about the names it asks to
// index. Everything here is a promise a commit can make and an external writer
// cannot, which is why indexing is conservative by default.
type indexOptions struct {
	// allowIntegrity lets a quarantined name pass instead of failing the caller.
	allowIntegrity bool
	// durable means these bytes were fsynced before they were published. Every
	// commit path does that; a file that merely appeared on disk did not.
	durable bool
	// published carries bytes the caller already read, keyed by name.
	published map[string]*hashed
	// trustStat accepts an unchanged size and timestamp as proof that the file
	// did not change. Bytes that changed underneath both is the scrub's job.
	trustStat bool
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
	return w.indexPathsLocked(names, indexOptions{})
}

func (w *Watcher) beginCommitLocked(paths []string) error {
	names, err := w.relativePaths(paths)
	if err != nil {
		return err
	}
	// Observe the old destination before an overwrite, even if fsnotify has not
	// indexed it yet. Its clock is the lower bound for the acknowledged mutation.
	// A file whose size and timestamp still match the index has nothing new to
	// observe, so the old generation is not read again just to confirm that.
	if err = w.indexPathsLocked(names, indexOptions{allowIntegrity: true, trustStat: true}); err != nil {
		return err
	}
	return w.store.BeginLocal(names)
}
func (w *Watcher) finishCommitLocked(paths []string, published map[string]fileops.Published) error {
	names, err := w.relativePaths(paths)
	if err != nil {
		return err
	}
	opts := indexOptions{durable: true}
	for full, entry := range published {
		// Converted one at a time: a shortcut that cannot be attributed to a
		// name is simply dropped, and that name is read the ordinary way.
		mapped, err := w.relativePaths([]string{full})
		if err != nil || len(mapped) != 1 {
			continue
		}
		if opts.published == nil {
			opts.published = make(map[string]*hashed, len(published))
		}
		opts.published[mapped[0]] = &hashed{info: entry.Info, hash: entry.Hash}
	}
	if err = w.indexPathsLocked(names, opts); err != nil {
		return err
	}
	return w.store.EndLocal(names)
}
func (w *Watcher) indexPathsLocked(paths []string, opts indexOptions) error {
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
		err := w.refreshFileLocked(name, opts)
		if opts.allowIntegrity && errors.Is(err, ErrIntegrity) {
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
		if err = w.indexPathsLocked(paths, indexOptions{}); err != nil {
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
	return w.store.IsQuarantined(name)
}
func (w *Watcher) clearIntegrity(name string, verifiedKey ...string) error {
	key := ""
	if len(verifiedKey) > 0 {
		key = verifiedKey[0]
	}
	if !w.NeedsRepair(name) && !w.store.IsDamagedObject(key) {
		return nil
	}
	if key == "" {
		// Removing a quarantined name must also survive power loss before its
		// fence is dropped, otherwise the damaged name can reappear unfenced.
		if err := fileops.SyncSurvivingParent(filepath.Join(w.dir, filepath.FromSlash(name)), w.dir); err != nil {
			return err
		}
	}
	if err := w.store.ClearQuarantine(name, key); err != nil {
		return err
	}
	w.requestRescan()
	return nil
}

func (w *Watcher) checkReadLocked(path string, info os.FileInfo) error {
	key, err := fileops.GenerationKey(path, info)
	if err != nil {
		return err
	}
	if w.store.IsDamagedObject(key) {
		return fileops.ErrUnreadable
	}
	for _, name := range w.store.QuarantinedNames() {
		damaged := filepath.Join(w.dir, filepath.FromSlash(name))
		if path == damaged {
			return fileops.ErrUnreadable
		}
		// A symlink or hard link must not bypass a name's quarantine.
		if other, err := os.Stat(damaged); err == nil && os.SameFile(other, info) {
			return fileops.ErrUnreadable
		}
	}
	return nil
}
func (w *Watcher) quarantine(meta *store.FileMeta) error {
	err := fmt.Errorf("%w: %s", ErrIntegrity, meta.Name)
	// The name is recorded and repaired from a peer; it is reported as a count
	// rather than as this node's last error, so one damaged file does not claim
	// the whole node is faulty.
	fileops.RevokeReadersLocked(w.dir, filepath.Join(w.dir, filepath.FromSlash(meta.Name)))
	path := filepath.Join(w.dir, filepath.FromSlash(meta.Name))
	info, statErr := os.Stat(path)
	if statErr != nil {
		return statErr
	}
	key, keyErr := fileops.GenerationKey(path, info)
	if keyErr != nil {
		return keyErr
	}
	if qerr := w.store.SetQuarantined(meta.Name, key); qerr != nil {
		return fmt.Errorf("persist quarantine %q: %w", meta.Name, qerr)
	}
	// An unindexed alias has no trusted version to request yet.
	if meta.Hash == "" {
		return err
	}
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
		// A restart can observe a rename that was never flushed. Seeing matching
		// bytes is not a durability barrier: repeat both flushes before the
		// transaction that advertises the version and clears its replica intent.
		if !meta.Deleted {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			if err := errors.Join(f.Sync(), f.Close()); err != nil {
				return err
			}
		}
		if err := fileops.SyncSurvivingParent(path, w.dir); err != nil {
			return err
		}
		_, err = w.store.PutRemote(*meta)
		return err
	}
	if statErr != nil && !missing {
		return statErr
	}
	return w.store.ClearReplicaIntent(name)
}
