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

type ReaderLoad struct {
	Active, BytesRead, Revocations int64
}

// ReaderStats counts filesystem reads, including peer downloads and probes.
// BytesRead is not a certificate of successful network delivery.
func ReaderStats(dir string) ReaderLoad {
	r := root(dir)
	return ReaderLoad{r.readerCount.Load(), r.readBytes.Load(), r.readerRevocations.Load()}
}

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

// OpenReader takes no commit lock: a GET must not queue behind a replica
// apply or a maintenance pass, and thousands of them must not queue behind
// each other. The storage fence and the quarantine check are both safe to run
// alongside commits; see Hooks.
func OpenReader(dir, path string) (*Reader, error) {
	root := root(dir)
	if err := root.validateRead(dir); err != nil {
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
	if err != nil {
		f.Close()
		return nil, err
	}
	r := &Reader{file: f, root: root, info: info}
	// Register before checking. A quarantine that lands between the two then
	// either is seen by the check or finds this reader to revoke.
	root.readersMu.Lock()
	if root.readers == nil {
		root.readers = make(map[*Reader]struct{})
	}
	root.readers[r] = struct{}{}
	root.readerCount.Add(1)
	root.readersMu.Unlock()
	if check := root.hook().CheckRead; check != nil {
		if err := check(path, info); err != nil {
			r.Close()
			return nil, err
		}
	}
	return r, nil
}

// validateRead applies the storage fence to a read. A fence that has no
// read-side check is applied the way commits apply it, under the lock.
func (r *rootState) validateRead(dir string) error {
	h := r.hook()
	if h.ValidateRead != nil {
		return h.ValidateRead()
	}
	if h.Validate == nil {
		return nil
	}
	unlock := Lock(dir)
	defer unlock()
	return h.Validate()
}

// RevokeReadersLocked also fences aliases and descriptors opened before the
// damage was discovered. The caller holds the volume's commit lock and has
// already fenced the generation against new readers.
func RevokeReadersLocked(dir, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	RevokeGenerationLocked(dir, info)
}

// RevokeGenerationLocked revokes every registered reader of the object info
// describes. Readers registered after it has collected its targets run their
// own quarantine check, which already sees the caller's fence.
func RevokeGenerationLocked(dir string, info os.FileInfo) {
	root := root(dir)
	var targets []*Reader
	root.readersMu.Lock()
	for reader := range root.readers {
		if os.SameFile(info, reader.info) {
			targets = append(targets, reader)
		}
	}
	root.readersMu.Unlock()
	// Waiting for an in-flight read happens outside readersMu, so a slow disk
	// under one descriptor does not hold up every other open and close.
	for _, reader := range targets {
		reader.mu.Lock()
		if reader.err == nil {
			reader.err = ErrUnreadable
			reader.root.readerRevocations.Add(1)
		}
		reader.mu.Unlock()
	}
}

func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.file.Read(p[:min(len(p), 64<<10)])
	r.root.readBytes.Add(int64(n))
	return n, err
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
		r.root.readBytes.Add(int64(count))
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
	r.root.readersMu.Lock()
	if _, exists := r.root.readers[r]; exists {
		delete(r.root.readers, r)
		r.root.readerCount.Add(-1)
	}
	r.root.readersMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = os.ErrClosed
	return r.file.Close()
}

var _ io.ReadSeekCloser = (*Reader)(nil)
