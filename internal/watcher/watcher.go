package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
	"github.com/fsnotify/fsnotify"
)

// FileEvent represents a detected file change.
type FileEvent struct {
	Name    string
	ModTime int64
	Size    int64
	Hash    string
	Deleted bool
}

// Watcher monitors a directory tree for file changes using fsnotify + periodic scan.
type Watcher struct {
	dir    string
	store  *store.Store
	logger *slog.Logger

	debounceWindow    time.Duration
	maxDebounceWindow time.Duration // max time to accumulate events before forced flush
	scanInterval      time.Duration
	ignorePatterns    []string

	// scrubRate is the byte budget per second for continuous verification.
	// Zero disables it.
	scrubRate int64

	statusMu    sync.Mutex
	lastScan    time.Time
	lastScrub   time.Time
	lastError   string
	readyOnce   sync.Once
	recovered   bool
	repairPeers []string
	integrity   map[string]bool // protected by statusMu
	storageID   string          // accessed under the shared filesystem commit lock

	// work carries debounced batches from the fsnotify loop to the processing
	// goroutine. Hashing and directory scans must never run on the event loop:
	// while they do, fsnotify events are not drained and the kernel queue
	// overflows, silently dropping changes.
	work chan []string

	// rescan requests an out-of-band full scan. It is the recovery path for
	// anything the event stream may have dropped.
	rescan chan struct{}

	// ready is closed after the first complete, successful scan.
	// Other components (e.g. syncer) should wait on this before starting.
	ready chan struct{}
}

// workQueueDepth bounds the batches buffered between the event loop and the
// processing goroutine. On overflow the watcher falls back to a full scan
// rather than blocking the event loop.
const workQueueDepth = 256

// DefaultScrubRate verifies roughly 28 GiB per hour. It is a background budget
// chosen so a large node finishes cycles in hours instead of trying, and
// failing, to re-read everything every scan interval.
const DefaultScrubRate = 8 << 20

// New creates a new Watcher.
func New(dir string, s *store.Store, logger *slog.Logger, debounceWindow, scanInterval time.Duration, ignorePatterns []string) *Watcher {
	dir, _ = filepath.Abs(dir)
	// Max debounce window: 10x the debounce window, at least 2 seconds.
	maxDebounce := debounceWindow * 10
	if maxDebounce < 2*time.Second {
		maxDebounce = 2 * time.Second
	}

	w := &Watcher{
		dir:               dir,
		store:             s,
		logger:            logger,
		debounceWindow:    debounceWindow,
		maxDebounceWindow: maxDebounce,
		scanInterval:      scanInterval,
		ignorePatterns:    ignorePatterns,
		ready:             make(chan struct{}),
		work:              make(chan []string, workQueueDepth),
		rescan:            make(chan struct{}, 1),
		integrity:         make(map[string]bool),
		scrubRate:         DefaultScrubRate,
	}
	fileops.SetNotifier(dir, w.requestRescan)
	fileops.SetHooks(dir, fileops.Hooks{Validate: w.prepareStorageLocked, CheckSources: w.checkSourcesLocked, Begin: w.beginCommitLocked, Finish: w.finishCommitLocked})
	return w
}

// SetScrubRate sets the continuous verification budget in bytes per second.
// Zero disables the scrub, which leaves silent corruption to be found by a peer
// or not at all. Call it before Run.
func (w *Watcher) SetScrubRate(bytesPerSecond int64) {
	w.scrubRate = max(0, bytesPerSecond)
}

// Ready returns a channel closed after the first complete, successful scan.
func (w *Watcher) Ready() <-chan struct{} {
	return w.ready
}

// shouldIgnore checks if a file path matches any of the configured ignore patterns.
// It matches each path segment's basename against the patterns.
func (w *Watcher) shouldIgnore(relPath string) bool {
	return ShouldIgnore(relPath, w.ignorePatterns)
}

// Run starts the watcher. It blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	// Ensure sync directory exists.
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("create sync dir %s: %w", w.dir, err)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	defer fsw.Close()

	// Recursively add all directories to fsnotify.
	failCount, walkErr := w.addDirsRecursive(fsw, w.dir)
	if walkErr != nil {
		return fmt.Errorf("add dirs to watcher: %w", walkErr)
	}
	if failCount > 0 {
		w.logger.Warn("some directories could not be watched; changes in them may be missed until the next periodic scan",
			"failed_watches", failCount,
			"hint", "on Linux run: sysctl -w fs.inotify.max_user_watches=1048576")
	}

	w.logger.Info("watcher started", "dir", w.dir)

	// The processing goroutine owns every expensive operation: hashing,
	// store writes and full directory scans. Keeping them off this loop is what
	// lets fsnotify events be drained continuously — a scan that blocked the
	// loop would let the kernel queue overflow and drop changes outright.
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		w.processLoop(ctx)
	}()
	// Verification runs on its own goroutine at its own budget: it must never
	// delay indexing, and indexing must never wait for it.
	workers.Add(1)
	go func() {
		defer workers.Done()
		w.scrubLoop(ctx)
	}()
	defer func() {
		close(w.work)
		workers.Wait()
	}()

	// Debounce timer and pending events.
	pending := make(map[string]struct{})
	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	// maxDebounceTimer is a hard deadline: if events keep arriving and the
	// debounce timer keeps resetting, we force a flush after maxDebounceWindow
	// to prevent starvation under sustained load.
	var maxDebounceTimer *time.Timer
	var maxDebounceCh <-chan time.Time

	flushPending := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}
		debounceCh = nil
		if maxDebounceTimer != nil {
			maxDebounceTimer.Stop()
		}
		maxDebounceCh = nil

		if len(pending) == 0 {
			return
		}
		names := make([]string, 0, len(pending))
		for name := range pending {
			names = append(names, name)
		}
		pending = make(map[string]struct{})
		w.submit(names)
	}

	for {
		select {
		case <-ctx.Done():
			flushPending()
			w.logger.Info("watcher stopped")
			return nil

		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}

			// Compute relative path from the sync dir.
			relPath, err := filepath.Rel(w.dir, event.Name)
			if err != nil || relPath == "." {
				continue
			}
			// Normalize to forward slashes for consistency.
			relPath = filepath.ToSlash(relPath)

			// Reject paths that escape the sync directory (e.g. "../meta/birak.db-wal").
			// On some platforms fsnotify may deliver events for sibling directories.
			if isOutsideSyncDir(relPath) {
				continue
			}

			// Skip ignored files.
			if w.shouldIgnore(relPath) {
				continue
			}

			// If a new directory was created, add it to fsnotify and scan for
			// files that might have been created before we started watching.
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if _, addErr := w.addDirsRecursive(fsw, event.Name); addErr != nil {
						w.logger.Error("failed to watch new directory", "path", relPath, "error", addErr)
					}
					// Walk the new directory to find files created before we started watching.
					_ = filepath.WalkDir(event.Name, func(path string, d fs.DirEntry, walkErr error) error {
						if walkErr != nil || d.IsDir() {
							return nil
						}
						rp, rpErr := filepath.Rel(w.dir, path)
						if rpErr != nil {
							return nil
						}
						name := filepath.ToSlash(rp)
						if !w.shouldIgnore(name) {
							pending[name] = struct{}{}
						}
						return nil
					})
					// Continue processing — don't add directory itself to pending.
				}
			}

			// For regular files, add to pending debounce set.
			if info, err := os.Stat(event.Name); err != nil || !info.IsDir() {
				pending[relPath] = struct{}{}
			}

			// Reset debounce timer (short delay after last event).
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(w.debounceWindow)
			debounceCh = debounceTimer.C

			// Start the max-wait deadline on the FIRST event in a series.
			// This prevents starvation when events arrive continuously.
			if maxDebounceCh == nil {
				maxDebounceTimer = time.NewTimer(w.maxDebounceWindow)
				maxDebounceCh = maxDebounceTimer.C
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			// This is where an inotify queue overflow surfaces. Events have
			// been lost, so the only sound response is to rebuild state from
			// the filesystem rather than trust the stream.
			w.logger.Error("fsnotify error, requesting full scan", "error", err)
			w.requestRescan()

		case <-debounceCh:
			flushPending()

		case <-maxDebounceCh:
			// Hard deadline reached — flush regardless of ongoing events.
			w.logger.Debug("max debounce deadline reached, flushing")
			flushPending()
		}
	}
}

// submit hands a debounced batch to the processing goroutine. It never blocks:
// stalling here would stop fsnotify from being drained, which is the very
// failure the split is meant to prevent. If the queue is full the batch is
// dropped and a full scan is requested instead, which rediscovers the same
// changes from disk.
func (w *Watcher) submit(names []string) {
	select {
	case w.work <- names:
	default:
		w.logger.Warn("watcher work queue full, falling back to full scan", "dropped", len(names))
		w.requestRescan()
	}
}

// requestRescan asks for an out-of-band full scan, coalescing repeats.
func (w *Watcher) requestRescan() {
	select {
	case w.rescan <- struct{}{}:
	default:
	}
}

// processLoop performs all expensive work: hashing batches, writing to the
// store, and running periodic scans. Being a single goroutine, scans can never
// overlap each other or a batch.
func (w *Watcher) processLoop(ctx context.Context) {
	// Initial scan picks up anything that changed while the daemon was down.
	w.periodicScan(ctx)

	scanTicker := time.NewTicker(w.scanInterval)
	defer scanTicker.Stop()

	for {
		select {
		case names, ok := <-w.work:
			if !ok {
				return
			}
			w.processBatch(names)

		case <-w.rescan:
			w.logger.Info("running requested full scan")
			w.periodicScan(ctx)
			drainTicker(scanTicker)

		case <-scanTicker.C:
			w.periodicScan(ctx)
			// Drain any ticks that accumulated while the scan was running.
			// Without this, a slow scan would be immediately followed by
			// another scan from the buffered tick.
			drainTicker(scanTicker)

		case <-ctx.Done():
			// Drain whatever the event loop already handed over, then stop.
			for {
				select {
				case names, ok := <-w.work:
					if !ok {
						return
					}
					w.processBatch(names)
				default:
					return
				}
			}
		}
	}
}

// drainTicker discards any pending ticks on a ticker channel.
func drainTicker(t *time.Ticker) {
	for {
		select {
		case <-t.C:
		default:
			return
		}
	}
}

// addDirsRecursive adds a directory and all its subdirectories to the fsnotify
// watcher. It returns the number of directories that could not be watched
// (e.g. because of inotify limits) and any walk-level error.
func (w *Watcher) addDirsRecursive(fsw *fsnotify.Watcher, root string) (failCount int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip inaccessible dirs
		}
		if !d.IsDir() {
			return nil
		}
		// Check if this directory should be ignored.
		if path != root {
			relPath, relErr := filepath.Rel(w.dir, path)
			if relErr == nil {
				relPath = filepath.ToSlash(relPath)
				if w.shouldIgnore(relPath) {
					return fs.SkipDir
				}
			}
		}
		if addErr := fsw.Add(path); addErr != nil {
			w.logger.Warn("failed to add dir to watcher", "path", path, "error", addErr)
			failCount++
		}
		return nil
	})
	return failCount, err
}

// processBatch inspects and publishes each file under the same lock used by
// gateway commits and replication. No stale event batch can overwrite a newer
// store entry at the end of a long scan.
func (w *Watcher) processBatch(names []string) {
	for _, name := range names {
		err := w.Refresh(name)
		if err == nil {
			continue
		}
		if errors.Is(err, fileops.ErrBusy) {
			// The writer holding this name indexes it when it commits. Recording
			// that as a local fault would fail readiness for a healthy node.
			w.logger.Debug("deferred indexing a file being written", "name", name)
			continue
		}
		if errors.Is(err, ErrIntegrity) {
			// Already quarantined and queued for repair, and counted in status.
			// One damaged name is not a reason to stop serving the rest.
			w.logger.Error("file quarantined", "name", name, "error", err)
			continue
		}
		w.setError(err)
		w.logger.Error("inspect file failed", "name", name, "error", err)
	}
}

func (w *Watcher) Refresh(name string) error {
	unlock := fileops.Lock(w.dir)
	defer unlock()
	return w.RefreshLocked(name)
}

// RefreshLocked synchronously indexes current bytes, including a gateway
// commit whose fsnotify event has not been processed yet. Caller holds Lock.
func (w *Watcher) RefreshLocked(name string) error {
	if err := w.prepareStorageLocked(); err != nil {
		return err
	}
	return w.refreshFileLocked(name, indexOptions{})
}

// hashed carries bytes read outside the commit lock. It is used only while the
// file's generation is provably unchanged; otherwise the file is read again.
type hashed struct {
	info os.FileInfo
	hash string
}

// scanFile indexes one name, reading and hashing it *before* taking the commit
// lock. Hashing under that lock made every gateway write wait for the largest
// file in the tree; a file rewritten mid-read is detected and simply re-read.
func (w *Watcher) scanFile(name string) error {
	path := filepath.Join(w.dir, filepath.FromSlash(name))
	info, hash, err := fileops.Snapshot(path)
	if err != nil {
		// Not a stable regular file right now — missing, replaced, a directory,
		// a symlink, or held by a writer. The locked path classifies all of that.
		return w.Refresh(name)
	}
	unlock := fileops.Lock(w.dir)
	defer unlock()
	return w.refreshFileLocked(name, indexOptions{published: map[string]*hashed{name: {info: info, hash: hash}}})
}

func (w *Watcher) refreshFileLocked(name string, opts indexOptions) error {
	if fileops.BusyLocked(w.dir, filepath.Join(w.dir, filepath.FromSlash(name))) {
		return fileops.ErrBusy
	}
	if err := w.recoverReplicaLocked(name); err != nil {
		return err
	}
	if opts.trustStat && w.unchangedByStat(name) {
		return nil
	}
	ev, err := w.inspectFile(name, opts.published[name])
	if err != nil || ev == nil {
		return err
	}
	// A recovered rename or a direct filesystem write must be durable before
	// peers can consume its metadata, even if its original writer never fsynced.
	// A commit already did: every publishing path fsyncs its bytes before the
	// rename, so repeating it here only made each write wait for the disk twice.
	path := filepath.Join(w.dir, filepath.FromSlash(name))
	if !ev.Deleted && !opts.durable {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
	}
	// The directory entry needs the same treatment as the bytes: a commit has
	// already flushed the directories it changed, and repeating that here was
	// the second of two fsyncs held under the volume's one lock.
	if !opts.durable {
		if err := fileops.SyncSurvivingParent(path, w.dir); err != nil {
			return err
		}
	}
	_, err = w.store.PutLocal(store.FileMeta{Name: ev.Name, ModTime: ev.ModTime, Size: ev.Size, Hash: ev.Hash, Deleted: ev.Deleted})
	return err
}

func (w *Watcher) inspectFile(name string, precomputed *hashed) (*FileEvent, error) {
	if isOutsideSyncDir(name) || w.shouldIgnore(name) {
		return nil, nil
	}
	fullPath := filepath.Join(w.dir, filepath.FromSlash(name))
	if fileops.BusyLocked(w.dir, fullPath) {
		return nil, fileops.ErrBusy
	}
	existing, err := w.store.GetFile(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(fullPath)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		if existing == nil || existing.Deleted {
			w.clearIntegrity(name)
			return nil, nil
		}
		w.clearIntegrity(name)
		// The root was verified above; absence is a local deletion, not loss of a mount.
		CleanEmptyParentsLocked(fullPath, w.dir, w.ignorePatterns, w.logger)
		return &FileEvent{Name: name, ModTime: existing.ModTime + 1, Deleted: true}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		var safe bool
		info, safe = w.safeSymlinkInfo(fullPath)
		if !safe {
			if existing != nil && !existing.Deleted {
				return nil, fmt.Errorf("indexed file %q was replaced by an unsupported symlink", name)
			}
			return nil, nil
		}
		resolved, err := filepath.EvalSymlinks(fullPath)
		if err != nil {
			return nil, err
		}
		targets, err := w.relativePaths([]string{resolved})
		if err != nil {
			return nil, err
		}
		if targets[0] != name {
			// An alias without history must not publish a known damaged target.
			if err := w.refreshFileLocked(targets[0], indexOptions{}); err != nil {
				return nil, err
			}
		}
	}
	if !info.Mode().IsRegular() {
		trusted, err := w.store.HasLocalIntent(name)
		if err != nil {
			return nil, err
		}
		if info.IsDir() && trusted && existing != nil && !existing.Deleted {
			return &FileEvent{Name: name, ModTime: existing.ModTime, Deleted: true}, nil
		}
		if existing != nil && !existing.Deleted {
			return nil, fmt.Errorf("indexed file %q was replaced by a non-regular file", name)
		}
		return nil, nil
	}
	info, hash, err := w.snapshot(fullPath, precomputed)
	if err != nil {
		return nil, err
	}
	trusted, err := w.store.HasLocalIntent(name)
	if err != nil {
		return nil, err
	}
	if existing != nil && !existing.Deleted && existing.Hash != hash && existing.ModTime == info.ModTime().UnixNano() && existing.Size == info.Size() && !trusted {
		return nil, w.quarantine(existing)
	}
	w.clearIntegrity(name)
	if existing != nil && !existing.Deleted && existing.Hash == hash && existing.ModTime == info.ModTime().UnixNano() && existing.Size == info.Size() {
		return nil, nil
	}
	return &FileEvent{Name: name, ModTime: info.ModTime().UnixNano(), Size: info.Size(), Hash: hash}, nil
}

// snapshot returns the file's current size, timestamp and hash, reusing bytes
// already read outside the commit lock when the path still names that exact
// generation. Anything else is read again here.
func (w *Watcher) snapshot(path string, precomputed *hashed) (os.FileInfo, string, error) {
	if precomputed != nil {
		if current, err := os.Stat(path); err == nil && fileops.SameGeneration(precomputed.info, current) {
			return precomputed.info, precomputed.hash, nil
		}
	}
	return fileops.Snapshot(path)
}

// ScanStatus describes the last complete checksum scan, independent of peers.
// A failed or incomplete scan never opens readiness.
type ScanStatus struct {
	Ready         bool   `json:"ready"`
	LastScanAgoMS int64  `json:"last_scan_ms_ago"`
	LastError     string `json:"last_error,omitempty"`
	// Quarantined counts names whose bytes changed without a write timestamp.
	// They are individually unavailable and awaiting repair from a peer.
	Quarantined int `json:"quarantined"`
	// LastScrubAgoMS is the age of the last completed verification cycle, or -1
	// when no cycle has finished yet. Readiness does not depend on it: the scrub
	// is a continuous background budget, not a deadline.
	LastScrubAgoMS int64 `json:"last_scrub_ms_ago"`
}

func (w *Watcher) Status() ScanStatus {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	status := ScanStatus{LastError: w.lastError, LastScanAgoMS: -1, LastScrubAgoMS: -1, Quarantined: len(w.integrity)}
	if !w.lastScrub.IsZero() {
		status.LastScrubAgoMS = time.Since(w.lastScrub).Milliseconds()
	}
	if !w.lastScan.IsZero() {
		status.LastScanAgoMS = time.Since(w.lastScan).Milliseconds()
		// Readiness answers "can this node serve and accept writes", which is a
		// property of the process and the volume. A damaged file is unavailable
		// by name and counted in Quarantined; it does not take the other million
		// files out of rotation, and — when no peer holds a healthy copy — it
		// used to take them out permanently.
		status.Ready = w.lastError == "" && time.Since(w.lastScan) < 2*w.scanInterval+time.Minute
	}
	return status
}

func (w *Watcher) setError(err error) {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	w.lastError = err.Error()
}

func (w *Watcher) CheckStorage() error {
	unlock := fileops.Lock(w.dir)
	defer unlock()
	return w.CheckStorageLocked()
}

// CheckStorageLocked validates recovery/storage while the caller holds Lock.
func (w *Watcher) CheckStorageLocked() error { return w.prepareStorageLocked() }

// A persisted sentinel binds this metadata database to its data volume. A
// missing/remounted root after restart must fail closed, not delete the cluster.
func (w *Watcher) checkStorageLocked() error {
	info, err := os.Stat(w.dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sync root is not a directory")
	}
	if w.storageID == "" {
		w.storageID, err = w.store.NodeValue("storage_id")
		if err != nil {
			return err
		}
	}
	private := filepath.Join(w.dir, ".birak")
	if info, err := os.Lstat(private); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("storage state directory must be a real directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	marker := filepath.Join(private, "storage-id")
	if info, err := os.Lstat(marker); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("invalid storage sentinel")
	}
	data, err := os.ReadFile(marker)
	if w.storageID != "" {
		if err != nil {
			return fmt.Errorf("data volume sentinel unavailable: %w", err)
		}
		if string(data) != w.storageID {
			return fmt.Errorf("data volume identity mismatch")
		}
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	id := string(data)
	if id == "" {
		// Migration from pre-sentinel releases: an empty data root alongside live
		// metadata could be an absent mount. Require an operator to restore it.
		count, err := w.store.FileCount()
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(w.dir)
		if err != nil {
			return err
		}
		visible := false
		for _, entry := range entries {
			if !w.shouldIgnore(entry.Name()) {
				visible = true
				break
			}
		}
		if count > 0 && !visible {
			return fmt.Errorf("data root is empty but metadata contains live files")
		}
		id = w.store.Epoch()
		if err := os.MkdirAll(private, 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := f.WriteString(id)
		err = errors.Join(writeErr, f.Sync(), f.Close())
		if err != nil {
			return err
		}
		if err := fileops.SyncParents(private, w.dir); err != nil {
			return err
		}
	}
	if err := w.store.SetNodeValue("storage_id", id); err != nil {
		return err
	}
	w.storageID = id
	return nil
}

// periodicScan is a checksum scrub, not just an mtime/size comparison. Changes
// are published one by one using fresh snapshots; memory holds only names.
// Any traversal error suppresses the entire deletion pass.
func (w *Watcher) periodicScan(ctx context.Context) (result error) {
	// complete distinguishes "the scan could not finish" from "the scan
	// finished and found damaged files". Only the first is a storage fault.
	complete := false
	defer func() {
		if !complete {
			if result != nil {
				w.setError(result)
				w.logger.Error("scan incomplete", "error", result)
			}
			return
		}
		w.statusMu.Lock()
		w.lastScan = time.Now()
		w.lastError = ""
		w.statusMu.Unlock()
		w.readyOnce.Do(func() { close(w.ready) })
	}()
	if err := w.CheckStorage(); err != nil {
		return err
	}
	var scanErr, integrityErr error
	err := filepath.WalkDir(w.dir, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			scanErr = errors.Join(scanErr, walkErr)
			return nil
		}
		if path == w.dir {
			return nil
		}
		rel, err := filepath.Rel(w.dir, path)
		if err != nil {
			scanErr = errors.Join(scanErr, err)
			return nil
		}
		name := filepath.ToSlash(rel)
		if w.shouldIgnore(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if err := w.sweepFile(name); err != nil {
			switch {
			case errors.Is(err, ErrIntegrity):
				integrityErr = errors.Join(integrityErr, err)
			case errors.Is(err, fileops.ErrBusy):
				// A gateway owns this name right now. Its own commit indexes the
				// result, so an upload in flight is not an incomplete scan — and
				// must not take a node that is serving traffic out of rotation.
				w.logger.Debug("scan skipped a file being written", "name", name)
			default:
				scanErr = errors.Join(scanErr, err)
			}
		}
		return nil
	})
	if err != nil || scanErr != nil {
		return errors.Join(err, scanErr)
	}
	var after string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := w.store.ListNonDeleted(after, 1000)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, meta := range page {
			after = meta.Name
			if w.shouldIgnore(meta.Name) {
				continue
			}
			// Names the walk already reconciled are answered by a single stat and
			// cost nothing more. Remembering which ones those were would mean
			// holding every name in memory at once — a gigabyte on a tree of ten
			// million files, allocated by a background sweep, which is the kind
			// of thing that takes a node down at exactly the wrong moment.
			//
			// A file may also have appeared since the walk. Both paths recheck
			// disk and store under the commit lock, so neither can publish a
			// stale deletion.
			if err := w.sweepFile(meta.Name); err != nil && !errors.Is(err, fileops.ErrBusy) && !errors.Is(err, ErrIntegrity) {
				return err
			}
		}
	}
	if err := w.CheckStorage(); err != nil {
		return err
	}
	complete = true
	if integrityErr != nil {
		w.logger.Error("scan found damaged files awaiting repair", "error", integrityErr)
	}
	return integrityErr
}

// safeSymlinkInfo follows a file symlink only when its target remains inside the
// sync root and is not hidden by reserved or configured ignore rules. Birak
// replicates the target bytes as a regular file; links to directories, private
// state, or external files are not indexable.
func (w *Watcher) safeSymlinkInfo(path string) (os.FileInfo, bool) {
	absRoot, err := filepath.Abs(w.dir)
	if err != nil {
		return nil, false
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, false
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil || (resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(filepath.Separator))) {
		return nil, false
	}
	targetRel, err := filepath.Rel(realRoot, resolved)
	if err != nil || targetRel == "." || w.shouldIgnore(filepath.ToSlash(targetRel)) {
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, false
	}
	return info, true
}

func hashFile(path string) (string, error) {
	_, hash, err := fileops.Snapshot(path)
	return hash, err
}

// CleanEmptyParents removes parent directories up to (but not including) rootDir,
// only when they are actually empty. Ignore patterns exclude files from
// replication; they never grant permission to delete local-only contents.
func CleanEmptyParents(filePath, rootDir string, ignorePatterns []string, logger *slog.Logger) {
	unlock := fileops.Lock(rootDir)
	defer unlock()
	CleanEmptyParentsLocked(filePath, rootDir, ignorePatterns, logger)
}

// CleanEmptyParentsLocked requires the shared namespace lock.
func CleanEmptyParentsLocked(filePath, rootDir string, ignorePatterns []string, logger *slog.Logger) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return
	}
	r, err := os.OpenRoot(absRoot)
	if err != nil {
		return
	}
	defer r.Close()
	dir := filepath.Dir(filePath)
	for {
		absDir, _ := filepath.Abs(dir)
		if absDir == absRoot || !strings.HasPrefix(absDir, absRoot+string(filepath.Separator)) {
			break
		}
		if fileops.BusyTreeLocked(rootDir, dir) {
			break
		}
		rel, err := filepath.Rel(absRoot, absDir)
		if err != nil || !removeEmptyDirectory(r, rel) {
			break
		}
		logger.Debug("removed empty directory", "path", dir)
		if err := fileops.SyncDir(filepath.Dir(dir)); err != nil {
			logger.Error("persist directory cleanup failed", "error", err)
			break
		}
		dir = filepath.Dir(dir)
	}
}

// removeEmptyDirectory never unlinks files or symlinks. The final Remove is an
// atomic emptiness check by the OS, including files created after Lstat.
func removeEmptyDirectory(root *os.Root, dir string) bool {
	info, err := root.Lstat(dir)
	return err == nil && info.IsDir() && root.Remove(dir) == nil
}

// isOutsideSyncDir returns true if a relative path escapes the sync directory
// (e.g. starts with "../"). Such paths can arrive from fsnotify on some
// platforms and must be rejected to avoid watching meta/database files.
func isOutsideSyncDir(relPath string) bool {
	return strings.HasPrefix(relPath, "../") || relPath == ".."
}

// ShouldIgnore is exported for use by other packages (server, syncer).
// Internal scratch file patterns (.birak-tmp-* and .birak-bak-*) are always
// ignored regardless of user-provided patterns to prevent syncing temporary
// files created during atomic writes.
func ShouldIgnore(relPath string, patterns []string) bool {
	// Birak's private state directory contains staged multipart uploads and must
	// never be indexed or replicated as user data.
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) > 0 && parts[0] == ".birak" {
		return true
	}

	// Check the basename of the file.
	base := filepath.Base(relPath)
	if matched, _ := filepath.Match(".birak-tmp-*", base); matched {
		return true
	}
	if matched, _ := filepath.Match(".birak-bak-*", base); matched {
		return true
	}
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	// Also check each parent directory segment.
	for _, part := range parts[:len(parts)-1] {
		for _, pattern := range patterns {
			if matched, _ := filepath.Match(pattern, part); matched {
				return true
			}
		}
	}
	return false
}
