package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// stallTimeout is the maximum time to wait for any data from a peer during a
// file download. If no bytes arrive within this window, the transfer is
// considered stalled and aborted. This replaces the old fixed 30s client
// timeout that was too short for large files.
const stallTimeout = 60 * time.Second

// Syncer polls peers for changes and downloads newer files.
type Syncer struct {
	store          *store.Store
	watcher        *watcher.Watcher
	syncDir        string
	nodeID         string
	peers          []string
	ignorePatterns []string
	logger         *slog.Logger

	pollInterval           time.Duration
	batchLimit             int
	maxConcurrentDownloads int

	// client is used for metadata requests (/changes, /status).
	// File downloads use a separate client without a global timeout.
	client *http.Client

	// downloadClient has no overall timeout — large file transfers are
	// bounded by context cancellation and per-read stall detection instead.
	downloadClient *http.Client
}

// New creates a new Syncer.
func New(
	s *store.Store,
	w *watcher.Watcher,
	syncDir, nodeID string,
	peers []string,
	ignorePatterns []string,
	logger *slog.Logger,
	pollInterval time.Duration,
	batchLimit int,
	maxConcurrentDownloads int,
) *Syncer {
	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Syncer{
		store:                  s,
		watcher:                w,
		syncDir:                syncDir,
		nodeID:                 nodeID,
		peers:                  peers,
		ignorePatterns:         ignorePatterns,
		logger:                 logger,
		pollInterval:           pollInterval,
		batchLimit:             batchLimit,
		maxConcurrentDownloads: maxConcurrentDownloads,
		// Metadata requests: 30s is plenty.
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		// File downloads: no global timeout — we rely on context
		// cancellation and stall detection (stallTimeout) instead.
		downloadClient: &http.Client{
			Transport: transport,
		},
	}
}

// Run starts polling all peers. It blocks until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	if len(s.peers) == 0 {
		s.logger.Info("no peers configured, syncer idle")
		<-ctx.Done()
		return
	}

	// Wait for the watcher's initial scan to complete before polling peers.
	// This ensures our local store knows about all pre-existing files,
	// preventing us from downloading files we already have with a newer version.
	s.logger.Info("syncer waiting for initial scan to complete")
	select {
	case <-s.watcher.Ready():
	case <-ctx.Done():
		return
	}

	s.logger.Info("syncer started", "peers", s.peers, "poll_interval", s.pollInterval)

	var wg sync.WaitGroup
	for _, peer := range s.peers {
		wg.Add(1)
		go func(peerURL string) {
			defer wg.Done()
			s.pollPeer(ctx, peerURL)
		}(peer)
	}
	wg.Wait()
	s.logger.Info("syncer stopped")
}

// pollPeer continuously polls a single peer for changes.
func (s *Syncer) pollPeer(ctx context.Context, peerURL string) {
	backoff := s.pollInterval
	maxBackoff := 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := s.syncOnce(ctx, peerURL)
		if err != nil {
			s.logger.Warn("sync with peer failed", "peer", peerURL, "error", err)
			// Exponential backoff on error.
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Reset backoff on success.
		backoff = s.pollInterval

		if n > 0 {
			// Got changes — immediately poll again to drain.
			s.logger.Info("synced changes from peer", "peer", peerURL, "count", n)
			continue
		}

		// No changes — wait before next poll.
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.pollInterval):
		}
	}
}

// syncOnce fetches one batch of changes from a peer and applies them.
// Returns the number of changes processed.
func (s *Syncer) syncOnce(ctx context.Context, peerURL string) (int, error) {
	cursor, err := s.store.GetCursor(peerURL)
	if err != nil {
		return 0, fmt.Errorf("get cursor for %s: %w", peerURL, err)
	}

	reqURL := fmt.Sprintf("%s/changes?since=%d&limit=%d", peerURL, cursor, s.batchLimit)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch changes from %s: %w", peerURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("peer %s returned status %d", peerURL, resp.StatusCode)
	}

	var changes []store.FileMeta
	if err := json.NewDecoder(resp.Body).Decode(&changes); err != nil {
		return 0, fmt.Errorf("decode changes from %s: %w", peerURL, err)
	}

	if len(changes) == 0 {
		return 0, nil
	}

	// Process changes sequentially by version. The cursor is only advanced
	// past items that are successfully handled (downloaded, skipped by hash
	// match, or intentionally skipped). If a download fails, we stop and
	// keep the cursor so the failed item is retried on the next poll.
	lastSuccessVer := cursor
	processed := 0
	var firstErr error

	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			break
		}

		// Skip paths escaping the sync directory (e.g. "../meta/birak.db-wal").
		// These can appear in a peer's store if the watcher previously tracked
		// files outside the sync dir (e.g. the SQLite WAL file). We must skip
		// them to prevent the cursor from getting stuck (the HTTP server
		// rejects such paths with 400).
		if strings.HasPrefix(change.Name, "../") || change.Name == ".." {
			s.logger.Debug("skipping file outside sync dir from peer", "name", change.Name)
			lastSuccessVer = change.Version
			processed++
			continue
		}

		// Skip ignored files — intentional skip, safe to advance cursor.
		if watcher.ShouldIgnore(change.Name, s.ignorePatterns) {
			s.logger.Debug("skipping ignored file from peer", "name", change.Name)
			lastSuccessVer = change.Version
			processed++
			continue
		}

		// Check if we need this change.
		local, err := s.store.GetFile(change.Name)
		if err != nil {
			s.logger.Error("get local file failed", "name", change.Name, "error", err)
			// DB error — stop processing, retry from here.
			firstErr = err
			break
		}

		if local != nil && local.Hash == change.Hash {
			// Same content, skip.
			s.logger.Debug("skipping unchanged file", "name", change.Name, "hash", change.Hash[:min(12, len(change.Hash))])
			lastSuccessVer = change.Version
			processed++
			continue
		}

		if change.Deleted {
			// Remote deleted this file.
			if local == nil || local.Deleted {
				s.logger.Debug("skipping deletion, already handled", "name", change.Name)
				lastSuccessVer = change.Version
				processed++
				continue
			}
			// Delete wins if its timestamp is newer.
			if change.ModTime > local.ModTime {
				s.logger.Info("applying remote deletion", "name", change.Name, "peer", peerURL)
				if err := s.applyDeletion(change); err != nil {
					s.logger.Error("apply deletion failed", "name", change.Name, "error", err)
					firstErr = err
					break
				}
			} else {
				s.logger.Debug("skipping deletion, local is newer", "name", change.Name)
			}
			lastSuccessVer = change.Version
			processed++
			continue
		}

		// If local file was deleted with a newer timestamp, our deletion takes precedence.
		if local != nil && local.Deleted && local.ModTime >= change.ModTime {
			s.logger.Debug("local deletion is newer, skipping remote file", "name", change.Name)
			lastSuccessVer = change.Version
			processed++
			continue
		}

		if local != nil && !local.Deleted && change.ModTime <= local.ModTime {
			// Local is same age or newer — skip.
			s.logger.Debug("local file is newer, skipping", "name", change.Name,
				"local_mod", local.ModTime, "remote_mod", change.ModTime,
				"local_hash", local.Hash[:min(12, len(local.Hash))],
				"remote_hash", change.Hash[:min(12, len(change.Hash))])
			lastSuccessVer = change.Version
			processed++
			continue
		}

		// Need to download this file.
		if err := s.downloadAndApply(ctx, peerURL, change); err != nil {
			s.logger.Error("download failed", "name", change.Name, "peer", peerURL, "error", err)
			firstErr = err
			// Stop processing — cursor stays before this item so it is retried.
			break
		}
		lastSuccessVer = change.Version
		processed++
	}

	// Advance cursor only up to the last successfully processed version.
	if lastSuccessVer > cursor {
		if err := s.store.SetCursor(peerURL, lastSuccessVer); err != nil {
			return 0, fmt.Errorf("update cursor for %s: %w", peerURL, err)
		}
		s.logger.Debug("cursor updated", "peer", peerURL, "version", lastSuccessVer)
	}

	if firstErr != nil {
		return processed, fmt.Errorf("sync from %s partially failed: %w", peerURL, firstErr)
	}

	return processed, nil
}

// downloadAndApply downloads a file from a peer and writes it locally.
func (s *Syncer) downloadAndApply(ctx context.Context, peerURL string, meta store.FileMeta) error {
	// URL-encode each path segment to handle special characters and subdirectories.
	encodedName := encodePathSegments(meta.Name)
	reqURL := fmt.Sprintf("%s/files/%s", peerURL, encodedName)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}

	// Use the download client (no global timeout) for file transfers.
	resp, err := s.downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", meta.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// File was deleted between change notification and download.
		// Treat as deletion.
		s.logger.Warn("file not found on peer, treating as deleted", "name", meta.Name, "peer", peerURL)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", meta.Name, resp.StatusCode)
	}

	destPath := filepath.Join(s.syncDir, filepath.FromSlash(meta.Name))

	// Ensure parent directory exists.
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create parent dir for %s: %w", meta.Name, err)
	}

	// Write to temp file with a unique name (random suffix) to prevent
	// collisions when multiple goroutines download the same file from
	// different peers simultaneously.
	baseName := filepath.Base(meta.Name)
	tmpFile, err := os.CreateTemp(destDir, ".birak-tmp-"+baseName+"-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Hash while writing, with stall detection.
	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)

	if _, err := copyWithStallDetect(ctx, writer, resp.Body, stallTimeout); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write file %s: %w", meta.Name, err)
	}
	tmpFile.Close()

	// Verify hash.
	gotHash := hex.EncodeToString(hasher.Sum(nil))
	if gotHash != meta.Hash {
		os.Remove(tmpPath)
		return fmt.Errorf("hash mismatch for %s: expected %s, got %s", meta.Name, meta.Hash[:12], gotHash[:12])
	}

	// Set mod time to match the source.
	modTime := time.Unix(0, meta.ModTime)
	if err := os.Chtimes(tmpPath, modTime, modTime); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("set modtime for %s: %w", meta.Name, err)
	}

	// Mark as synced so the watcher's fast-path skips the fsnotify event
	// without needing to hash the file. Even if MarkSynced expires before the
	// watcher processes the event, the store-based dedup in inspectFile will
	// catch it (PutFile runs right after Rename, microseconds later).
	s.watcher.MarkSynced(meta.Name, meta.Hash)

	// Atomic rename — file appears on disk.
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename %s: %w", meta.Name, err)
	}

	// Update local store AFTER rename. This ordering is critical: if PutFile
	// were called before Rename and Rename failed, the store would record a
	// file that doesn't exist on disk. On retry the syncer would see
	// local.Hash == change.Hash and SKIP the file, leaving it permanently
	// missing from disk (explaining size discrepancies between nodes).
	// With Rename-then-PutFile, a PutFile failure is self-healing: the file
	// is on disk, the watcher or periodic scan will detect it and create the
	// store entry.
	if _, err := s.store.PutFile(meta.Name, meta.ModTime, meta.Size, meta.Hash, false); err != nil {
		return fmt.Errorf("update store for %s: %w", meta.Name, err)
	}

	s.logger.Info("file synced", "name", meta.Name, "size", meta.Size, "hash", meta.Hash[:12], "peer", peerURL)
	return nil
}

// copyWithStallDetect copies from src to dst, aborting if no data arrives
// within the stall timeout or the context is cancelled. This replaces a
// global HTTP client timeout that was too short for large file transfers.
func copyWithStallDetect(ctx context.Context, dst io.Writer, src io.Reader, stall time.Duration) (int64, error) {
	buf := make([]byte, 256*1024) // 256 KB chunks
	var total int64

	for {
		// Check context before each read.
		if err := ctx.Err(); err != nil {
			return total, err
		}

		// Set a per-read deadline via a timer.
		timer := time.NewTimer(stall)
		readDone := make(chan struct{})
		var n int
		var readErr error

		go func() {
			n, readErr = src.Read(buf)
			close(readDone)
		}()

		select {
		case <-readDone:
			timer.Stop()
		case <-timer.C:
			return total, fmt.Errorf("transfer stalled: no data for %v", stall)
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		}

		if n > 0 {
			nw, wErr := dst.Write(buf[:n])
			total += int64(nw)
			if wErr != nil {
				return total, wErr
			}
			if nw != n {
				return total, io.ErrShortWrite
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
	}
}

// applyDeletion removes a file locally and marks it as deleted in the store.
func (s *Syncer) applyDeletion(meta store.FileMeta) error {
	destPath := filepath.Join(s.syncDir, filepath.FromSlash(meta.Name))

	// Mark as synced (fast-path optimisation for watcher).
	s.watcher.MarkSynced(meta.Name, "")

	// Remove file first (ignore if already gone).
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", meta.Name, err)
	}

	// Try to clean up empty parent directories up to syncDir.
	watcher.CleanEmptyParents(destPath, s.syncDir, s.ignorePatterns, s.logger)

	// Update store AFTER disk removal. Same reasoning as downloadAndApply:
	// if PutFile fails after the file is already removed, the periodic scan
	// will detect the deletion and create the store entry (self-healing).
	// The reverse (PutFile first, then Remove fails) would leave the store
	// saying "deleted" while the file still exists on disk.
	if _, err := s.store.PutFile(meta.Name, meta.ModTime, 0, "", true); err != nil {
		return fmt.Errorf("mark deleted in store %s: %w", meta.Name, err)
	}

	s.logger.Info("file deletion synced", "name", meta.Name)
	return nil
}

// encodePathSegments URL-encodes each segment of a slash-separated path.
func encodePathSegments(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
