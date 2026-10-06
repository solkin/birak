// Package generation stores immutable, content-addressed file generations.
// It deliberately has no garbage collector: consensus references and retry
// records must be accounted for before any acknowledged generation is removed.
package generation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/birak/birak/internal/fileops"
)

var ErrCorrupt = errors.New("generation checksum or size mismatch")

type Ref struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

func (r Ref) Validate() error {
	b, err := hex.DecodeString(r.Hash)
	if err != nil || len(b) != sha256.Size || r.Hash != strings.ToLower(r.Hash) || r.Size < 0 {
		return errors.New("invalid generation reference")
	}
	return nil
}

type Store struct {
	dir         string
	directories map[string]os.FileInfo
	life        sync.RWMutex // Close waits for in-flight installs before releasing lease
	mu          sync.Mutex   // publishing/closing only; uploads do not hold this lock
	release     func() error
	// Explicit fault boundaries, also used by flush-failure/ENOSPC tests.
	syncFile func(*os.File) error
	syncDir  func(string) error
}

// SyncDir is strict. In particular, it must not turn an unsupported directory
// flush into a durable receipt (the legacy gateway helper is best-effort on NTFS).
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// MakeDir persists each newly created directory in its parent before returning.
func MakeDir(path string) error {
	if fi, err := os.Stat(path); err == nil {
		if !fi.IsDir() {
			return errors.New("generation parent is not a directory")
		}
		// Also retry a parent flush left incomplete by a previous attempt.
		return SyncDir(filepath.Dir(path))
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return errors.New("missing filesystem root")
	}
	if err := MakeDir(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	return SyncDir(parent)
}

func New(dir string) (*Store, error) {
	if err := MakeDir(dir); err != nil {
		return nil, err
	}
	release, err := fileops.AcquireLease(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, release: release, syncFile: (*os.File).Sync, syncDir: SyncDir, directories: make(map[string]os.FileInfo)}
	for _, sub := range []string{"objects", "staging"} {
		if err := MakeDir(filepath.Join(dir, sub)); err != nil {
			release()
			return nil, err
		}
	}
	// Only uncommitted scratch files are removed. Published generations survive
	// failed Raft operations as well as overwrites and deletes.
	entries, err := os.ReadDir(filepath.Join(dir, "staging"))
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				err = os.Remove(filepath.Join(dir, "staging", e.Name()))
				if err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		release()
		return nil, err
	}
	for _, sub := range []string{"", "objects", "staging"} {
		path := filepath.Join(dir, sub)
		info, err := os.Stat(path)
		if err != nil {
			release()
			return nil, err
		}
		s.directories[path] = info
	}
	return s, nil
}

// The volume is private. Replacing/unmounting one of these directories fences
// this process instead of creating a fresh empty store behind its voting ID.
func (s *Store) checkStorage() error {
	if s.release == nil {
		return os.ErrClosed
	}
	for path, expected := range s.directories {
		actual, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(expected, actual) {
			return errors.New("generation volume replaced; re-admission required")
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.life.Lock()
	defer s.life.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release == nil {
		return nil
	}
	err := s.release()
	s.release = nil
	return err
}

func (s *Store) path(ref Ref) string {
	return filepath.Join(s.dir, "objects", ref.Hash[:2], ref.Hash[2:])
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Stage streams an upload with a mandatory byte limit. No object-sized buffer
// or mutable pathname is used as a replication source.
func (s *Store) Stage(ctx context.Context, r io.Reader, limit int64) (Ref, error) {
	return s.put(ctx, r, limit, nil)
}

// Receive only acknowledges exact, verified bytes after flushing the file and
// its directory. A successful call is a durable receipt for this reference.
func (s *Store) Receive(ctx context.Context, ref Ref, r io.Reader) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	_, err := s.put(ctx, r, ref.Size, &ref)
	return err
}

func (s *Store) put(ctx context.Context, r io.Reader, limit int64, expected *Ref) (Ref, error) {
	s.life.RLock()
	defer s.life.RUnlock()
	var zero Ref
	if err := s.checkStorage(); err != nil {
		return zero, err
	}
	if limit < 0 || limit == int64(^uint64(0)>>1) {
		return zero, errors.New("invalid upload limit")
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "upload-")
	if err != nil {
		return zero, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, r}, limit+1))
	if err != nil {
		return zero, err
	}
	if n > limit {
		return zero, errors.New("generation exceeds upload limit")
	}
	ref := Ref{Hash: hex.EncodeToString(h.Sum(nil)), Size: n}
	if expected != nil && ref != *expected {
		return zero, ErrCorrupt
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := s.syncFile(f); err != nil {
		return zero, err
	}
	if err := f.Close(); err != nil {
		return zero, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkStorage(); err != nil {
		return zero, err
	}
	dest := s.path(ref)
	if err := MakeDir(filepath.Dir(dest)); err != nil {
		return zero, err
	}
	if _, err := os.Lstat(dest); err == nil {
		// Never replace a published inode, even to repair it: open readers and
		// receipts refer to immutable bytes. Explicit repair is a separate step.
		existing, err := s.open(ctx, ref)
		if err != nil {
			return zero, err
		}
		err = errors.Join(s.syncFile(existing), existing.Close())
		if err != nil {
			return zero, err
		}
	} else if !os.IsNotExist(err) {
		return zero, err
	} else if err := os.Rename(f.Name(), dest); err != nil {
		return zero, err
	}
	if err := s.syncDir(filepath.Dir(dest)); err != nil {
		return zero, err
	}
	if err := s.checkStorage(); err != nil {
		return zero, err
	}
	return ref, nil
}

// Open verifies before exposing bytes. It fails closed on missing/truncated/
// damaged generations. The returned descriptor must be closed by its caller.
// The private storage directory must not be modified by external writers.
func (s *Store) Open(ctx context.Context, ref Ref) (*os.File, error) {
	s.life.RLock()
	defer s.life.RUnlock()
	return s.open(ctx, ref)
}

func (s *Store) open(ctx context.Context, ref Ref) (*os.File, error) {
	if err := s.checkStorage(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	p := s.path(ref)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() != ref.Size {
		return nil, ErrCorrupt
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, f})
	if err == nil && (n != ref.Size || hex.EncodeToString(h.Sum(nil)) != ref.Hash) {
		err = fmt.Errorf("%s: %w", ref.Hash, ErrCorrupt)
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// VerifyDurable also repairs a possibly incomplete flush from an earlier failed
// install. Mere presence of a hash-named file is not a durable receipt.
func (s *Store) VerifyDurable(ctx context.Context, ref Ref) error {
	s.life.RLock()
	defer s.life.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release == nil {
		return os.ErrClosed
	}
	f, err := s.open(ctx, ref)
	if err != nil {
		return err
	}
	err = errors.Join(s.syncFile(f), f.Close())
	if err != nil {
		return err
	}
	if err = SyncDir(filepath.Dir(filepath.Dir(s.path(ref)))); err != nil {
		return err
	}
	if err = s.syncDir(filepath.Dir(s.path(ref))); err != nil {
		return err
	}
	return s.checkStorage()
}
