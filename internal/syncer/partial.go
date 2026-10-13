package syncer

// Resumable replica transfers.
//
// A replica used to be staged in a fresh scratch file that every failure
// deleted, and startup deleted any that were left. On a link that drops now
// and then, a 500 MB APK interrupted at 90% started again from zero, and could
// keep doing so for as long as the link kept dropping.
//
// A large transfer now keeps its scratch file when it stops part-way. How far
// it got is recorded only after those bytes are flushed, together with the
// SHA-256 of that prefix. The next attempt — after a retry or a restart —
// rehashes the prefix, which it needs for the final checksum anyway, and asks
// the peer for the rest with an If-Range guard naming the content hash. A peer
// that no longer holds those exact bytes answers with the whole file, a prefix
// that no longer matches its record is dropped, and either way the transfer
// falls back to a full download. The full checksum is still verified before
// anything is published.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
)

const (
	// resumableBytes is the smallest transfer worth keeping when it stops: below
	// it, fetching the file again costs less than the bookkeeping.
	resumableBytes = 1 << 20
	// checkpointBytes is how much may arrive between two durable checkpoints.
	// A failure the process sees records its exact position anyway; this bounds
	// what a crash or power loss costs.
	checkpointBytes = 16 << 20
	// partialMaxAge forgets a partial transfer nobody has touched for a week.
	partialMaxAge = 7 * 24 * time.Hour
	// partialPrefix names the scratch files a transfer is staged in.
	partialPrefix = ".birak-tmp-replica-"
)

var (
	errPartialStale = errors.New("partial download no longer matches its record")
	errTooLong      = errors.New("peer sent more than the advertised size")
)

// stage is the scratch file one transfer writes into. offset bytes of it are
// hashed into hasher; checkpointed of those are durable and recorded.
type stage struct {
	s            *Syncer
	meta         store.FileMeta
	file         *os.File
	path         string // as opened
	rel          string // slash-separated, relative to the sync root
	offset       int64
	checkpointed int64
	hasher       hash.Hash
	resumable    bool
	recorded     bool
	dirSynced    bool
	invalid      bool // the bytes are known to be wrong; never keep them
	done         bool // published or discarded
}

// loadPartials adopts the partial transfers recorded before a restart, so the
// startup janitor leaves their scratch files alone. A record is dropped when
// its file is gone, it has expired, or no queued work could ever resume it;
// the janitor then removes the file like any other abandoned scratch.
func (s *Syncer) loadPartials() {
	s.partials = make(map[string]store.PartialDownload)
	records, err := s.store.PartialDownloads()
	if err != nil {
		s.logger.Error("read partial downloads failed", "error", err)
		return
	}
	if len(records) == 0 {
		return
	}
	names := make([]string, len(records))
	for i, rec := range records {
		names[i] = rec.Meta.Name
	}
	queued, err := s.store.QueuedNames(names)
	if err != nil {
		s.logger.Error("read partial downloads failed", "error", err)
		return
	}
	for _, rec := range records {
		reason := ""
		full, ok := s.partialPath(rec.Path)
		switch {
		case !ok:
			reason = "invalid scratch path"
		case !queued[rec.Meta.Name]:
			reason = "no queued work"
		case time.Since(rec.Updated) > partialMaxAge:
			reason = "expired"
		default:
			info, err := os.Lstat(full)
			if err != nil || !info.Mode().IsRegular() || info.Size() < rec.Verified {
				reason = "scratch file missing or short"
			}
		}
		if reason != "" {
			s.logger.Info("dropping partial download", "name", rec.Meta.Name, "reason", reason)
			if err := s.store.DropPartialDownload(rec.Meta.Name); err != nil {
				s.logger.Warn("drop partial download failed", "name", rec.Meta.Name, "error", err)
			}
			continue
		}
		fileops.RetainTemp(full)
		s.partials[rec.Meta.Name] = rec
	}
	if len(s.partials) > 0 {
		s.logger.Info("partial downloads kept for resumption", "count", len(s.partials))
	}
}

// partialPath maps a recorded scratch path into the sync root, accepting only
// a replica scratch file there.
func (s *Syncer) partialPath(rel string) (string, bool) {
	native := filepath.FromSlash(rel)
	if !filepath.IsLocal(native) || filepath.ToSlash(filepath.Clean(native)) != rel ||
		!strings.HasPrefix(filepath.Base(native), partialPrefix) || strings.HasPrefix(rel, ".birak/") {
		return "", false
	}
	return filepath.Join(s.syncDir, native), true
}

func (s *Syncer) partial(name string) (store.PartialDownload, bool) {
	s.partialsMu.Lock()
	defer s.partialsMu.Unlock()
	rec, ok := s.partials[name]
	return rec, ok
}

func (s *Syncer) rememberPartial(rec store.PartialDownload) {
	s.partialsMu.Lock()
	defer s.partialsMu.Unlock()
	if s.partials == nil {
		s.partials = make(map[string]store.PartialDownload)
	}
	s.partials[rec.Meta.Name] = rec
}

func (s *Syncer) forgetPartial(name string) {
	s.partialsMu.Lock()
	defer s.partialsMu.Unlock()
	delete(s.partials, name)
}

// dropPartial discards a recorded transfer and its scratch file.
func (s *Syncer) dropPartial(rec store.PartialDownload) {
	if err := s.store.DropPartialDownload(rec.Meta.Name); err != nil {
		s.logger.Warn("drop partial download failed", "name", rec.Meta.Name, "error", err)
		return
	}
	s.forgetPartial(rec.Meta.Name)
	if full, ok := s.partialPath(rec.Path); ok {
		removeScratch(full)
	}
}

// settlePartial forgets a kept transfer once this node holds its state or a
// newer one: nothing would ever resume it.
func (s *Syncer) settlePartial(name string) {
	if _, ok := s.partial(name); !ok {
		return
	}
	unlock := s.paths.lock(name)
	defer unlock()
	rec, ok := s.partial(name)
	if !ok {
		return
	}
	local, err := s.store.GetFile(name)
	if err != nil || store.CompareState(&rec.Meta, local) > 0 {
		return
	}
	s.dropPartial(rec)
}

// resumableStage reopens the partial transfer of meta's content, if there is
// one that can still be trusted, and drops any other partial transfer of the
// name. It returns nil when the download has to start from the beginning. The
// caller holds the path lock for meta.Name and must release a returned stage.
func (s *Syncer) resumableStage(ctx context.Context, meta store.FileMeta) (*stage, error) {
	rec, ok := s.partial(meta.Name)
	if !ok {
		return nil, nil
	}
	if meta.Size >= resumableBytes && rec.Meta.Hash == meta.Hash && rec.Meta.Size == meta.Size {
		st, err := s.resumeStage(ctx, rec, meta)
		if err == nil {
			return st, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.logger.Warn("partial download cannot be resumed; downloading it again",
			"name", meta.Name, "verified", rec.Verified, "error", err)
	}
	s.dropPartial(rec)
	return nil, nil
}

// newStage creates the scratch file for a download from the first byte.
func (s *Syncer) newStage(meta store.FileMeta) (*stage, error) {
	f, err := s.replicaTemp(meta.Name)
	if err != nil {
		return nil, err
	}
	st := &stage{s: s, meta: meta, file: f, path: f.Name(), hasher: sha256.New(), resumable: meta.Size >= resumableBytes}
	if rel, err := filepath.Rel(s.syncDir, f.Name()); err == nil {
		st.rel = filepath.ToSlash(rel)
	} else {
		st.resumable = false
	}
	return st, nil
}

// resumeStage reopens a recorded transfer and proves its prefix still holds the
// bytes that were recorded, rehashing them into the checksum the download
// continues.
func (s *Syncer) resumeStage(ctx context.Context, rec store.PartialDownload, meta store.FileMeta) (*stage, error) {
	f, full, err := s.openPartialFile(rec.Path)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if rec.Verified <= 0 || rec.Verified >= meta.Size || info.Size() < rec.Verified {
		return nil, errPartialStale
	}
	hasher := sha256.New()
	if err := hashPrefix(ctx, hasher, f, rec.Verified); err != nil {
		return nil, err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != rec.Prefix {
		return nil, fmt.Errorf("%w: checksum of the first %d bytes differs", errPartialStale, rec.Verified)
	}
	// Anything beyond the checkpoint was never confirmed durable.
	if err := f.Truncate(rec.Verified); err != nil {
		return nil, err
	}
	if _, err := f.Seek(rec.Verified, io.SeekStart); err != nil {
		return nil, err
	}
	keep = true
	return &stage{
		s: s, meta: meta, file: f, path: full, rel: rec.Path,
		offset: rec.Verified, checkpointed: rec.Verified, hasher: hasher,
		resumable: true, recorded: true, dirSynced: true,
	}, nil
}

// openPartialFile opens a recorded scratch file for writing, under the same
// lock and storage fence that created it.
func (s *Syncer) openPartialFile(rel string) (*os.File, string, error) {
	full, ok := s.partialPath(rel)
	if !ok {
		return nil, "", errPartialStale
	}
	unlock := fileops.Lock(s.syncDir)
	defer unlock()
	if err := s.watcher.CheckStorageLocked(); err != nil {
		return nil, "", err
	}
	// The directory may have been replaced since the transfer stopped.
	root, err := filepath.EvalSymlinks(s.syncDir)
	if err != nil {
		return nil, "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		return nil, "", err
	}
	if parent != root && !strings.HasPrefix(parent, root+string(filepath.Separator)) {
		return nil, "", errPartialStale
	}
	before, err := os.Lstat(full)
	if err != nil {
		return nil, "", err
	}
	if !before.Mode().IsRegular() {
		return nil, "", errPartialStale
	}
	f, err := os.OpenFile(full, os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, "", errors.Join(errPartialStale, err)
	}
	return f, full, nil
}

// hashPrefix feeds the first n bytes of f into h, honouring cancellation: the
// prefix of a large file is a lot to read for a process that is shutting down.
func hashPrefix(ctx context.Context, h hash.Hash, f *os.File, n int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := buf[:min(int64(len(buf)), n)]
		read, err := io.ReadFull(f, chunk)
		h.Write(chunk[:read])
		n -= int64(read)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return errPartialStale
			}
			return err
		}
	}
	return nil
}

// write appends bytes from the peer, checkpointing at the configured interval.
func (st *stage) write(p []byte) error {
	if _, err := st.file.Write(p); err != nil {
		return err
	}
	st.hasher.Write(p)
	st.offset += int64(len(p))
	if st.resumable && st.offset-st.checkpointed >= checkpointBytes && st.offset < st.meta.Size {
		if err := st.checkpoint(); err != nil {
			st.s.logger.Warn("record download progress failed", "name", st.meta.Name, "error", err)
		}
	}
	return nil
}

// receive copies the rest of the transfer into the stage.
func receive(st *stage, src io.Reader) error {
	buf := make([]byte, 256<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if st.offset+int64(n) > st.meta.Size {
				return errTooLong
			}
			if werr := st.write(buf[:n]); werr != nil {
				// A local write failure — a full disk, an I/O error — is not
				// progress worth keeping; holding the bytes would only prolong it.
				st.invalid = true
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// checkpoint makes the bytes received so far durable, then records them. The
// order is what lets a restart trust the record.
func (st *stage) checkpoint() error {
	if err := st.file.Sync(); err != nil {
		return err
	}
	if !st.dirSynced {
		if err := fileops.SyncDir(filepath.Dir(st.path)); err != nil {
			return err
		}
		st.dirSynced = true
	}
	rec := store.PartialDownload{
		Meta:     st.meta,
		Path:     st.rel,
		Verified: st.offset,
		Prefix:   hex.EncodeToString(st.hasher.Sum(nil)),
		Updated:  time.Now(),
	}
	if err := st.s.store.SavePartialDownload(rec); err != nil {
		return err
	}
	st.recorded = true
	st.checkpointed = st.offset
	st.s.rememberPartial(rec)
	return nil
}

// restart empties the stage for a peer that answered with the whole file.
func (st *stage) restart() error {
	if st.recorded {
		if err := st.s.store.DropPartialDownload(st.meta.Name); err != nil {
			return err
		}
		st.recorded = false
		st.s.forgetPartial(st.meta.Name)
	}
	if err := st.file.Truncate(0); err != nil {
		return err
	}
	if _, err := st.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	st.hasher.Reset()
	st.offset, st.checkpointed = 0, 0
	return nil
}

func (st *stage) closeFile() error {
	if st.file == nil {
		return nil
	}
	err := st.file.Close()
	st.file = nil
	return err
}

// discard removes the scratch file and forgets any record of it.
func (st *stage) discard() {
	if st.done {
		return
	}
	st.done = true
	st.closeFile()
	if st.recorded {
		if err := st.s.store.DropPartialDownload(st.meta.Name); err != nil {
			st.s.logger.Warn("drop partial download failed", "name", st.meta.Name, "error", err)
		}
		st.s.forgetPartial(st.meta.Name)
	}
	removeScratch(st.path)
}

// published retires a stage whose file was renamed into place.
func (st *stage) published() {
	st.done = true
	st.closeFile()
	if st.recorded {
		if err := st.s.store.DropPartialDownload(st.meta.Name); err != nil {
			// A leftover record names a file that no longer exists; the next
			// start drops it.
			st.s.logger.Warn("drop partial download failed", "name", st.meta.Name, "error", err)
		}
		st.s.forgetPartial(st.meta.Name)
	}
	fileops.ReleaseTempPath(st.path)
}

// release ends one attempt. A transfer that stopped part-way keeps its file and
// a durable record of how far it got; anything else is discarded.
func (st *stage) release() {
	if st.done {
		return
	}
	if !st.invalid && st.resumable && st.offset > 0 && st.offset < st.meta.Size {
		if st.offset > st.checkpointed {
			if err := st.checkpoint(); err != nil {
				st.s.logger.Warn("keep partial download failed", "name", st.meta.Name, "error", err)
				st.discard()
				return
			}
		}
		st.closeFile()
		st.s.logger.Info("partial download kept", "name", st.meta.Name, "bytes", st.offset, "size", st.meta.Size)
		return
	}
	st.discard()
}

func removeScratch(path string) {
	fileops.ReleaseTempPath(path)
	os.Remove(path)
}

// parseContentRange reads "bytes first-last/total".
func parseContentRange(value string) (first, last, total int64, ok bool) {
	rest, found := strings.CutPrefix(value, "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	span, size, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, 0, false
	}
	from, to, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, 0, false
	}
	var err1, err2, err3 error
	first, err1 = strconv.ParseInt(from, 10, 64)
	last, err2 = strconv.ParseInt(to, 10, 64)
	total, err3 = strconv.ParseInt(size, 10, 64)
	return first, last, total, err1 == nil && err2 == nil && err3 == nil
}
