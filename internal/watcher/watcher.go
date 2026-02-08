package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	// recentlySynced tracks files written by the syncer that should be
	// ignored by the watcher to prevent sync loops. This serves as a
	// fast-path optimisation — the store-based dedup in inspectFile is
	// the authoritative check.
	recentlySynced   map[string]string // relPath -> expected hash
	recentlySyncedMu sync.Mutex

	// onChange is called for each debounced file event batch.
	onChange func([]FileEvent)

	// ready is closed after the initial scan completes.
	// Other components (e.g. syncer) should wait on this before starting.
	ready chan struct{}
}

// New creates a new Watcher.
func New(dir string, s *store.Store, logger *slog.Logger, debounceWindow, scanInterval time.Duration, ignorePatterns []string, onChange func([]FileEvent)) *Watcher {
	// Max debounce window: 10x the debounce window, at least 2 seconds.
	maxDebounce := debounceWindow * 10
	if maxDebounce < 2*time.Second {
		maxDebounce = 2 * time.Second
	}

	return &Watcher{
		dir:               dir,
		store:             s,
		logger:            logger,
		debounceWindow:    debounceWindow,
		maxDebounceWindow: maxDebounce,
		scanInterval:      scanInterval,
		ignorePatterns:    ignorePatterns,
		recentlySynced:    make(map[string]string),
		onChange:           onChange,
		ready:             make(chan struct{}),
	}
}

// Ready returns a channel that is closed after the initial scan completes.
func (w *Watcher) Ready() <-chan struct{} {
	return w.ready
}

// MarkSynced marks a file as recently synced so the watcher ignores the
// next fsnotify event for it. The entry expires after 5 seconds.
func (w *Watcher) MarkSynced(name, hash string) {
	w.recentlySyncedMu.Lock()
	w.recentlySynced[name] = hash
	w.recentlySyncedMu.Unlock()

	// Auto-expire after 5 seconds.
	go func() {
		time.Sleep(5 * time.Second)
		w.recentlySyncedMu.Lock()
		if w.recentlySynced[name] == hash {
			delete(w.recentlySynced, name)
		}
		w.recentlySyncedMu.Unlock()
	}()
}

// isSynced checks if a file event should be ignored (was recently synced).
func (w *Watcher) isSynced(name, hash string) bool {
	w.recentlySyncedMu.Lock()
	defer w.recentlySyncedMu.Unlock()
	expected, ok := w.recentlySynced[name]
	if ok && expected == hash {
		delete(w.recentlySynced, name)
		return true
	}
	return false
}

// shouldIgnore checks if a file path matches any of the configured ignore patterns.
// It matches each path segment's basename against the patterns.
func (w *Watcher) shouldIgnore(relPath string) bool {
	// Check the basename of the file.
	base := filepath.Base(relPath)
	for _, pattern := range w.ignorePatterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	// Also check each parent directory segment.
	dir := filepath.Dir(relPath)
	for dir != "." && dir != "" {
		seg := filepath.Base(dir)
		for _, pattern := range w.ignorePatterns {
			if matched, _ := filepath.Match(pattern, seg); matched {
				return true
			}
		}
		dir = filepath.Dir(dir)
	}
	return false
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
	if err := w.addDirsRecursive(fsw, w.dir); err != nil {
		return fmt.Errorf("add dirs to watcher: %w", err)
	}

	w.logger.Info("watcher started", "dir", w.dir)

	// Run initial scan to pick up any changes that happened while daemon was down.
	w.periodicScan()

	// Signal that the initial scan is complete. Other components (syncer) wait on this
	// to avoid syncing before the local state is known.
	close(w.ready)

	// Debounce timer and pending events.
	pending := make(map[string]struct{})
	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	// maxDebounceTimer is a hard deadline: if events keep arriving and the
	// debounce timer keeps resetting, we force a flush after maxDebounceWindow
	// to prevent starvation (and MarkSynced expiry under sustained load).
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
		w.processBatch(names)
	}

	scanTicker := time.NewTicker(w.scanInterval)
	defer scanTicker.Stop()

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
					if err := w.addDirsRecursive(fsw, event.Name); err != nil {
						w.logger.Error("failed to watch new directory", "path", relPath, "error", err)
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
			w.logger.Error("fsnotify error", "error", err)

		case <-debounceCh:
			flushPending()

		case <-maxDebounceCh:
			// Hard deadline reached — flush regardless of ongoing events.
			w.logger.Debug("max debounce deadline reached, flushing")
			flushPending()

		case <-scanTicker.C:
			w.periodicScan()
		}
	}
}

// addDirsRecursive adds a directory and all its subdirectories to the fsnotify watcher.
func (w *Watcher) addDirsRecursive(fsw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
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
		if err := fsw.Add(path); err != nil {
			w.logger.Warn("failed to add dir to watcher", "path", path, "error", err)
		}
		return nil
	})
}

// processBatch handles a batch of relative file paths detected by fsnotify.
func (w *Watcher) processBatch(names []string) {
	var events []FileEvent

	for _, name := range names {
		ev, err := w.inspectFile(name)
		if err != nil {
			w.logger.Error("inspect file failed", "name", name, "error", err)
			continue
		}
		if ev == nil {
			continue // skipped (synced file or directory)
		}
		events = append(events, *ev)
	}

	if len(events) > 0 {
		w.onChange(events)
	}
}

// inspectFile stats and hashes a file, returning a FileEvent.
// name is a relative path (e.g. "subdir/file.txt").
// Returns nil if the file should be skipped (recently synced or a directory).
func (w *Watcher) inspectFile(name string) (*FileEvent, error) {
	// Safety check: reject paths that escape the sync directory.
	if isOutsideSyncDir(name) {
		return nil, nil
	}

	fullPath := filepath.Join(w.dir, filepath.FromSlash(name))
	info, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		// File was deleted — look up the last known ModTime from the store
		// so the deletion timestamp reflects the file's real age rather than
		// the wall-clock time of detection. Using time.Now() would make
		// every deletion "newer" than the original file on other nodes,
		// causing false conflict wins.
		existing, _ := w.store.GetFile(name)
		if existing != nil && existing.Deleted {
			// Already marked as deleted in store — nothing to do.
			w.logger.Debug("skipping deletion, already in store", "name", name)
			return nil, nil
		}

		// Check fast-path MarkSynced (syncer just deleted this file).
		if w.isSynced(name, "") {
			w.logger.Debug("skipping synced deletion", "name", name)
			return nil, nil
		}

		w.logger.Info("file deleted detected", "name", name)

		// Clean up empty parent directories on the source node.
		CleanEmptyParents(fullPath, w.dir, w.ignorePatterns, w.logger)

		delModTime := time.Now().UnixNano()
		if existing != nil {
			// Use the file's last known ModTime + 1ns. This ensures the
			// deletion beats the exact version it's deleting, but does NOT
			// beat legitimately newer versions of the same file on other
			// nodes (unlike time.Now() which always wins).
			delModTime = existing.ModTime + 1
		}

		return &FileEvent{
			Name:    name,
			ModTime: delModTime,
			Size:    0,
			Hash:    "",
			Deleted: true,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", fullPath, err)
	}

	// Skip directories.
	if info.IsDir() {
		return nil, nil
	}

	hash, err := hashFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("hash %s: %w", fullPath, err)
	}

	// Fast-path: check in-memory MarkSynced (avoids unnecessary store writes).
	if w.isSynced(name, hash) {
		w.logger.Debug("skipping synced file", "name", name)
		return nil, nil
	}

	// Authoritative dedup: if the store already has this exact hash for this
	// file, the change was already recorded (e.g. by the syncer). This
	// replaces the fragile time-based MarkSynced as the primary guard
	// against sync loops.
	existing, _ := w.store.GetFile(name)
	if existing != nil && !existing.Deleted && existing.Hash == hash {
		w.logger.Debug("skipping file, store already has same hash", "name", name)
		return nil, nil
	}

	w.logger.Info("file change detected", "name", name, "size", info.Size(), "hash", hash[:12])
	return &FileEvent{
		Name:    name,
		ModTime: info.ModTime().UnixNano(),
		Size:    info.Size(),
		Hash:    hash,
		Deleted: false,
	}, nil
}

// periodicScan performs a recursive directory scan to detect changes missed by fsnotify.
func (w *Watcher) periodicScan() {
	w.logger.Debug("periodic scan started")

	// IMPORTANT: snapshot the store BEFORE walking the disk.
	// This prevents a race where the syncer adds a file to the store while
	// the walk is in progress — such a file would appear in a post-walk
	// AllFiles() result but not in the onDisk map, causing a false deletion.
	known, err := w.store.AllFiles()
	if err != nil {
		w.logger.Error("periodic scan: get all files failed", "error", err)
		return
	}

	// Build set of files on disk by walking the directory tree.
	onDisk := make(map[string]struct{})
	var events []FileEvent

	err = filepath.WalkDir(w.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			w.logger.Error("periodic scan: walk error", "path", path, "error", err)
			return nil // continue walking
		}
		if d.IsDir() {
			// Check if this directory should be ignored.
			if path != w.dir {
				relPath, relErr := filepath.Rel(w.dir, path)
				if relErr == nil {
					relPath = filepath.ToSlash(relPath)
					if w.shouldIgnore(relPath) {
						return fs.SkipDir
					}
				}
			}
			return nil
		}

		relPath, relErr := filepath.Rel(w.dir, path)
		if relErr != nil {
			return nil
		}
		// Normalize to forward slashes.
		name := filepath.ToSlash(relPath)

		// Skip ignored files.
		if w.shouldIgnore(name) {
			return nil
		}

		onDisk[name] = struct{}{}

		info, infoErr := d.Info()
		if infoErr != nil {
			w.logger.Error("periodic scan: stat failed", "name", name, "error", infoErr)
			return nil
		}

		// Check against store (use live query, not snapshot, for per-file checks —
		// we want the freshest data to avoid unnecessary hash computation).
		existing, getErr := w.store.GetFile(name)
		if getErr != nil {
			w.logger.Error("periodic scan: get file from store failed", "name", name, "error", getErr)
			return nil
		}

		// Quick check: if mod_time and size match, skip expensive hash.
		if existing != nil && !existing.Deleted &&
			existing.ModTime == info.ModTime().UnixNano() &&
			existing.Size == info.Size() {
			return nil
		}

		hash, hashErr := hashFile(path)
		if hashErr != nil {
			w.logger.Error("periodic scan: hash failed", "name", name, "error", hashErr)
			return nil
		}

		// If hash matches, no real change.
		if existing != nil && !existing.Deleted && existing.Hash == hash {
			return nil
		}

		// Check synced (fast-path).
		if w.isSynced(name, hash) {
			return nil
		}

		w.logger.Info("periodic scan: change detected", "name", name, "size", info.Size())
		events = append(events, FileEvent{
			Name:    name,
			ModTime: info.ModTime().UnixNano(),
			Size:    info.Size(),
			Hash:    hash,
			Deleted: false,
		})
		return nil
	})
	if err != nil {
		w.logger.Error("periodic scan: walk failed", "error", err)
	}

	// Detect deletions: files in the pre-walk store snapshot but not on disk.
	// Using the snapshot taken BEFORE the walk ensures that files added by
	// the syncer during the walk cannot trigger false deletions.
	for name, meta := range known {
		if _, ok := onDisk[name]; !ok {
			// Double-check the live store — the syncer may have already marked
			// this file as deleted while we were walking.
			liveMeta, _ := w.store.GetFile(name)
			if liveMeta == nil || liveMeta.Deleted {
				continue
			}

			// Check synced (fast-path for syncer-originated deletions).
			if w.isSynced(name, "") {
				continue
			}

			w.logger.Info("periodic scan: deletion detected", "name", name)

			// Clean up empty parent directories on the source node.
			fullPath := filepath.Join(w.dir, filepath.FromSlash(name))
			CleanEmptyParents(fullPath, w.dir, w.ignorePatterns, w.logger)

			// Use the file's last known ModTime + 1ns. This ensures the
			// deletion beats the exact version being deleted, but does NOT
			// beat legitimately newer versions on other nodes (unlike
			// time.Now() which always wins).
			events = append(events, FileEvent{
				Name:    name,
				ModTime: meta.ModTime + 1,
				Deleted: true,
			})
		}
	}

	if len(events) > 0 {
		w.logger.Info("periodic scan completed", "changes", len(events))
		w.onChange(events)
	} else {
		w.logger.Debug("periodic scan completed, no changes")
	}
}

// hashFile computes the SHA256 hex digest of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CleanEmptyParents removes parent directories up to (but not including) rootDir,
// if they are empty or contain only ignored files. Ignored files are removed
// before attempting to remove the directory. This is used both by the watcher
// (source node) and the syncer (remote nodes) after file deletions.
func CleanEmptyParents(filePath, rootDir string, ignorePatterns []string, logger *slog.Logger) {
	absRoot, _ := filepath.Abs(rootDir)
	dir := filepath.Dir(filePath)
	for {
		absDir, _ := filepath.Abs(dir)
		if absDir == absRoot || !strings.HasPrefix(absDir, absRoot) {
			break
		}
		if !removeIfOnlyIgnored(dir, ignorePatterns, logger) {
			break
		}
		logger.Debug("removed empty directory", "path", dir)
		dir = filepath.Dir(dir)
	}
}

// removeIfOnlyIgnored removes a directory if it is empty or contains only ignored files.
// Returns true if the directory was successfully removed.
func removeIfOnlyIgnored(dir string, ignorePatterns []string, logger *slog.Logger) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	// Check if all remaining entries are ignored files (not directories).
	for _, entry := range entries {
		if entry.IsDir() {
			return false // subdirectory present — don't remove
		}
		if !ShouldIgnore(entry.Name(), ignorePatterns) {
			return false // non-ignored file present — don't remove
		}
	}

	// Remove ignored files first, then the directory.
	for _, entry := range entries {
		fp := filepath.Join(dir, entry.Name())
		if err := os.Remove(fp); err != nil {
			logger.Warn("failed to remove ignored file during cleanup", "path", fp, "error", err)
			return false
		}
		logger.Debug("removed ignored file during cleanup", "path", fp)
	}

	return os.Remove(dir) == nil
}

// isOutsideSyncDir returns true if a relative path escapes the sync directory
// (e.g. starts with "../"). Such paths can arrive from fsnotify on some
// platforms and must be rejected to avoid watching meta/database files.
func isOutsideSyncDir(relPath string) bool {
	return strings.HasPrefix(relPath, "../") || relPath == ".."
}

// ShouldIgnore is exported for use by other packages (server, syncer).
func ShouldIgnore(relPath string, patterns []string) bool {
	// Check the basename of the file.
	base := filepath.Base(relPath)
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	// Also check each parent directory segment.
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	for _, part := range parts[:len(parts)-1] {
		for _, pattern := range patterns {
			if matched, _ := filepath.Match(pattern, part); matched {
				return true
			}
		}
	}
	return false
}
