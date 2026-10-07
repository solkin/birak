package generation

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// Reader pins a verified inode until Close. Detection of damage revokes every
// reader of that hash before a replacement is installed. Already sent bytes
// cannot be recalled; subsequent reads fail instead of continuing on damage.
type Reader struct {
	file    *os.File
	store   *Store
	ref     Ref
	mu      sync.Mutex
	revoked atomic.Bool
	closed  bool
}

func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return 0, err
	}
	return r.file.Read(p)
}
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return 0, err
	}
	return r.file.ReadAt(p, off)
}
func (r *Reader) Seek(off int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return 0, err
	}
	return r.file.Seek(off, whence)
}
func (r *Reader) check() error {
	if r.closed {
		return os.ErrClosed
	}
	if r.revoked.Load() {
		return ErrCorrupt
	}
	return nil
}
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.file.Close()
	r.store.readersMu.Lock()
	delete(r.store.readers[r.ref.Hash], r)
	if len(r.store.readers[r.ref.Hash]) == 0 {
		delete(r.store.readers, r.ref.Hash)
	}
	r.store.readersMu.Unlock()
	return err
}
func (s *Store) pin(f *os.File, ref Ref) *Reader {
	r := &Reader{file: f, store: s, ref: ref}
	s.readersMu.Lock()
	defer s.readersMu.Unlock()
	if s.readers[ref.Hash] == nil {
		s.readers[ref.Hash] = map[*Reader]struct{}{}
	}
	s.readers[ref.Hash][r] = struct{}{}
	return r
}
func (s *Store) revoke(ref Ref) {
	s.readersMu.Lock()
	var readers []*Reader
	for r := range s.readers[ref.Hash] {
		r.revoked.Store(true)
		readers = append(readers, r)
	}
	s.readersMu.Unlock()
	for _, r := range readers {
		r.mu.Lock()
		r.mu.Unlock()
	}
}
func (s *Store) pinned(hash string) bool {
	s.readersMu.Lock()
	defer s.readersMu.Unlock()
	return len(s.readers[hash]) != 0 || s.backupPins[hash] != 0
}

var _ io.ReadSeekCloser = (*Reader)(nil)
var ErrNoSpace = errors.New("insufficient space above configured reserve")
