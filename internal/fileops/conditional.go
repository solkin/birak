package fileops

import (
	"errors"
	"os"
	"path/filepath"
)

// PublishIf is Publish for a write that depends on what the destination is
// now, such as an S3 PUT with If-None-Match or If-Match. precondition is handed
// the destination's current stat (or the error stating it) under the commit
// lock, in the same lock hold as the rename, so no other commit can create or
// replace the destination between the check and the publication; a non-nil
// result refuses the write and is returned as is. A nil precondition publishes
// unconditionally.
//
// A refused write changes nothing, so it opens no commit: indexing the
// untouched destination would only read all of it again under the lock.
//
// The returned stat describes the published generation. It is taken from the
// scratch file before the rename, which keeps its inode, size and timestamp,
// so it names exactly what this call published even if another write replaces
// the destination right after the lock is released.
func PublishIf(root, scratch, dest string, precondition func(current os.FileInfo, err error) error) (os.FileInfo, error) {
	f, err := os.OpenFile(scratch, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return nil, err
	}
	info, hash, err := Snapshot(scratch)
	if err != nil {
		return nil, err
	}
	unlock := Lock(root)
	defer unlock()
	if precondition != nil {
		// The storage fence comes first: a precondition must not judge a
		// destination on a volume that is not the one it should be.
		if err := validateLocked(root); err != nil {
			return nil, err
		}
		current, statErr := os.Stat(dest)
		if err := precondition(current, statErr); err != nil {
			return nil, err
		}
	}
	err = commitPublishedLocked(root, nil, []string{dest}, map[string]Published{dest: {Info: info, Hash: hash}}, func() error {
		if BusyLocked(root, dest) {
			return ErrBusy
		}
		if err := os.Rename(scratch, dest); err != nil {
			return err
		}
		return SyncParents(filepath.Dir(dest), root)
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}
