package fileops

// Deleting a directory used to be one commit: os.RemoveAll under the volume
// lock, then the watcher indexed a tombstone for every file of the tree, still
// under the lock. On a 300k-file directory that held every write on the node
// for close to five minutes. A tree is now removed a batch of files at a time,
// each batch an ordinary commit that deletes and indexes only its own files, so
// other writers get the lock between batches.
//
// Every batch is complete on its own: its intents are durable before anything
// is unlinked and its tombstones are written before the lock is released. A
// crash between batches leaves a tree that is partly deleted and fully
// indexed — what is gone is tombstoned, what is left is untouched — and a
// crash inside one is resolved by the same intent recovery as any commit.
// Any other failure part way, such as a file whose name cannot be indexed or a
// directory that refuses an unlink, stops the removal the same way, with an
// error, where it used to refuse the whole tree before deleting anything.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A batch starts small and adapts so that one lock hold stays near the target,
// whatever the disk: indexing a file costs about a millisecond on an SSD and
// much more on a disk that has to seek for every fsync.
const (
	removeBatchFirst  = 64
	removeBatchMin    = 8
	removeBatchMax    = 512
	removeBatchTarget = 100 * time.Millisecond
)

// ErrTreeChanged reports a recursive removal that found an entry it had not
// walked: something was written into the tree while it was being removed. The
// new entry is kept, along with its directories.
var ErrTreeChanged = errors.New("directory changed while it was being removed")

// removeTreeBetweenBatches lets tests act while the lock is free.
var removeTreeBetweenBatches func()

// removeTree deletes the directory tree at path in bounded commits. The walk
// runs outside the lock; each full batch of files is removed and indexed under
// it, and the emptied directories go last, in one commit of their own that
// refuses to delete anything that is not a directory.
func removeTree(dir, path string) error {
	// A writer already working inside the tree refuses the whole removal before
	// anything is deleted, as it always has. Writers that start later are caught
	// by the batch that reaches their file.
	if err := Do(dir, func() error {
		if writerInTreeLocked(dir, path) {
			return ErrBusy
		}
		return nil
	}); err != nil {
		return err
	}
	size := removeBatchFirst
	batch := make([]string, 0, removeBatchMax)
	kept := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		held, busy, err := removeBatch(dir, batch)
		kept += busy
		batch = batch[:0]
		size = nextRemoveBatch(size, held)
		if err == nil && removeTreeBetweenBatches != nil {
			removeTreeBetweenBatches()
		}
		return err
	}
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory emptied by an earlier batch may already have been
			// cleaned up by the indexer.
			if gone(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		batch = append(batch, p)
		if len(batch) >= size {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return err
	}
	err = Commit(dir, []string{path}, func() error { return removeEmptyTreeLocked(dir, path) })
	if kept > 0 && (err == nil || errors.Is(err, ErrTreeChanged)) {
		return fmt.Errorf("%w: %d file(s) being written were kept", ErrBusy, kept)
	}
	return err
}

// removeBatch removes and indexes files in one commit and reports how long it
// held the lock and how many files it left because a writer holds them —
// opened after the removal started, or through a hard link from outside the
// tree. Those are kept with their directories and the removal ends in ErrBusy;
// failing the whole batch instead stopped the removal half done.
func removeBatch(dir string, files []string) (time.Duration, int, error) {
	unlock := Lock(dir)
	acquired := time.Now()
	idle := files[:0:0]
	for _, file := range files {
		if !BusyLocked(dir, file) {
			idle = append(idle, file)
		}
	}
	busy := len(files) - len(idle)
	files = idle
	if len(files) == 0 {
		unlock()
		return 0, busy, nil
	}
	err := commitLocked(dir, nil, files, func() error {
		parents := make(map[string]bool)
		for _, file := range files {
			info, err := os.Lstat(file)
			if gone(err) {
				continue
			}
			if err != nil {
				return err
			}
			if info.IsDir() {
				// Replaced by a directory since the walk; the last commit
				// decides about directories.
				continue
			}
			if err := os.Remove(file); err != nil && !gone(err) {
				return err
			}
			parents[filepath.Dir(file)] = true
		}
		for parent := range parents {
			if err := SyncDir(parent); err != nil && !gone(err) {
				return err
			}
		}
		return nil
	})
	held := time.Since(acquired)
	unlock()
	return held, busy, err
}

// removeEmptyTreeLocked removes the directories left at path, deepest first.
// Anything else in them was written after the walk passed, or is still being
// written: it is kept, with the directories leading to it, and the removal
// reports ErrTreeChanged.
func removeEmptyTreeLocked(dir, path string) error {
	var dirs []string
	var changed error
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if gone(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			if changed == nil {
				changed = fmt.Errorf("%w: %s", ErrTreeChanged, p)
			}
			return nil
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		// rmdir is the emptiness check: a directory holding something new
		// stays, and so does every directory above it.
		if err := os.Remove(dirs[i]); err != nil && !gone(err) && changed == nil {
			return err
		}
	}
	return errors.Join(changed, syncRemovedParent(dir, path))
}

// writerInTreeLocked reports an open writer for path or anything below it. It
// is BusyTreeLocked without the walk for hard links, which each batch makes
// per file instead.
func writerInTreeLocked(dir, path string) bool {
	abs := canonical(path)
	sep := string(filepath.Separator)
	for _, w := range root(dir).writers {
		if w.path == abs || w.requested == path || strings.HasPrefix(w.path, abs+sep) || strings.HasPrefix(w.requested, path+sep) {
			return true
		}
	}
	return false
}

// nextRemoveBatch scales the batch toward the target hold time, at most
// doubling per step so one fast batch does not overshoot on a slow disk.
func nextRemoveBatch(size int, held time.Duration) int {
	next := 2 * size
	if held > 0 {
		next = min(next, int(float64(size)*float64(removeBatchTarget)/float64(held)))
	}
	return max(removeBatchMin, min(removeBatchMax, next))
}

func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
