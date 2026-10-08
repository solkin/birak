package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
)

// Staging uses the nearest existing directory, on the destination filesystem.
// It does not create a directory over a conflicting file before verification.
func (s *Syncer) replicaTemp(name string) (*os.File, error) {
	unlock := fileops.Lock(s.syncDir)
	defer unlock()
	// Response headers can arrive long after the original path check. Resolve
	// again under the namespace lock before creating anything on disk.
	dest, err := s.safeLocalPath(name)
	if err != nil {
		return nil, err
	}
	if err := s.watcher.CheckStorageLocked(); err != nil {
		return nil, err
	}
	for dir := filepath.Dir(dest); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil && info.IsDir() {
			return fileops.CreateTemp(dir, ".birak-tmp-replica-*")
		}
		if err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) {
			return nil, err
		}
		if dir == s.syncDir || dir == filepath.Dir(dir) {
			return nil, fmt.Errorf("no staging directory for %s", dest)
		}
	}
}

// namespaceConflictsLocked verifies actual blocking files before making a
// decision. Unsupported, ignored, unreadable or actively written entries stop
// resolution without moving any bytes.
func (s *Syncer) namespaceConflictsLocked(name string) ([]store.FileMeta, error) {
	var result []store.FileMeta
	add := func(name string) error {
		full, err := s.safeLocalPath(name)
		if err != nil {
			return err
		}
		if fileops.BusyTreeLocked(s.syncDir, full) {
			return fileops.ErrBusy
		}
		if err := s.watcher.RefreshLocked(name); err != nil {
			return err
		}
		meta, err := s.store.GetFile(name)
		if err != nil {
			return err
		}
		if meta == nil || meta.Deleted {
			return fmt.Errorf("unsupported namespace entry %q", name)
		}
		result = append(result, *meta)
		return nil
	}
	for ancestor := path.Dir(name); ancestor != "."; ancestor = path.Dir(ancestor) {
		full := filepath.Join(s.syncDir, filepath.FromSlash(ancestor))
		info, err := os.Lstat(full)
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if err := add(ancestor); err != nil {
				return nil, err
			}
			return result, nil
		}
	}
	full := filepath.Join(s.syncDir, filepath.FromSlash(name))
	info, err := os.Lstat(full)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}
	if fileops.BusyTreeLocked(s.syncDir, full) {
		return nil, fileops.ErrBusy
	}
	err = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.syncDir, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if _, err := s.safeLocalPath(name); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		return add(name)
	})
	return result, err
}

// settleNamespaceLocked returns true when the incoming file should be published
// at its requested name. Preservation always precedes deletion. Every step uses
// the existing replica intents, so retries and restart need no second journal.
func (s *Syncer) settleNamespaceLocked(ctx context.Context, meta store.FileMeta, temporary string) (bool, error) {
	conflicts, err := s.namespaceConflictsLocked(meta.Name)
	if err != nil {
		return false, err
	}
	winner := meta
	for _, local := range conflicts {
		if store.CompareState(&local, &winner) > 0 {
			winner = local
		}
	}
	if winner.Name != meta.Name {
		if err := s.preserveConflictLocked(ctx, meta, temporary); err != nil {
			return false, err
		}
		dest, err := s.safeLocalPath(meta.Name)
		if err != nil {
			return false, err
		}
		return false, s.commitDeletionLocked(ctx, store.Superseded(meta.Name, winner), dest)
	}
	for _, local := range conflicts {
		full, err := s.safeLocalPath(local.Name)
		if err != nil {
			return false, err
		}
		if err := s.commitDeletionLocked(ctx, store.Superseded(local.Name, meta), full); err != nil {
			return false, err
		}
	}
	dest, err := s.safeLocalPath(meta.Name)
	if err != nil {
		return false, err
	}
	if info, err := os.Lstat(dest); err == nil && info.IsDir() {
		// Never RemoveAll: an unobserved entry must stop the replacement.
		if err := removeEmptyTree(dest); err != nil {
			return false, err
		}
	}
	return true, nil
}

func removeEmptyTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return fmt.Errorf("namespace changed at %s", p)
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Syncer) preserveConflictLocked(ctx context.Context, original store.FileMeta, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	copyMeta := store.ConflictCopy(original)
	dest, err := s.safeLocalPath(copyMeta.Name)
	if err != nil {
		return err
	}
	if err := s.watcher.RefreshLocked(copyMeta.Name); err != nil {
		return err
	}
	previous, err := s.store.GetFile(copyMeta.Name)
	if err != nil {
		return err
	}
	if previous != nil && !previous.Deleted {
		if previous.Hash == copyMeta.Hash && previous.Size == copyMeta.Size {
			return nil
		}
		return fmt.Errorf("conflict copy destination is occupied: %s", copyMeta.Name)
	}
	// An operator may have removed an earlier conflict copy; a new conflict must
	// preserve its bytes again before removing the currently live generation.
	store.AdvanceClock(&copyMeta, previous)

	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular conflict source: %s", source)
	}
	out, err := fileops.CreateTemp(s.syncDir, ".birak-tmp-conflict-*")
	if err != nil {
		return err
	}
	defer fileops.ReleaseTemp(out)
	defer os.Remove(out.Name())
	defer out.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(in, original.Size+1))
	if err != nil {
		return err
	}
	if n != original.Size || hex.EncodeToString(hash.Sum(nil)) != original.Hash {
		return fmt.Errorf("conflict source changed: %s", original.Name)
	}
	current, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) || info.Size() != current.Size() || !info.ModTime().Equal(current.ModTime()) {
		return fileops.ErrBusy
	}
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	stamp := time.Unix(0, original.ModTime)
	if err := os.Chtimes(out.Name(), stamp, stamp); err != nil {
		return err
	}
	if err := errors.Join(out.Sync(), out.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.store.StageReplica(copyMeta); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), dest); err != nil {
		return err
	}
	if err := fileops.SyncParents(filepath.Dir(dest), s.syncDir); err != nil {
		return err
	}
	if err := s.namespaceStep("copy-published", original.Name); err != nil {
		return err
	}
	if _, err := s.store.PutRemote(copyMeta); err != nil {
		return err
	}
	s.logger.Warn("preserved namespace conflict", "name", original.Name, "copy", copyMeta.Name)
	return s.namespaceStep("preserved", original.Name)
}

func (s *Syncer) namespaceStep(step, name string) error {
	if s.namespaceCheckpoint != nil {
		return s.namespaceCheckpoint(step, name)
	}
	return nil
}
