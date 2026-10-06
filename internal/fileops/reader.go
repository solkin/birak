package fileops

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var ErrUnreadable = errors.New("file is quarantined")

// GenerationKey identifies the local filesystem object, including attributes
// that distinguish inode reuse. Damaged generations stay fenced after another
// inode repairs their original name: a hard link may still name the old bytes.
func GenerationKey(path string, info os.FileInfo) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	device, inode, err := objectIdentity(resolved, info)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x:%x", device, inode, info.Size(), info.ModTime().UnixNano()), nil
}

// Reader keeps an open generation revocable after checksum verification finds
// corruption. Revocation is permanent for this descriptor, even if a later
// repair replaces the path and clears its quarantine. No lock is held across
// a network write; revocation waits only for a bounded local read on the
// affected descriptor. Already delivered bytes cannot be recalled.
type Reader struct {
	mu   sync.Mutex
	file *os.File
	root *rootState
	info os.FileInfo
	err  error // protected by mu
}

func OpenReader(dir, path string) (*Reader, error) {
	unlock := Lock(dir)
	defer unlock()
	if err := validateLocked(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("not a regular file: %s", path)
	}
	if err == nil && root(dir).hooks.CheckRead != nil {
		err = root(dir).hooks.CheckRead(path, info)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	r := &Reader{file: f, root: root(dir), info: info}
	if r.root.readers == nil {
		r.root.readers = make(map[*Reader]struct{})
	}
	r.root.readers[r] = struct{}{}
	return r, nil
}

// RevokeReadersLocked also fences aliases and descriptors opened before the
// damage was discovered. The caller holds the volume's commit lock.
func RevokeReadersLocked(dir, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	for reader := range root(dir).readers {
		if os.SameFile(info, reader.info) {
			reader.mu.Lock()
			reader.err = ErrUnreadable
			reader.mu.Unlock()
		}
	}
}

func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	return r.file.Read(p[:min(len(p), 64<<10)])
}

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		r.mu.Lock()
		if r.err != nil {
			err := r.err
			r.mu.Unlock()
			return n, err
		}
		count, err := r.file.ReadAt(p[n:min(len(p), n+(64<<10))], off+int64(n))
		r.mu.Unlock()
		n += count
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (r *Reader) Seek(off int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	return r.file.Seek(off, whence)
}
func (r *Reader) Stat() (os.FileInfo, error)               { return r.file.Stat() }
func (r *Reader) Name() string                             { return r.file.Name() }
func (r *Reader) Chmod(mode os.FileMode) error             { return r.file.Chmod(mode) }
func (r *Reader) Truncate(size int64) error                { return r.file.Truncate(size) }
func (r *Reader) WriteAt(p []byte, off int64) (int, error) { return 0, os.ErrPermission }
func (r *Reader) Close() error {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.root.readers, r)
	r.err = os.ErrClosed
	return r.file.Close()
}

var _ io.ReadSeekCloser = (*Reader)(nil)
