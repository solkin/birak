// Package fileops coordinates short filesystem commits across gateways,
// indexing and replication. Network transfers never hold a commit lock.
package fileops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var ErrBusy = errors.New("file is being written")

type rootState struct {
	mu      sync.Mutex
	writers map[*os.File]*writer
	hooks   Hooks
	notify  func()

	// Every commit on a volume queues behind mu, so how long it is held is the
	// one number that explains write latency under load — and the only way to
	// tell "this node is slow" from "this node is serialized".
	lockCount atomic.Int64
	lockHeld  atomic.Int64 // nanoseconds
	lockWorst atomic.Int64 // nanoseconds
}

// LockLoad reports how much time commits have spent holding a volume's lock.
type LockLoad struct {
	Acquisitions int64
	Held         time.Duration
	Worst        time.Duration
}

// LockStats returns the accumulated hold time for a volume.
func LockStats(dir string) LockLoad {
	r := root(dir)
	return LockLoad{
		Acquisitions: r.lockCount.Load(),
		Held:         time.Duration(r.lockHeld.Load()),
		Worst:        time.Duration(r.lockWorst.Load()),
	}
}

var roots sync.Map
var rootsMu sync.Mutex // binds alternate names to one stable controller

func root(dir string) *rootState {
	abs, _ := filepath.Abs(dir)
	if v, ok := roots.Load(abs); ok {
		return v.(*rootState)
	}
	rootsMu.Lock()
	defer rootsMu.Unlock()
	if v, ok := roots.Load(abs); ok {
		return v.(*rootState)
	}
	real := canonical(abs)
	v, ok := roots.Load(real)
	if !ok {
		v = &rootState{writers: make(map[*os.File]*writer)}
		roots.Store(real, v)
	}
	// Keep an established spelling bound even if a symlink/mount is replaced.
	// The watcher's sentinel check must still fence that path afterwards.
	roots.Store(abs, v)
	return v.(*rootState)
}

// Lock serializes namespace commits, including directory renames/deletions.
// Callers must not recursively acquire it.
func Lock(dir string) func() {
	r := root(dir)
	r.mu.Lock()
	return r.release(time.Now())
}

// release unlocks and accounts for the time the lock was held.
func (r *rootState) release(acquired time.Time) func() {
	return func() {
		held := time.Since(acquired).Nanoseconds()
		r.mu.Unlock()
		r.lockCount.Add(1)
		r.lockHeld.Add(held)
		for worst := r.lockWorst.Load(); held > worst; worst = r.lockWorst.Load() {
			if r.lockWorst.CompareAndSwap(worst, held) {
				break
			}
		}
	}
}

func Do(dir string, fn func() error) error {
	unlock := Lock(dir)
	defer unlock()
	if err := validateLocked(dir); err != nil {
		return err
	}
	return fn()
}

// SetNotifier is installed by the watcher. It only requests a rescan and must
// not acquire a commit lock or perform I/O.
func SetNotifier(dir string, notify func()) {
	unlock := Lock(dir)
	defer unlock()
	root(dir).notify = notify
}

// Published is bytes a commit has already read and already made durable. The
// indexer takes them instead of reading the file again under the lock, which is
// what kept a large write waiting for its own bytes twice.
type Published struct {
	Info os.FileInfo
	Hash string
}

// Hooks run under the root lock. The watcher owns validation and durable
// indexing; fileops owns filesystem mutations. No callback reacquires Lock.
type Hooks struct {
	Validate     func() error
	CheckSources func([]string) error
	Begin        func([]string) error
	// Finish indexes the result. Every commit fsyncs both what it publishes and
	// the directories it changes before Finish runs, so Finish is told the
	// result is durable and needs no second pass over the disk, and is handed
	// whatever the commit already hashed. A new commit path that publishes
	// without flushing owes both of those before it calls Finish.
	Finish func(paths []string, published map[string]Published) error
}

func SetHooks(dir string, hooks Hooks) { unlock := Lock(dir); defer unlock(); root(dir).hooks = hooks }
func validateLocked(dir string) error {
	if fn := root(dir).hooks.Validate; fn != nil {
		return fn()
	}
	return nil
}
func Commit(dir string, paths []string, fn func() error) error {
	return CommitFrom(dir, nil, paths, fn)
}

// CommitFrom verifies existing inputs before granting write intent to outputs.
// A damaged source must never become a trusted COPY/MOVE/partial upload.
func CommitFrom(dir string, sources, paths []string, fn func() error) error {
	unlock := Lock(dir)
	defer unlock()
	return commitLocked(dir, sources, paths, fn)
}
func checkSourcesLocked(dir string, paths []string) error {
	if fn := root(dir).hooks.CheckSources; fn != nil && len(paths) != 0 {
		return fn(paths)
	}
	return nil
}

func commitLocked(dir string, sources, paths []string, fn func() error) error {
	return commitPublishedLocked(dir, sources, paths, nil, fn)
}

func commitPublishedLocked(dir string, sources, paths []string, published map[string]Published, fn func() error) error {
	if err := validateLocked(dir); err != nil {
		return err
	}
	if err := checkSourcesLocked(dir, sources); err != nil {
		return err
	}
	h := root(dir).hooks
	if h.Begin != nil {
		if err := h.Begin(paths); err != nil {
			return err
		}
	}
	err := fn()
	if err != nil {
		if recoveryErr := validateLocked(dir); recoveryErr != nil {
			return errors.Join(err, recoveryErr)
		}
	}
	// Even a failed operation may have modified the namespace. Keep the intent
	// until its actual result has been indexed; never acknowledge stale metadata.
	if h.Finish != nil {
		err = errors.Join(err, h.Finish(paths, published))
	}
	if root(dir).notify != nil {
		root(dir).notify()
	}
	return err
}

type writer struct {
	path, requested string
	original        os.FileInfo
}

func canonical(path string) string {
	path, _ = filepath.Abs(path)
	if resolved, err := ResolvePath(path); err == nil {
		return resolved
	}
	return path
}

// ResolvePath resolves aliases through the nearest existing ancestor, retaining
// missing components so new files and deleted trees use the same namespace.
func ResolvePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	suffix := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if (!os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR)) || path == filepath.Dir(path) {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(path), suffix)
		path = filepath.Dir(path)
	}
}

// BusyLocked checks both names and inode identity (including hard links).
func BusyLocked(dir, path string) bool {
	abs := canonical(path)
	info, _ := os.Stat(path)
	for _, w := range root(dir).writers {
		if abs == w.path || path == w.requested || (info != nil && w.original != nil && os.SameFile(info, w.original)) {
			return true
		}
	}
	return false
}
func BusyTreeLocked(dir, path string) bool {
	abs := canonical(path)
	if BusyLocked(dir, path) {
		return true
	}
	for _, w := range root(dir).writers {
		if strings.HasPrefix(w.path, abs+string(filepath.Separator)) || strings.HasPrefix(w.requested, path+string(filepath.Separator)) {
			return true
		}
	}
	// A hard link inside a directory may alias a writer outside that directory.
	busy := false
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && BusyLocked(dir, p) {
			busy = true
			return filepath.SkipAll
		}
		return nil
	})
	return busy
}

func Rename(dir, src, dest string) error {
	return CommitFrom(dir, []string{src}, []string{src, dest}, func() error {
		if BusyTreeLocked(dir, src) || BusyTreeLocked(dir, dest) {
			return ErrBusy
		}
		if err := os.Rename(src, dest); err != nil {
			return err
		}
		return errors.Join(SyncParents(filepath.Dir(src), dir), SyncParents(filepath.Dir(dest), dir))
	})
}

func Remove(dir, path string, recursive bool) error {
	return Commit(dir, []string{path}, func() error {
		if BusyTreeLocked(dir, path) {
			return ErrBusy
		}
		var err error
		if recursive {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
		if err != nil {
			return err
		}
		parent := filepath.Dir(path)
		for {
			if _, err := os.Stat(parent); err == nil {
				break
			} else if !os.IsNotExist(err) {
				return err
			}
			if parent == filepath.Dir(parent) {
				return os.ErrNotExist
			}
			parent = filepath.Dir(parent)
		}
		return SyncParents(parent, dir)
	})
}

func Mkdir(dir, path string, mode os.FileMode, parents bool) error {
	return Do(dir, func() error {
		var err error
		if parents {
			err = os.MkdirAll(path, mode)
		} else {
			err = os.Mkdir(path, mode)
		}
		if err != nil {
			return err
		}
		return SyncParents(path, dir)
	})
}

// OpenWriter stages a complete generation. CLOSE is the publication boundary;
// disconnects and process termination leave the previous generation intact.
func OpenWriter(dir, path string, flag int, mode os.FileMode) (*os.File, func() error, error) {
	// The lock is released explicitly before the staged generation is seeded
	// from the current one, so it is not held across a whole file copy.
	r := root(dir)
	r.mu.Lock()
	release := r.release(time.Now())
	locked := true
	unlock := func() {
		if locked {
			locked = false
			release()
		}
	}
	defer unlock()
	if err := validateLocked(dir); err != nil {
		return nil, nil, err
	}
	if BusyLocked(dir, path) {
		return nil, nil, ErrBusy
	}
	dest := canonical(path)
	info, err := os.Stat(dest)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if os.IsNotExist(err) && flag&os.O_CREATE == 0 {
		return nil, nil, err
	}
	if info != nil {
		if !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("not a regular file: %s", path)
		}
		if flag&os.O_EXCL != 0 && flag&os.O_CREATE != 0 {
			return nil, nil, os.ErrExist
		}
		// Check write permission without truncating the published generation.
		probe, err := os.OpenFile(dest, os.O_WRONLY, 0)
		if err != nil {
			return nil, nil, err
		}
		probe.Close()
		if flag&os.O_TRUNC == 0 {
			if err := checkSourcesLocked(dir, []string{dest, path}); err != nil {
				return nil, nil, err
			}
		}
		mode = info.Mode().Perm()
	}
	f, err := CreateTemp(filepath.Dir(dest), ".birak-tmp-*")
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*os.File, func() error, error) {
		f.Close()
		os.Remove(f.Name())
		ReleaseTemp(f)
		return nil, nil, err
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	r.writers[f] = &writer{path: dest, requested: path, original: info}
	var once sync.Once
	var closeErr error
	closeWriter := func() error {
		once.Do(func() {
			// Flush and hash the staged bytes before taking the lock. The
			// scratch file belongs to this writer alone, so nothing can change
			// it meanwhile — and doing either under the lock would make every
			// other commit on this volume wait for one file to reach the disk.
			// The rename below keeps the inode, so this stat and hash describe
			// the published file exactly.
			syncErr := f.Sync()
			var published map[string]Published
			if syncErr == nil {
				if info, hash, err := Snapshot(f.Name()); err == nil {
					published = map[string]Published{dest: {Info: info, Hash: hash}}
				}
			}
			u := Lock(dir)
			defer u()
			if _, ok := r.writers[f]; !ok {
				closeErr = os.ErrClosed
				return
			}
			delete(r.writers, f)
			defer ReleaseTemp(f)
			defer os.Remove(f.Name())
			closeErr = errors.Join(syncErr, f.Close())
			if closeErr != nil {
				return
			}
			closeErr = commitPublishedLocked(dir, nil, []string{dest, path}, published, func() error {
				// External namespace replacement must not turn an open handle into a lost update.
				current, err := os.Stat(dest)
				if info != nil && (err != nil || !same(info, current)) {
					return ErrBusy
				}
				if info == nil && (!os.IsNotExist(err)) {
					if err != nil {
						return err
					}
					return ErrBusy
				}
				if err := os.Rename(f.Name(), dest); err != nil {
					return err
				}
				return SyncParents(filepath.Dir(dest), dir)
			})
		})
		return closeErr
	}

	// Seeding happens after the writer is registered and the lock is dropped.
	// Registration already makes the destination busy for every other commit on
	// this volume, so the copy needs no lock of its own — and holding one across
	// it meant that appending to a large file stopped every write on the node
	// for as long as the copy took. An external replacement during the copy is
	// caught by the generation check at publication, as it always was.
	if info != nil && flag&os.O_TRUNC == 0 {
		unlock()
		if err := seedFromCurrent(f, dest, info); err != nil {
			return nil, nil, errors.Join(err, AbortWriter(dir, f))
		}
	}
	return f, closeWriter, nil
}

// seedFromCurrent fills a staged generation with the bytes it replaces, so a
// write that does not truncate starts from the published content.
func seedFromCurrent(f *os.File, dest string, info os.FileInfo) error {
	source, err := os.Open(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, source); err != nil {
		return errors.Join(err, source.Close())
	}
	if err := source.Close(); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return os.Chtimes(f.Name(), info.ModTime(), info.ModTime())
}
func AbortWriter(dir string, f *os.File) error {
	unlock := Lock(dir)
	defer unlock()
	if _, ok := root(dir).writers[f]; !ok {
		return nil
	}
	delete(root(dir).writers, f)
	ReleaseTemp(f)
	return errors.Join(f.Close(), os.Remove(f.Name()))
}

// Snapshot hashes a stable regular inode and confirms the path still names it.
// This prevents recording a hash with stat data from a different generation.
func Snapshot(path string) (os.FileInfo, string, error) {
	before, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if !before.Mode().IsRegular() {
		return nil, "", fmt.Errorf("not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !same(before, opened) {
		return nil, "", ErrBusy
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, "", err
	}
	after, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if n != before.Size() || !same(before, after) {
		return nil, "", ErrBusy
	}
	return after, hex.EncodeToString(h.Sum(nil)), nil
}

// SameGeneration reports whether two stat results name the same unchanged file.
// A caller that hashed a file outside the commit lock uses this to prove, under
// the lock, that the bytes it read are still the ones at that path.
func SameGeneration(a, b os.FileInfo) bool { return a != nil && b != nil && same(a, b) }

func same(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode()
}

func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// SyncParents persists every directory entry up to and including the root,
// covering parent directories that MkdirAll may have just created.
func SyncParents(dir, boundary string) error {
	base, err := filepath.Abs(canonical(boundary))
	if err != nil {
		return err
	}
	current, err := filepath.Abs(canonical(dir))
	if err != nil {
		return err
	}
	for {
		if err := SyncDir(current); err != nil {
			return err
		}
		if current == base {
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("directory is outside sync root")
		}
		current = parent
	}
}

// SyncSurvivingParent also works after recursive removal of parent directories.
func SyncSurvivingParent(path, boundary string) error {
	parent := filepath.Dir(path)
	for {
		info, err := os.Stat(parent)
		if err == nil && info.IsDir() {
			return SyncParents(parent, boundary)
		}
		if err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) {
			return err
		}
		if parent == filepath.Dir(parent) {
			return os.ErrNotExist
		}
		parent = filepath.Dir(parent)
	}
}

// Publish fsyncs a completed scratch file before its atomic publication.
func Publish(root, scratch, dest string) error {
	f, err := os.OpenFile(scratch, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return Commit(root, []string{dest}, func() error {
		if BusyLocked(root, dest) {
			return ErrBusy
		}
		if err := os.Rename(scratch, dest); err != nil {
			return err
		}
		return SyncParents(filepath.Dir(dest), root)
	})
}

// RemoveReplicaFile removes only a file generation. An implicit directory or a
// regular-file ancestor means this file is already absent, not a failed unlink.
// Caller holds the namespace lock and has refreshed/compared the file state.
func RemoveReplicaFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("cannot remove unsupported file type: %s", path)
	}
	if err = os.Remove(path); os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	return err
}
