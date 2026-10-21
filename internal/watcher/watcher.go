package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// Zero disables it. Atomic because an operator-facing setter that quietly
	// races the loop reading it is a trap, not a knob.
	scrubRate atomic.Int64

	statusMu    sync.Mutex
	lastScan    time.Time
	lastScrub   time.Time
	lastError   string
	readyOnce   sync.Once
	recovered   bool
	repairPeers []string
	// storageID is written under the shared filesystem commit lock and read
	// without it by the lock-free volume check.
	storageID atomic.Pointer[string]
	// fence is the outcome of the last full storage validation. Reads and
	// probes consult it instead of queueing for the commit lock to repeat it.
	fence atomic.Pointer[storageFence]
	// quarantined answers the read-path alias check without a stat of every
	// quarantined name on every GET.
	quarantined quarantineIndex

	// work carries debounced batches from the fsnotify loop to the processing
	// goroutine. Hashing and directory scans must never run on the event loop:
	// while they do, fsnotify events are not drained and the kernel queue
	// overflows, silently dropping changes.
	work chan []string

	// restamped counts files whose bytes match the index but whose timestamp
	// does not. Reported once, after the first sweep: see warnAboutRestamping.
	restamped atomic.Int64

	// Names that cannot be indexed are skipped, not treated as faults; see
	// unindexable. lastSkipped is what the last complete sweep skipped, by
	// reason; skipTotals counts every skip since start, events included.
	// Guarded by statusMu.
	lastSkipped     map[string]int
	lastSkipSamples []string
	skipTotals      map[string]int64

	// maxExternalDeletions and deletions are the breaker for deletions
	// inferred from absence; see breaker.go.
	maxExternalDeletions atomic.Int64
	deletions            deletionBudget

	// readErrors counts I/O errors reading indexed files, each of which
	// quarantined the file for repair from a peer.
	readErrors atomic.Int64

	// mtimeMeasured is set once the volume's timestamp granularity has been
	// measured. Guarded by the commit lock.
	mtimeMeasured bool

	// progressInterval is how often a long sweep reports how far it got.
	progressInterval time.Duration

	// now is the clock the deletion budget runs on; tests move it.
	now func() time.Time

	// rescan requests an out-of-band full scan. It is the recovery path for
	// anything the event stream may have dropped.
	rescan chan struct{}

	// ready is closed after the first complete, successful scan.
	// Other components (e.g. syncer) should wait on this before starting.
	ready chan struct{}

	// scanTicks replaces the scan ticker in tests.
	scanTicks <-chan time.Time
}

// workQueueDepth bounds the batches buffered between the event loop and the
// processing goroutine. On overflow the watcher falls back to a full scan
// rather than blocking the event loop.
const workQueueDepth = 256

// DefaultScrubRate verifies roughly 28 GiB per hour. It is a background budget
// chosen so a large node finishes cycles in hours instead of trying, and
// failing, to re-read everything every scan interval.
const DefaultScrubRate = 8 << 20

// sweepProgressInterval is how long a sweep runs before it says how far it
// got, and how often it says so again. The first index of a million files takes
// a long while, and it must not look like a hang.
const sweepProgressInterval = 30 * time.Second

// deviceOf reports the filesystem an entry is on. A variable so tests can
// present a directory as a mount point without mounting anything.
var deviceOf = func(_ string, info os.FileInfo) (uint64, bool) { return fileops.Device(info) }

// snapshotFile reads and hashes a file. A variable so tests can make a read
// fail the way a failing disk does.
var snapshotFile = fileops.Snapshot

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
		skipTotals:        make(map[string]int64),
		progressInterval:  sweepProgressInterval,
		now:               time.Now,
	}
	w.scrubRate.Store(DefaultScrubRate)
	w.maxExternalDeletions.Store(DefaultMaxExternalDeletions)
	fileops.SetNotifier(dir, w.requestRescan)
	fileops.SetHooks(dir, fileops.Hooks{Validate: w.prepareStorageLocked, ValidateRead: w.validateRead, CheckSources: w.checkSourcesLocked, CheckRead: w.checkRead, Begin: w.beginCommitLocked, Finish: w.finishCommitLocked})
	return w
}

// SetScrubRate sets the continuous verification budget in bytes per second.
// Zero disables the scrub, which leaves silent corruption to be found by a peer
// or not at all.
func (w *Watcher) SetScrubRate(bytesPerSecond int64) {
	w.scrubRate.Store(max(0, bytesPerSecond))
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

// processLoop performs all expensive work: hashing event batches, writing to
// the store, and running periodic scans. Being a single goroutine, scans can
// never overlap each other or a batch.
//
// Batches are taken into one backlog and indexed a name at a time, so the loop
// can stop between any two names. A sweep that comes due runs there too,
// instead of after the backlog: indexing a bulk upload of tens of thousands of
// files takes longer than the readiness window, and a node that is busy
// indexing must not report itself unready for it. The sweep leaves backlog
// names to the backlog, so it stays a stat walk rather than repeating the
// same indexing work.
func (w *Watcher) processLoop(ctx context.Context) {
	// Initial scan picks up anything that changed while the daemon was down.
	w.periodicScan(ctx)

	scanTicker := time.NewTicker(w.scanInterval)
	defer scanTicker.Stop()
	ticks := scanTicker.C
	if w.scanTicks != nil {
		ticks = w.scanTicks
	}
	backlog := newBacklog()
	sweep := func() {
		w.sweep(ctx, backlog.has)
		// Drain any ticks that accumulated while the scan was running.
		// Without this, a slow scan would be immediately followed by
		// another scan from the buffered tick.
		drainTicks(ticks)
	}

	for ctx.Err() == nil {
		if backlog.len() == 0 {
			// The burst is over: what it found missing is one group.
			w.settleEventDeletions(ctx)
			select {
			case names, ok := <-w.work:
				if !ok {
					return
				}
				backlog.add(names)
			case <-w.rescan:
				w.logger.Info("running requested full scan")
				sweep()
			case <-ticks:
				sweep()
			case <-ctx.Done():
				// Whatever was handed over and not yet indexed is found by the
				// next start's initial scan. Shutdown does not wait for it.
				return
			}
			continue
		}
		// Take what else has been handed over without waiting, so a sweep
		// knows every name that is already queued for indexing.
		w.absorb(backlog)
		select {
		case <-ticks:
			sweep()
			continue
		default:
		}
		w.processName(backlog.next())
	}
}

// maxBacklog bounds the names taken off the work queue ahead of indexing.
// Beyond it batches stay queued, and an overflowing queue falls back to a full
// scan as it always has.
const maxBacklog = 100_000

func (w *Watcher) absorb(b *backlog) {
	for b.len() < maxBacklog {
		select {
		case names, ok := <-w.work:
			if !ok {
				return
			}
			b.add(names)
		default:
			return
		}
	}
}

// backlog holds the names handed over by the event loop and not yet indexed,
// in arrival order and each once: a file written twice is indexed once.
type backlog struct {
	order []string
	head  int
	set   map[string]struct{}
}

func newBacklog() *backlog { return &backlog{set: make(map[string]struct{})} }

func (b *backlog) len() int { return len(b.order) - b.head }

func (b *backlog) has(name string) bool {
	_, ok := b.set[name]
	return ok
}

func (b *backlog) add(names []string) {
	for _, name := range names {
		if _, ok := b.set[name]; ok {
			continue
		}
		b.set[name] = struct{}{}
		b.order = append(b.order, name)
	}
}

func (b *backlog) next() string {
	name := b.order[b.head]
	b.order[b.head] = ""
	b.head++
	delete(b.set, name)
	switch {
	case b.head == len(b.order):
		b.order, b.head = b.order[:0], 0
	case b.head >= 4096 && 2*b.head >= len(b.order):
		b.order = append(b.order[:0], b.order[b.head:]...)
		b.head = 0
	}
	return name
}

// drainTicks discards any pending ticks.
func drainTicks(ticks <-chan time.Time) {
	for {
		select {
		case <-ticks:
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

// processBatch indexes names handed over by the event loop, stopping between
// any two names once ctx is cancelled, and then decides what it found missing
// as one group.
func (w *Watcher) processBatch(ctx context.Context, names []string) {
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		w.processName(name)
	}
	w.settleEventDeletions(ctx)
}

// processName hashes outside the commit lock, then checks the generation and
// current state under the same lock used by gateways and replication. A name
// found missing is collected rather than published; see breaker.go.
func (w *Watcher) processName(name string) {
	var deferDeletion func(string)
	if w.maxExternalDeletions.Load() > 0 {
		deferDeletion = w.deferEventDeletion
	}
	err := w.scanFileWith(name, deferDeletion)
	if err == nil {
		return
	}
	if errors.Is(err, fileops.ErrBusy) {
		// The writer holding this name indexes it when it commits. Recording
		// that as a local fault would fail readiness for a healthy node.
		w.logger.Debug("deferred indexing a file being written", "name", name)
		return
	}
	if errors.Is(err, ErrIntegrity) {
		// Already quarantined and queued for repair, and counted in status.
		// One damaged name is not a reason to stop serving the rest.
		w.logger.Error("file quarantined", "name", name, "error", err)
		return
	}
	if reason := unindexable(err); reason != "" {
		w.statusMu.Lock()
		w.skipTotals[reason]++
		w.statusMu.Unlock()
		w.logger.Warn("file cannot be indexed and is left alone",
			"name", strconv.Quote(name), "reason", reason, "error", err)
		return
	}
	w.setError(err)
	w.logger.Error("inspect file failed", "name", name, "error", err)
}

// Reasons a name is skipped rather than failing the sweep.
const (
	SkipNameCollision = "name_collision"
	SkipInvalidName   = "invalid_name"
	SkipVanished      = "vanished"
	SkipUnreadable    = "unreadable"
)

// SkipReasons lists every reason, for reporting a series per reason.
var SkipReasons = []string{SkipNameCollision, SkipInvalidName, SkipVanished, SkipUnreadable}

// errUnreadable marks a file this node cannot read, and has no indexed version
// of to repair.
var errUnreadable = errors.New("file cannot be read")

// storageFault marks a failure of the volume as a whole, found while indexing
// one name. It is never mistaken for that name's own problem.
type storageFault struct{ err error }

func (e *storageFault) Error() string { return e.err.Error() }
func (e *storageFault) Unwrap() error { return e.err }

// unindexable classifies errors that belong to one name rather than to the
// node: a spelling that collides with a reserved one, a name no node can
// store, or an entry this process is not permitted to read. Such a file can sit
// in the sync directory for weeks; failing every sweep on it took the whole
// node out of rotation, and after a restart kept replication from ever
// starting.
//
// An unreadable name is never evidence of a deletion: a file below a directory
// that lost its search permission cannot even be stat'ed, and must not become
// a tombstone on every peer because of it.
func unindexable(err error) string {
	var fault *storageFault
	switch {
	case errors.As(err, &fault):
		return ""
	case errors.Is(err, store.ErrNameCollision), errors.Is(err, fileops.ErrSpellingAlias):
		return SkipNameCollision
	case errors.Is(err, store.ErrInvalidName):
		return SkipInvalidName
	case errors.Is(err, errUnreadable), errors.Is(err, fs.ErrPermission):
		return SkipUnreadable
	}
	return ""
}

// unreadableEntry reports a walk error for one entry below the root that this
// process may not read.
func unreadableEntry(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}

// vanished reports a walk error caused by an entry removed while the walk was
// reading it, such as a directory deleted between listing and reading it.
func vanished(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// skipTally collects what one sweep skipped, each name once: the walk and the
// deletion pass may both meet the same unreadable file. Past maxSkipDedupe
// names a repeat is counted again rather than remembered, so a directory of a
// million files that lost its search permission costs no gigabyte of names.
type skipTally struct {
	counts  map[string]int
	samples []string
	seen    map[string]bool
}

const (
	maxSkipSamples = 10
	maxSkipDedupe  = 100_000
)

func (t *skipTally) add(reason, name string) {
	if t.counts == nil {
		t.counts = make(map[string]int)
		t.seen = make(map[string]bool)
	}
	if t.seen[name] {
		return
	}
	if len(t.seen) < maxSkipDedupe {
		t.seen[name] = true
	}
	t.counts[reason]++
	if len(t.samples) < maxSkipSamples {
		t.samples = append(t.samples, reason+": "+strconv.Quote(name))
	}
}

func (t *skipTally) total() int {
	n := 0
	for _, count := range t.counts {
		n += count
	}
	return n
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

// RefreshTrustedLocked observes a name the way a commit observes the
// destination it is about to replace: a file whose size and timestamp still
// match the index is taken as unchanged rather than read again under the lock.
// A quarantined name is always read. Caller holds Lock.
func (w *Watcher) RefreshTrustedLocked(name string) error {
	if err := w.prepareStorageLocked(); err != nil {
		return err
	}
	return w.refreshFileLocked(name, indexOptions{trustStat: true})
}

// RefreshPublishedLocked indexes a name whose bytes the caller has just
// published and already hashed, the way a commit hands its hash to Finish.
// info must describe the published inode; if the path no longer names that
// unchanged generation, the file is read again. Caller holds Lock.
func (w *Watcher) RefreshPublishedLocked(name string, info os.FileInfo, hash string) error {
	if err := w.prepareStorageLocked(); err != nil {
		return err
	}
	return w.refreshFileLocked(name, indexOptions{published: map[string]*hashed{name: {info: info, hash: hash}}})
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
func (w *Watcher) scanFile(name string) error { return w.scanFileWith(name, nil) }

// scanFileWith is scanFile for a caller that collects deletions instead of
// publishing them: a name found missing is handed to deferDeletion. A failure
// to validate the volume is returned as a storageFault, so it is never
// counted against the one name being indexed.
func (w *Watcher) scanFileWith(name string, deferDeletion func(string)) error {
	opts := indexOptions{deferDeletion: deferDeletion}
	path := filepath.Join(w.dir, filepath.FromSlash(name))
	// A file that is not a stable regular file right now — missing, replaced,
	// a directory, a symlink, unreadable, or held by a writer — is read again
	// by the locked path, which classifies all of that.
	if info, hash, err := snapshotFile(path); err == nil {
		opts.published = map[string]*hashed{name: {info: info, hash: hash}}
	}
	unlock := fileops.Lock(w.dir)
	defer unlock()
	if err := w.prepareStorageLocked(); err != nil {
		return &storageFault{err}
	}
	return w.refreshFileLocked(name, opts)
}

func (w *Watcher) refreshFileLocked(name string, opts indexOptions) error {
	if err := w.store.CheckName(name); err != nil {
		return err
	}
	if err := fileops.CheckSpelling(w.dir, filepath.Join(w.dir, filepath.FromSlash(name))); err != nil {
		return err
	}
	if fileops.BusyLocked(w.dir, filepath.Join(w.dir, filepath.FromSlash(name))) {
		return fileops.ErrBusy
	}
	if err := w.recoverReplicaLocked(name); err != nil {
		return err
	}
	if opts.trustStat && w.unchangedByStat(name) {
		return nil
	}
	ev, err := w.inspectFile(name, opts)
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

func (w *Watcher) inspectFile(name string, opts indexOptions) (*FileEvent, error) {
	if isOutsideSyncDir(name) || w.shouldIgnore(name) {
		return nil, nil
	}
	precomputed := opts.published[name]
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
			return nil, w.clearIntegrity(name)
		}
		if opts.deferDeletion != nil {
			// Absence is evidence here, not yet a decision: the caller weighs
			// it together with every other name it found missing.
			opts.deferDeletion(name)
			return nil, nil
		}
		if err := w.clearIntegrity(name); err != nil {
			return nil, err
		}
		// The root was verified above; absence is a local deletion, not loss of a mount.
		CleanEmptyParentsLocked(fullPath, w.dir, w.ignorePatterns, w.logger)
		return &FileEvent{Name: name, ModTime: existing.ModTime, Deleted: true}, nil
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
		return nil, w.readFailed(name, existing, err)
	}
	trusted, err := w.store.HasLocalIntent(name)
	if err != nil {
		return nil, err
	}
	key, err := fileops.GenerationKey(fullPath, info)
	if err != nil {
		return nil, err
	}
	if !trusted && w.store.IsDamagedObject(key) && (existing == nil || existing.Hash != hash) {
		if existing == nil {
			existing = &store.FileMeta{Name: name}
		}
		return nil, w.quarantine(existing)
	}
	sameStamp := existing != nil && fileops.SameModTime(w.dir, info.ModTime(), existing.ModTime)
	if existing != nil && !existing.Deleted && existing.Hash != hash && sameStamp && existing.Size == info.Size() && !trusted {
		return nil, w.quarantine(existing)
	}
	if w.NeedsRepair(name) || w.store.IsDamagedObject(key) {
		// Do not drop durable quarantine based on merely visible replacement
		// bytes: recovery may otherwise trust a repair lost to a later crash.
		f, err := os.Open(fullPath)
		if err != nil {
			return nil, err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return nil, err
		}
		if err := fileops.SyncSurvivingParent(fullPath, w.dir); err != nil {
			return nil, err
		}
		if err := w.clearIntegrity(name, key); err != nil {
			return nil, err
		}
	}
	if existing != nil && !existing.Deleted && existing.Hash == hash && sameStamp && existing.Size == info.Size() {
		return nil, nil
	}
	if !trusted && existing != nil && !existing.Deleted && existing.Hash == hash && existing.Size == info.Size() {
		// Unchanged bytes with restored timestamps are not a new mutation. Keep
		// the recorded rank and durably restore mtime before exposing metadata.
		stamp := time.Unix(0, existing.ModTime)
		if err := os.Chtimes(fullPath, stamp, stamp); err != nil {
			return nil, err
		}
		f, err := os.Open(fullPath)
		if err != nil {
			return nil, err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return nil, err
		}
		w.restamped.Add(1)
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
	return snapshotFile(path)
}

// readFailed classifies a failure to read a regular file. An I/O error on an
// indexed file is a damaged copy like a checksum mismatch, and is handled the
// same way: the name is quarantined and repaired from a peer. Logging it and
// carrying on left the node serving whatever the disk returned next time. A
// file this node may not read, or has no version of to repair, is skipped.
func (w *Watcher) readFailed(name string, existing *store.FileMeta, err error) error {
	if errors.Is(err, syscall.EIO) {
		w.readErrors.Add(1)
		if existing != nil && !existing.Deleted {
			w.logger.Error("indexed file cannot be read; quarantined until a peer repairs it", "name", strconv.Quote(name), "error", err)
			return w.quarantineFor(existing, "local read error", err)
		}
		return fmt.Errorf("%w: %w", errUnreadable, err)
	}
	return err
}

// ScanStatus describes the last complete checksum scan, independent of peers.
// A failed or incomplete scan never opens readiness.
type ScanStatus struct {
	ScrubBytesPerSecond     int64  `json:"scrub_bytes_per_second"`
	DirectorySyncBestEffort bool   `json:"directory_sync_best_effort"`
	ReplicaInitialized      bool   `json:"replica_initialized"`
	Ready                   bool   `json:"ready"`
	LastScanAgoMS           int64  `json:"last_scan_ms_ago"`
	LastError               string `json:"last_error,omitempty"`
	// Quarantined counts names whose bytes changed without a write timestamp.
	// They are individually unavailable and awaiting repair from a peer.
	Quarantined int `json:"quarantined"`
	// LastScrubAgoMS is the age of the last completed verification cycle, or -1
	// when no cycle has finished yet. Readiness does not depend on it: the scrub
	// is a continuous background budget, not a deadline.
	LastScrubAgoMS int64 `json:"last_scrub_ms_ago"`
	// Skipped counts, by reason, what the last complete sweep found on disk
	// and left alone instead of failing: names whose spelling collides with
	// one already reserved (name_collision), names no node can store
	// (invalid_name), entries removed while it walked (vanished), and files or
	// directories this process may not read (unreadable). SkippedSamples names
	// a few of them; SkippedTotal counts every skip since start, event batches
	// included.
	Skipped        map[string]int   `json:"skipped,omitempty"`
	SkippedSamples []string         `json:"skipped_samples,omitempty"`
	SkippedTotal   map[string]int64 `json:"skipped_total,omitempty"`
	// HeldExternalDeletions counts indexed files missing from disk that are
	// not published as deletions because there are more of them than
	// sync.max_external_deletions allows. Nonzero is a fault: see LastError.
	HeldExternalDeletions int `json:"held_external_deletions"`
	// ReadErrors counts I/O errors reading indexed files since start. Each
	// quarantined the file until a peer repairs it.
	ReadErrors int64 `json:"read_errors_total"`
}

func (w *Watcher) Status() ScanStatus {
	held, heldError := w.heldDeletions()
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	status := ScanStatus{LastError: w.lastError, LastScanAgoMS: -1, LastScrubAgoMS: -1, Quarantined: w.store.QuarantineCount(), ScrubBytesPerSecond: w.scrubRate.Load(), DirectorySyncBestEffort: runtime.GOOS == "windows"}
	status.HeldExternalDeletions, status.ReadErrors = held, w.readErrors.Load()
	if status.LastError == "" {
		status.LastError = heldError
	}
	if len(w.lastSkipped) > 0 {
		status.Skipped = make(map[string]int, len(w.lastSkipped))
		for reason, count := range w.lastSkipped {
			status.Skipped[reason] = count
		}
		status.SkippedSamples = append([]string(nil), w.lastSkipSamples...)
	}
	if len(w.skipTotals) > 0 {
		status.SkippedTotal = make(map[string]int64, len(w.skipTotals))
		for reason, count := range w.skipTotals {
			status.SkippedTotal[reason] = count
		}
	}
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
		status.Ready = status.LastError == "" && time.Since(w.lastScan) < 2*w.scanInterval+time.Minute
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

// storageFence is the outcome of one full storage validation. Only a holder of
// the commit lock runs that validation, since it may recover interrupted
// replacements and bind the volume.
type storageFence struct {
	err error
	at  time.Time // when the validation began
}

// readFenceTTL is how long a read trusts the last full validation before it
// revalidates. Every commit validates on its way in, so on a node taking
// writes this is refreshed long before it expires.
const readFenceTTL = time.Second

var errStorageUnvalidated = errors.New("storage has not been validated yet")

// validateRead is the storage fence for a read. A GET must not queue behind a
// replica apply for something a commit re-establishes on every acquisition.
func (w *Watcher) validateRead() error {
	if w.fence.Load() == nil {
		// Nothing has validated this volume yet. That happens once, before any
		// gateway is serving, and a read must not skip recovery to get there.
		return w.CheckStorage()
	}
	return w.checkStorageWithoutWaiting(readFenceTTL)
}

// ProbeStorage reports what CheckStorage would, without waiting for the commit
// lock: a readiness probe must answer while a large replica is being applied.
func (w *Watcher) ProbeStorage() error { return w.checkStorageWithoutWaiting(0) }

// checkStorageWithoutWaiting accepts a successful validation younger than
// maxAge. Otherwise it revalidates if the commit lock is free, and if it is not
// it relies on the holder: every commit validated recovery state on its way in
// and a replacement journal written since is the holder's own, so only the
// volume binding can have changed — and that needs no lock to check.
func (w *Watcher) checkStorageWithoutWaiting(maxAge time.Duration) error {
	fence := w.fence.Load()
	if fence != nil && fence.err == nil && time.Since(fence.at) < maxAge {
		return nil
	}
	if unlock, ok := fileops.TryLock(w.dir); ok {
		defer unlock()
		return w.prepareStorageLocked()
	}
	if fence == nil {
		return errStorageUnvalidated
	}
	if fence.err != nil {
		return fence.err
	}
	return w.checkBoundVolume()
}

// checkBoundVolume repeats the read-only half of checkStorageLocked for a
// volume that is already bound.
func (w *Watcher) checkBoundVolume() error {
	id := w.storageID.Load()
	if id == nil {
		return errStorageUnvalidated
	}
	return w.checkVolume(*id)
}

// A persisted sentinel binds this metadata database to its data volume. A
// missing/remounted root after restart must fail closed, not delete the cluster.
//
// The first successful check also measures how finely the volume keeps
// modification times; see fileops.SameModTime.
func (w *Watcher) checkStorageLocked() error {
	err := w.checkBindingLocked()
	if err == nil && !w.mtimeMeasured {
		w.mtimeMeasured = true
		w.measureModTimeGranularityLocked()
	}
	return err
}

// measureModTimeGranularityLocked probes the volume with a scratch file in its
// private state directory. A probe that fails leaves comparisons exact, which
// costs a re-read now and then but never trusts a timestamp too far.
func (w *Watcher) measureModTimeGranularityLocked() {
	granularity, err := fileops.DetectModTimeGranularity(filepath.Join(w.dir, ".birak", "mtime-probe"))
	if err != nil {
		w.logger.Warn("could not measure how precisely the volume keeps modification times", "assumed", granularity, "error", err)
	}
	fileops.SetModTimeGranularity(w.dir, granularity)
	if granularity > time.Nanosecond {
		w.logger.Info("volume keeps coarse modification times; comparing them at its resolution", "granularity", granularity)
	}
}

func (w *Watcher) checkBindingLocked() error {
	if id := w.storageID.Load(); id != nil {
		return w.checkVolume(*id)
	}
	id, err := w.store.NodeValue("storage_id")
	if err != nil {
		return err
	}
	if id != "" {
		w.storageID.Store(&id)
		return w.checkVolume(id)
	}
	return w.bindVolumeLocked()
}

// storageMarker checks the layout every validation relies on and returns the
// sentinel's path.
func (w *Watcher) storageMarker() (string, error) {
	info, err := os.Stat(w.dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sync root is not a directory")
	}
	private := filepath.Join(w.dir, ".birak")
	if info, err := os.Lstat(private); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("storage state directory must be a real directory")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	marker := filepath.Join(private, "storage-id")
	if info, err := os.Lstat(marker); err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("invalid storage sentinel")
	}
	return marker, nil
}

// checkVolume confirms, changing nothing, that the sync root still holds the
// volume bound to id. It needs no lock.
func (w *Watcher) checkVolume(id string) error {
	marker, err := w.storageMarker()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return fmt.Errorf("data volume sentinel unavailable: %w", err)
	}
	if string(data) != id {
		return fmt.Errorf("data volume identity mismatch")
	}
	return nil
}

// bindVolumeLocked binds a database that has no volume yet: it adopts the
// volume's sentinel, or writes one. Caller holds Lock.
func (w *Watcher) bindVolumeLocked() error {
	marker, err := w.storageMarker()
	if err != nil {
		return err
	}
	private := filepath.Dir(marker)
	data, err := os.ReadFile(marker)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	id := string(data)
	if id != "" {
		// The volume was bound before, to a database this one is not: the
		// metadata volume was lost or replaced. Adopting the volume is safe —
		// every file on it is indexed again from scratch — but what lived only
		// in the old database is gone, and an operator has to hear that.
		w.logger.Warn("metadata database is new but the data volume was bound to an earlier one; starting a full re-index",
			"volume", id,
			"lost", "tombstones of files deleted before the loss and the queued repairs of the old database",
			"consequence", "deletions the old database had not replicated yet never reach peers, and peers re-read this node's changes from the start")
	}
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
	w.storageID.Store(&id)
	return nil
}

// periodicScan is a checksum scrub, not just an mtime/size comparison. Changes
// are published one by one using fresh snapshots; memory holds only names.
// Any traversal error suppresses the entire deletion pass.
func (w *Watcher) periodicScan(ctx context.Context) error { return w.sweep(ctx, nil) }

// sweep is periodicScan for a processing loop with a backlog: names for which
// queued reports true are left to it, since it indexes them next anyway.
//
// A name that belongs to one file rather than to the node — see unindexable —
// and an entry that vanished mid-walk are counted and skipped. Anything else
// leaves the sweep incomplete, which keeps the node unready.
//
// Indexed names missing from disk are not published as they are found. They
// are collected over the whole pass and decided together once it is complete;
// see breaker.go.
func (w *Watcher) sweep(ctx context.Context, queued func(string) bool) (result error) {
	// complete distinguishes "the scan could not finish" from "the scan
	// finished and found damaged files". Only the first is a storage fault.
	complete := false
	// held is a complete scan that found more files missing than it may
	// publish as deleted. That is a fault, reported by the breaker.
	held := false
	var skips skipTally
	progress := w.newSweepProgress()
	defer func() {
		if !complete {
			if result != nil {
				w.setError(result)
				w.logger.Error("scan incomplete", "error", result)
			}
			return
		}
		w.statusMu.Lock()
		first := w.lastScan.IsZero()
		w.lastScan = time.Now()
		w.lastError = ""
		w.lastSkipped, w.lastSkipSamples = skips.counts, skips.samples
		for reason, count := range skips.counts {
			w.skipTotals[reason] += int64(count)
		}
		w.statusMu.Unlock()
		if n := skips.total(); n > 0 {
			reasons := make([]string, 0, len(skips.counts))
			for reason, count := range skips.counts {
				reasons = append(reasons, fmt.Sprintf("%s=%d", reason, count))
			}
			sort.Strings(reasons)
			w.logger.Warn("scan left files it cannot index alone",
				"skipped", n, "reasons", strings.Join(reasons, ","), "examples", skips.samples)
		}
		progress.finish()
		if first {
			w.warnAboutRestamping()
		}
		// Like any scan that could not be trusted, a held group keeps a
		// starting node's replication from starting.
		if !held {
			w.readyOnce.Do(func() { close(w.ready) })
		}
	}()
	if err := w.CheckStorage(); err != nil {
		return err
	}
	deletions := w.sweepDeletions()
	var deferDeletion func(string)
	if deletions != nil {
		deferDeletion = deletions.add
	}
	var scanErr, integrityErr error
	inspectError := func(name string, err error) {
		if err == nil {
			return
		}
		switch {
		case errors.Is(err, ErrIntegrity):
			integrityErr = errors.Join(integrityErr, err)
		case errors.Is(err, fileops.ErrBusy):
			w.logger.Debug("scan skipped a file being written", "name", name)
		default:
			if reason := unindexable(err); reason != "" {
				w.logger.Debug("scan skipped a file it cannot index", "name", strconv.Quote(name), "reason", reason, "error", err)
				skips.add(reason, name)
				return
			}
			scanErr = errors.Join(scanErr, err)
		}
	}
	// Fetch metadata for bounded batches instead of one SQL query per inode.
	// The deletion pass below already owns index pages and reuses those too.
	batch := make([]string, 0, 256)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		known, err := w.store.GetFilesBatch(batch)
		if err != nil {
			return err
		}
		for _, name := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			scanned, err := w.sweepKnown(name, known[name], deferDeletion)
			if scanned {
				progress.indexed++
			}
			inspectError(name, err)
			progress.tick()
		}
		batch = batch[:0]
		return nil
	}
	var rootDevice uint64
	haveRootDevice := false
	err := filepath.WalkDir(w.dir, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path != w.dir {
				rel, _ := filepath.Rel(w.dir, path)
				switch {
				case vanished(walkErr):
					// Removed between being listed and being read. The deletion
					// pass below decides about anything indexed beneath it.
					skips.add(SkipVanished, filepath.ToSlash(rel))
					return nil
				case unreadableEntry(walkErr):
					// This process may not list it. What is indexed beneath it
					// is checked name by name in the deletion pass, where a name
					// that cannot even be stat'ed is skipped as well — never
					// taken for deleted.
					skips.add(SkipUnreadable, filepath.ToSlash(rel))
					return nil
				}
			}
			scanErr = errors.Join(scanErr, walkErr)
			return nil
		}
		if path == w.dir {
			if info, err := d.Info(); err == nil {
				rootDevice, haveRootDevice = deviceOf(path, info)
			}
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
		progress.tick()
		if d.IsDir() {
			if !haveRootDevice {
				return nil
			}
			return checkSameFilesystem(path, name, d, rootDevice, &skips, &scanErr)
		}
		progress.seen++
		if queued != nil && queued(name) {
			return nil
		}
		batch = append(batch, name)
		if len(batch) == cap(batch) {
			return flush()
		}
		return nil
	})
	if err != nil || scanErr != nil {
		return errors.Join(err, scanErr)
	}
	if err := flush(); err != nil || scanErr != nil {
		return errors.Join(err, scanErr)
	}
	progress.phase = "deletion check"
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
			if err := ctx.Err(); err != nil {
				return err
			}
			after = meta.Name
			if w.shouldIgnore(meta.Name) || queued != nil && queued(meta.Name) {
				continue
			}
			progress.checked++
			progress.tick()
			// Names the walk already reconciled are answered by a single stat and
			// cost nothing more. Remembering which ones those were would mean
			// holding every name in memory at once — a gigabyte on a tree of ten
			// million files, allocated by a background sweep, which is the kind
			// of thing that takes a node down at exactly the wrong moment.
			//
			// A file may also have appeared since the walk. Both paths recheck
			// disk and store under the commit lock, so neither can publish a
			// stale deletion.
			_, err := w.sweepKnown(meta.Name, &meta, deferDeletion)
			switch {
			case err == nil, errors.Is(err, fileops.ErrBusy), errors.Is(err, ErrIntegrity):
			case unindexable(err) != "":
				skips.add(unindexable(err), meta.Name)
			default:
				return err
			}
		}
	}
	if err := w.CheckStorage(); err != nil {
		return err
	}
	// Every indexed name has been looked at, so what is missing now is the
	// whole of what is missing, including what the event path is waiting on.
	var heldErr error
	if deletions != nil {
		deletions.merge(w.takeEventDeletions())
		if err := w.settleDeletions(ctx, deletions, true); err != nil {
			if !errors.Is(err, errHeldDeletions) {
				return err
			}
			held, heldErr = true, err
		}
	} else {
		w.setHeldDeletions(0, "")
	}
	complete = true
	if integrityErr != nil {
		w.logger.Error("scan found damaged files awaiting repair", "error", integrityErr)
	}
	return errors.Join(integrityErr, heldErr)
}

// checkSameFilesystem refuses a directory on another filesystem than the root.
// A separate mount below the root can disappear on its own — an unmount, a
// pool that fails to import — and leave an empty directory behind, and every
// file it held would then look deleted. Only the root is guarded against that,
// so a second filesystem fails the scan, loudly, before anything below it is
// indexed. A failed scan also skips the deletion pass.
func checkSameFilesystem(path, name string, d fs.DirEntry, rootDevice uint64, skips *skipTally, scanErr *error) error {
	info, err := d.Info()
	switch {
	case err != nil && vanished(err):
		skips.add(SkipVanished, name)
		return fs.SkipDir
	case err != nil && unreadableEntry(err):
		skips.add(SkipUnreadable, name)
		return fs.SkipDir
	case err != nil:
		*scanErr = errors.Join(*scanErr, err)
		return fs.SkipDir
	}
	if device, ok := deviceOf(path, info); ok && device != rootDevice {
		*scanErr = errors.Join(*scanErr, fmt.Errorf("sync_dir contains another filesystem at %q; Birak requires a single filesystem under sync_dir", name))
		return fs.SkipDir
	}
	return nil
}

// sweepProgress reports a sweep that runs long, so the first index of a large
// tree is visibly working rather than silent for an hour.
type sweepProgress struct {
	w                      *Watcher
	phase                  string
	started, last          time.Time
	seen, indexed, checked int
	reported               bool
}

func (w *Watcher) newSweepProgress() *sweepProgress {
	now := time.Now()
	return &sweepProgress{w: w, phase: "walk", started: now, last: now}
}

func (p *sweepProgress) tick() {
	now := time.Now()
	if now.Sub(p.last) < p.w.progressInterval {
		return
	}
	p.last, p.reported = now, true
	p.w.logger.Info("scan in progress", "phase", p.phase, "files_seen", p.seen, "indexed", p.indexed,
		"checked", p.checked, "elapsed", now.Sub(p.started).Round(time.Millisecond))
}

// finish closes a sweep that reported progress with its totals.
func (p *sweepProgress) finish() {
	if !p.reported {
		return
	}
	p.w.logger.Info("scan finished", "files_seen", p.seen, "indexed", p.indexed, "checked", p.checked,
		"elapsed", time.Since(p.started).Round(time.Millisecond))
}

// restampThreshold is where "somebody touched a few files" becomes "somebody
// restored a backup without preserving timestamps". Low enough to catch a real
// restore, high enough that ordinary editing never trips it.
const restampThreshold = 8

// warnAboutRestamping says the one thing an operator needs to hear after a bad
// restore, while there is still time to act. Conflict resolution ranks a file
// by its logical clock, which starts from its timestamp: a restored tree with
// fresh timestamps outranks the cluster's newer state and will overwrite it —
// silently, and on every node, including undoing deletions.
func (w *Watcher) warnAboutRestamping() {
	restamped := w.restamped.Load()
	if restamped < restampThreshold {
		return
	}
	w.logger.Warn("restored modification times for unchanged indexed files", "files", restamped)
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
		rel, err := filepath.Rel(absRoot, absDir)
		// Emptiness first: it ends the climb at the first directory that still
		// holds anything, at the cost of reading one entry. Checking for writers
		// first walked that directory instead — every remaining file of it,
		// for every file a tree removal deleted.
		if err != nil || !emptyDirectory(r, rel) || mountPoint(absDir) {
			break
		}
		if fileops.BusyTreeLocked(rootDir, dir) {
			break
		}
		if !removeEmptyDirectory(r, rel) {
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

// emptyDirectory reports a directory with no entries at all. It is only a fast
// way to stop early; removeEmptyDirectory still relies on the OS to refuse a
// directory that gained an entry since.
func emptyDirectory(root *os.Root, dir string) bool {
	f, err := root.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	entries, err := f.ReadDir(1)
	return len(entries) == 0 && errors.Is(err, io.EOF)
}

// mountPoint reports a directory on another filesystem than its parent: the
// root of something mounted there. Emptied of Birak's files it is still not
// Birak's to remove — taking it away would leave whatever mounted it without
// its mount point, and the next mount failing or landing elsewhere.
func mountPoint(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil {
		return false
	}
	parentInfo, err := os.Lstat(filepath.Dir(dir))
	if err != nil {
		return false
	}
	device, ok := deviceOf(dir, info)
	parent, parentOK := deviceOf(filepath.Dir(dir), parentInfo)
	return ok && parentOK && device != parent
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
