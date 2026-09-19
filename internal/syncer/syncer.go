package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// stallTimeout is the maximum time to wait for any data from a peer during a
// file download. If no bytes arrive within this window, the transfer is
// considered stalled and aborted. This replaces the old fixed 30s client
// timeout that was too short for large files.
const stallTimeout = 60 * time.Second

// Repair backoff bounds. A failing item is retried forever with growing delay —
// it is never dropped, because dropping it is exactly how a file goes missing
// on one node and is never noticed.
const (
	minRepairBackoff = 5 * time.Second
	maxRepairBackoff = 15 * time.Minute
)

// manifestPageSize is how many entries one reconciliation page carries.
const manifestPageSize = 1000

// errContentChanged means the peer's bytes no longer match the metadata we were
// given: the file was rewritten between the announcement and the download. It is
// not a failure of the file — the newer version simply has to be fetched — so it
// must never block the change stream.
var errContentChanged = errors.New("peer content changed during download")

// Options bundles the syncer's tuning knobs.
type Options struct {
	PollInterval           time.Duration
	BatchLimit             int
	MaxConcurrentDownloads int
	// RepairInterval is how often the durable repair queue is drained.
	RepairInterval time.Duration
	// ReconcileInterval is how often a full manifest comparison runs against
	// each peer. It rediscovers divergence independently of the stream cursor.
	// Zero disables full reconciliation.
	ReconcileInterval time.Duration
	// Secret is the optional cluster shared secret sent to peers.
	Secret string
}

// peerStat is the live replication state of one peer, surfaced by /status.
type peerStat struct {
	cursor          int64
	peerMaxVersion  int64
	epoch           string
	lastSuccess     time.Time
	lastReconcile   time.Time
	lastError       string
	consecutiveErrs int64
}

// Syncer polls peers for changes and downloads newer files.
type Syncer struct {
	store          *store.Store
	watcher        *watcher.Watcher
	syncDir        string
	nodeID         string
	peers          []string
	ignorePatterns []string
	logger         *slog.Logger
	opts           Options

	// client is used for metadata requests (/changes, /manifest, /meta).
	// File downloads use a separate client without a global timeout.
	client *http.Client

	// downloadClient has no overall timeout — large file transfers are
	// bounded by context cancellation and per-read stall detection instead.
	downloadClient *http.Client

	// paths serializes work on a single file name. Several peers can announce
	// the same path at once; without this the store could end up recording a
	// hash that a losing rename never put on disk, and this node would then
	// advertise content it does not have.
	paths *keyedMutex

	statsMu sync.Mutex
	stats   map[string]*peerStat

	// Unexported checkpoint for process-boundary regression tests.
	namespaceCheckpoint func(step, name string) error
}

// New creates a new Syncer.
func New(
	s *store.Store,
	w *watcher.Watcher,
	syncDir, nodeID string,
	peers []string,
	ignorePatterns []string,
	logger *slog.Logger,
	opts Options,
) *Syncer {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       90 * time.Second,
	}

	if opts.MaxConcurrentDownloads <= 0 {
		opts.MaxConcurrentDownloads = 1
	}
	if opts.RepairInterval <= 0 {
		opts.RepairInterval = 30 * time.Second
	}

	unique := make([]string, 0, len(peers))
	seen := make(map[string]bool)
	for _, peer := range peers {
		peer = strings.TrimRight(strings.TrimSpace(peer), "/")
		if !seen[peer] {
			unique = append(unique, peer)
			seen[peer] = true
		}
	}
	peers = unique
	w.SetRepairPeers(peers)
	stats := make(map[string]*peerStat, len(peers))
	for _, p := range peers {
		stats[p] = &peerStat{}
	}

	return &Syncer{
		store:          s,
		watcher:        w,
		syncDir:        syncDir,
		nodeID:         nodeID,
		peers:          peers,
		ignorePatterns: ignorePatterns,
		logger:         logger,
		opts:           opts,
		paths:          newKeyedMutex(),
		stats:          stats,
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

	s.logger.Info("syncer started",
		"peers", s.peers,
		"poll_interval", s.opts.PollInterval,
		"reconcile_interval", s.opts.ReconcileInterval,
		"max_concurrent_downloads", s.opts.MaxConcurrentDownloads,
	)

	var wg sync.WaitGroup
	for _, peer := range s.peers {
		// Three independent loops per peer. Repair and reconciliation must not
		// share a goroutine with polling: a slow full comparison would then
		// delay live changes, which is how a "backstop" turns into a bottleneck.
		for _, loop := range []func(context.Context, string){
			s.pollPeer, s.repairPeer, s.reconcilePeer,
		} {
			wg.Add(1)
			go func(fn func(context.Context, string), peerURL string) {
				defer wg.Done()
				fn(ctx, peerURL)
			}(loop, peer)
		}
	}
	wg.Wait()
	s.logger.Info("syncer stopped")
}

// ScanStatus and CheckStorage expose local readiness separately from peers.
func (s *Syncer) ScanStatus() watcher.ScanStatus { return s.watcher.Status() }
func (s *Syncer) CheckStorage() error            { return s.watcher.CheckStorage() }

// PeerStats implements server.StatsProvider.
func (s *Syncer) PeerStats() []server.PeerStatus {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()

	out := make([]server.PeerStatus, 0, len(s.peers))
	for _, peer := range s.peers {
		st := s.stats[peer]
		if st == nil {
			continue
		}
		pending, err := s.store.PendingRepairCount(peer)
		if err != nil {
			s.logger.Error("count pending repairs failed", "peer", peer, "error", err)
		}
		status := server.PeerStatus{
			Peer:            peer,
			Cursor:          st.cursor,
			PeerMaxVersion:  st.peerMaxVersion,
			Lag:             max64(0, st.peerMaxVersion-st.cursor),
			Epoch:           st.epoch,
			LastError:       st.lastError,
			ConsecutiveErrs: st.consecutiveErrs,
			Pending:         pending,
			// Healthy means "this peer's stream is moving": a recent successful
			// poll and nothing waiting for repair.
			Healthy: st.consecutiveErrs == 0 && pending == 0 &&
				!st.lastSuccess.IsZero() &&
				time.Since(st.lastSuccess) < 5*s.opts.PollInterval+time.Minute,
		}
		if !st.lastSuccess.IsZero() {
			status.LastSuccessAgo = time.Since(st.lastSuccess).Milliseconds()
		}
		if !st.lastReconcile.IsZero() {
			status.LastReconcileMS = time.Since(st.lastReconcile).Milliseconds()
		}
		out = append(out, status)
	}
	return out
}

func (s *Syncer) withStat(peerURL string, fn func(*peerStat)) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	st := s.stats[peerURL]
	if st == nil {
		st = &peerStat{}
		s.stats[peerURL] = st
	}
	fn(st)
}

// pollPeer continuously polls a single peer for changes.
func (s *Syncer) pollPeer(ctx context.Context, peerURL string) {
	backoff := s.opts.PollInterval
	maxBackoff := 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := s.syncOnce(ctx, peerURL)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("sync with peer failed", "peer", peerURL, "error", err)
			s.withStat(peerURL, func(st *peerStat) {
				st.lastError = err.Error()
				st.consecutiveErrs++
			})
			// Exponential backoff on error.
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Reset backoff on success.
		backoff = s.opts.PollInterval
		s.withStat(peerURL, func(st *peerStat) {
			st.lastSuccess = time.Now()
			st.lastError = ""
			st.consecutiveErrs = 0
		})

		if n > 0 {
			// Got changes — immediately poll again to drain.
			s.logger.Info("synced changes from peer", "peer", peerURL, "count", n)
			continue
		}

		// No changes — wait before the next poll. The jitter keeps a fleet of
		// pods from settling into lockstep against the same peer.
		if !sleepCtx(ctx, jitter(s.opts.PollInterval)) {
			return
		}
	}
}

// syncOnce fetches one batch of changes from a peer and applies them.
// Returns the number of changes processed.
func (s *Syncer) syncOnce(ctx context.Context, peerURL string) (int, error) {
	// At most one extra pass: the first may discover that our cursor is invalid
	// and reset it, and the retry then fetches from the start of the peer's
	// stream without waiting a full poll interval.
	for attempt := 0; ; attempt++ {
		n, reset, err := s.syncBatch(ctx, peerURL)
		if err != nil || !reset || attempt > 0 {
			return n, err
		}
	}
}

// syncBatch fetches and applies one batch. It reports reset=true when the peer's
// cursor was invalidated and the caller should immediately fetch again.
func (s *Syncer) syncBatch(ctx context.Context, peerURL string) (int, bool, error) {
	state, err := s.store.GetPeerState(peerURL)
	if err != nil {
		return 0, false, fmt.Errorf("get cursor for %s: %w", peerURL, err)
	}

	reqURL := fmt.Sprintf("%s/changes?since=%d&limit=%d&epoch=%s", peerURL, state.Version, s.opts.BatchLimit, url.QueryEscape(state.Epoch))
	resp, err := s.doRequest(ctx, reqURL)
	if err != nil {
		return 0, false, err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("peer %s returned status %d", peerURL, resp.StatusCode)
	}

	epoch := resp.Header.Get(server.HeaderEpoch)
	peerMax, hasMax := parseInt64Header(resp.Header.Get(server.HeaderMaxVersion))
	s.withStat(peerURL, func(st *peerStat) {
		st.cursor = state.Version
		st.epoch = epoch
		if hasMax {
			st.peerMaxVersion = peerMax
		}
	})

	// A peer whose metadata was rebuilt restarts numbering at 1. Our cursor is
	// then far ahead of anything it will ever produce, so "since=cursor" returns
	// an empty list forever and the peer becomes silently invisible. Detect it
	// from the epoch, and from a rewound max version for a database restored
	// from a backup (same epoch, lower versions).
	if reset, reason := needsCursorReset(state, epoch, peerMax, hasMax); reset {
		s.logger.Warn("resetting peer cursor",
			"peer", peerURL, "reason", reason,
			"old_cursor", state.Version, "old_epoch", state.Epoch, "new_epoch", epoch)
		if err := s.store.SetPeerState(peerURL, store.PeerState{Version: 0, Epoch: epoch}); err != nil {
			return 0, false, fmt.Errorf("reset cursor for %s: %w", peerURL, err)
		}
		return 0, true, nil
	}

	if state.Epoch != epoch && epoch != "" {
		// First contact, or an upgrade from a version that had no epoch.
		if err := s.store.SetPeerState(peerURL, store.PeerState{Version: state.Version, Epoch: epoch}); err != nil {
			return 0, false, fmt.Errorf("record peer epoch for %s: %w", peerURL, err)
		}
	}

	var changes []store.FileMeta
	if err := json.NewDecoder(resp.Body).Decode(&changes); err != nil {
		return 0, false, fmt.Errorf("decode changes from %s: %w", peerURL, err)
	}
	if len(changes) == 0 {
		return 0, false, nil
	}

	// Collapse the batch to the newest entry per name. Without this, an older
	// version of a file that has since been rewritten is downloaded first, fails
	// its hash check against the peer's current bytes, and — because the cursor
	// cannot pass it — blocks that peer's entire stream permanently.
	latest, batchMax := collapseByName(changes)

	applied, err := s.applyBatch(ctx, peerURL, latest)
	if err != nil {
		return 0, false, err
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}

	// The cursor may advance past everything in the batch, including failures:
	// each one is now durably recorded in the repair queue and retried out of
	// band. Nothing is dropped, and one bad file cannot hold up the rest.
	if batchMax > state.Version {
		if err := s.store.SetPeerState(peerURL, store.PeerState{Version: batchMax, Epoch: epoch}); err != nil {
			return 0, false, fmt.Errorf("update cursor for %s: %w", peerURL, err)
		}
		s.withStat(peerURL, func(st *peerStat) { st.cursor = batchMax })
		s.logger.Debug("cursor updated", "peer", peerURL, "version", batchMax)
	}

	if applied > 0 {
		s.logger.Debug("batch applied", "peer", peerURL, "applied", applied, "batch", len(changes))
	}
	return len(changes), false, nil
}

// applyBatch applies deduplicated changes, downloading up to
// MaxConcurrentDownloads files at a time. Failures are queued for repair rather
// than aborting the batch.
func (s *Syncer) applyBatch(ctx context.Context, peerURL string, changes []store.FileMeta) (int, error) {
	sem := make(chan struct{}, s.opts.MaxConcurrentDownloads)
	var wg sync.WaitGroup
	var applied atomic.Int64
	var errorsMu sync.Mutex
	var batchErr error

	for _, change := range changes {
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return int(applied.Load()), ctx.Err()
		}

		wg.Add(1)
		go func(change store.FileMeta) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := s.applyChange(ctx, peerURL, change); err != nil {
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, errContentChanged) {
					// The peer simply rewrote the file mid-transfer. Expected
					// churn on a hot path, not a fault — the repair pass picks
					// up whatever version the peer holds now.
					s.logger.Info("peer content changed mid-download, queued for repair",
						"name", change.Name, "peer", peerURL)
				} else {
					s.logger.Error("apply change failed, queued for repair",
						"name", change.Name, "peer", peerURL, "error", err)
				}
				if qErr := s.store.EnqueueChange(peerURL, change, err.Error()); qErr != nil {
					s.logger.Error("enqueue repair failed", "name", change.Name, "error", qErr)
					errorsMu.Lock()
					batchErr = errors.Join(batchErr, qErr)
					errorsMu.Unlock()
				}
				return
			}
			applied.Add(1)
			// The repair worker clears its own revision after verifying current
			// state. An unconditional delete here could erase a concurrent enqueue.
		}(change)
	}

	wg.Wait()
	return int(applied.Load()), errors.Join(batchErr, ctx.Err())
}

// applyChange brings one file in line with a peer's metadata. The decision and
// the write happen under the same per-path lock, so two peers announcing the
// same file cannot interleave into a store entry that disagrees with the disk.
func (s *Syncer) applyChange(ctx context.Context, peerURL string, change store.FileMeta) error {
	if err := s.validateChange(change); err != nil {
		// Unusable metadata is not a transient failure; retrying cannot help.
		s.logger.Warn("skipping unusable metadata from peer",
			"name", change.Name, "peer", peerURL, "error", err)
		return nil
	}

	unlock := s.paths.lock(change.Name)
	defer unlock()
	commitUnlock := fileops.Lock(s.syncDir)
	refreshErr := s.watcher.RefreshLocked(change.Name)
	damaged := errors.Is(refreshErr, watcher.ErrIntegrity)
	if refreshErr != nil && !damaged {
		err := refreshErr
		commitUnlock()
		return err
	}

	local, err := s.store.GetFile(change.Name)
	if err != nil {
		commitUnlock()
		return fmt.Errorf("get local file %q: %w", change.Name, err)
	}

	if !remoteWins(local, change) && !(damaged && store.CompareState(&change, local) == 0) {
		commitUnlock()
		s.logger.Debug("local state wins, skipping", "name", change.Name, "peer", peerURL)
		return nil
	}
	if !damaged && local != nil && !local.Deleted && !change.Deleted && local.Hash == change.Hash && local.Size == change.Size {
		defer commitUnlock()
		path, err := s.safeLocalPath(change.Name)
		if err != nil {
			return err
		}
		if err := s.store.StageReplica(change); err != nil {
			return err
		}
		stamp := time.Unix(0, change.ModTime)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
		_, err = s.store.PutRemote(change)
		return err
	}
	commitUnlock()

	if change.Deleted {
		s.logger.Info("applying remote deletion", "name", change.Name, "peer", peerURL)
		return s.applyDeletion(ctx, change)
	}
	return s.downloadAndApply(ctx, peerURL, change)
}

// validateChange rejects peer metadata that could never be applied safely.
func (s *Syncer) validateChange(change store.FileMeta) error {
	// Peer metadata is untrusted input. Resolve it with the same path policy as
	// local gateways and require the canonical relative name to be unchanged;
	// otherwise names such as a/../../outside could escape after filepath.Join.
	if _, err := s.safeLocalPath(change.Name); err != nil {
		return err
	}
	if !change.Deleted && (change.Size < 0 || change.Size == math.MaxInt64 || !isSHA256Hex(change.Hash)) {
		return fmt.Errorf("malformed metadata (size=%d hash=%q)", change.Size, change.Hash)
	}
	if change.SupersededBy != "" {
		if !change.Deleted || change.Size != 0 || !isSHA256Hex(change.Hash) || !store.NamespaceConflict(change.Name, change.SupersededBy) {
			return fmt.Errorf("malformed namespace tombstone for %s", change.Name)
		}
		if _, err := s.safeLocalPath(change.SupersededBy); err != nil {
			return err
		}
	}
	if change.ConflictOf != "" {
		if change.Deleted || change.Name != store.ConflictCopyName(change.ConflictOf, change.Hash) {
			return fmt.Errorf("malformed conflict copy for %s", change.Name)
		}
		if _, err := s.safeLocalPath(change.ConflictOf); err != nil {
			return err
		}
	}
	return nil
}

// repairPeer drains the durable repair queue for one peer.
func (s *Syncer) repairPeer(ctx context.Context, peerURL string) {
	for {
		if !sleepCtx(ctx, jitter(s.opts.RepairInterval)) {
			return
		}

		items, err := s.store.DueRepairs(peerURL, s.opts.BatchLimit)
		if err != nil {
			s.logger.Error("read repair queue failed", "peer", peerURL, "error", err)
			continue
		}
		if len(items) == 0 {
			continue
		}

		s.logger.Info("processing repair queue", "peer", peerURL, "items", len(items))
		for _, item := range items {
			if ctx.Err() != nil {
				return
			}
			s.repairOne(ctx, peerURL, item)
		}
	}
}

// repairOne compares current source metadata with the durably queued state.
// A newer source state supersedes old work; missing source metadata cannot
// erase a deletion that this node already accepted into its repair queue.
func (s *Syncer) repairOne(ctx context.Context, peerURL string, item store.RepairItem) {
	meta, err := s.fetchMeta(ctx, peerURL, item.Name)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if item.Meta == nil || !item.Meta.Deleted {
			s.deferRepair(peerURL, item, fmt.Sprintf("fetch metadata: %v", err))
			return
		}
		// A durably accepted deletion needs no bytes from the source. Apply it
		// even while the peer is offline; any newer local state still wins.
		meta = item.Meta
	}
	if item.Meta != nil && store.CompareState(item.Meta, meta) > 0 {
		meta = item.Meta
	}
	if meta == nil {
		// Legacy queue rows lack a full operation. Absence on the peer is not
		// proof of success: keep the unresolved item visible for intervention.
		s.deferRepair(peerURL, item, "peer has no record and queued operation has no metadata")
		return
	}

	if err := s.applyChange(ctx, peerURL, *meta); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.deferRepair(peerURL, item, err.Error())
		return
	}

	s.logger.Info("repair succeeded", "name", item.Name, "peer", peerURL, "attempts", item.Attempts+1)
	if err := s.store.ResolveRepairItem(item); err != nil {
		s.logger.Error("resolve repair failed", "name", item.Name, "error", err)
	}
}

// deferRepair reschedules a failed item with exponential backoff. Items are kept
// forever; /status surfaces the backlog so a permanently failing file is visible
// rather than silently absent.
func (s *Syncer) deferRepair(peerURL string, item store.RepairItem, cause string) {
	backoff := minRepairBackoff << min(item.Attempts, 12)
	if backoff > maxRepairBackoff || backoff <= 0 {
		backoff = maxRepairBackoff
	}
	s.logger.Warn("repair deferred", "name", item.Name, "peer", peerURL,
		"attempts", item.Attempts+1, "retry_in", backoff, "cause", cause)
	if err := s.store.DeferRepairItem(item, backoff, cause); err != nil {
		s.logger.Error("defer repair failed", "name", item.Name, "error", err)
	}
}

// reconcilePeer periodically compares the full manifest with a peer and queues
// anything this node is missing or holds an older version of.
//
// A stream cursor cannot rediscover a difference below its current position.
// Reconciliation derives that difference from the manifest, independently of
// the cursor. Persistent I/O or connectivity failures still require recovery.
func (s *Syncer) reconcilePeer(ctx context.Context, peerURL string) {
	if s.opts.ReconcileInterval == 0 {
		return
	}
	// Stagger the first pass so a restarted fleet does not reconcile in unison.
	if !sleepCtx(ctx, jitter(s.opts.ReconcileInterval/4)) {
		return
	}
	for {
		if err := s.reconcileOnce(ctx, peerURL); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("reconcile with peer failed", "peer", peerURL, "error", err)
		} else {
			s.withStat(peerURL, func(st *peerStat) { st.lastReconcile = time.Now() })
		}
		if !sleepCtx(ctx, jitter(s.opts.ReconcileInterval)) {
			return
		}
	}
}

// reconcileOnce walks the peer's manifest page by page and queues every entry
// whose remote state should win locally.
func (s *Syncer) reconcileOnce(ctx context.Context, peerURL string) error {
	s.logger.Debug("reconciliation started", "peer", peerURL)

	var after string
	queued, scanned := 0, 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		reqURL := fmt.Sprintf("%s/manifest?after=%s&limit=%d",
			peerURL, url.QueryEscape(after), manifestPageSize)
		resp, err := s.doRequest(ctx, reqURL)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			// A peer running an older build has no /manifest. Reconciliation is
			// simply unavailable against it; the change stream still works.
			resp.Body.Close()
			s.logger.Debug("peer does not support reconciliation", "peer", peerURL)
			return nil
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("manifest from %s: status %d", peerURL, resp.StatusCode)
		}

		var page []store.FileMeta
		decErr := json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if decErr != nil {
			return fmt.Errorf("decode manifest from %s: %w", peerURL, decErr)
		}
		if len(page) == 0 {
			break
		}

		n, err := s.reconcilePage(peerURL, page)
		if err != nil {
			return err
		}
		queued += n
		scanned += len(page)
		after = page[len(page)-1].Name
	}

	if queued > 0 {
		s.logger.Info("reconciliation queued repairs",
			"peer", peerURL, "queued", queued, "scanned", scanned)
	} else {
		s.logger.Debug("reconciliation complete, no divergence",
			"peer", peerURL, "scanned", scanned)
	}
	return nil
}

// reconcilePage diffs one manifest page against the local store.
func (s *Syncer) reconcilePage(peerURL string, page []store.FileMeta) (int, error) {
	usable := make([]store.FileMeta, 0, len(page))
	names := make([]string, 0, len(page))
	for _, entry := range page {
		if s.validateChange(entry) == nil {
			usable = append(usable, entry)
			names = append(names, entry.Name)
		}
	}
	if len(names) == 0 {
		return 0, nil
	}

	local, err := s.store.GetFilesBatch(names)
	if err != nil {
		return 0, fmt.Errorf("batch lookup during reconcile: %w", err)
	}

	queued := 0
	for _, entry := range usable {
		mine := local[entry.Name]
		if !remoteWins(mine, entry) && !(s.watcher.NeedsRepair(entry.Name) && store.CompareState(&entry, mine) == 0) {
			continue
		}
		s.logger.Info("reconciliation found divergence",
			"name", entry.Name, "peer", peerURL,
			"remote_deleted", entry.Deleted, "have_local", mine != nil)
		if err := s.store.EnqueueChange(peerURL, entry, "reconcile"); err != nil {
			return queued, fmt.Errorf("enqueue reconcile repair: %w", err)
		}
		queued++
	}
	return queued, nil
}

// fetchMeta reads a peer's current metadata for one name. A nil result means the
// peer has no record for it.
func (s *Syncer) fetchMeta(ctx context.Context, peerURL, name string) (*store.FileMeta, error) {
	reqURL := fmt.Sprintf("%s/meta/%s", peerURL, encodePathSegments(name))
	resp, err := s.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meta for %s: status %d", name, resp.StatusCode)
	}

	var meta store.FileMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode meta for %s: %w", name, err)
	}
	// Trust the requested name over whatever the peer echoed back.
	meta.Name = name
	return &meta, nil
}

// doRequest issues an authenticated cluster GET.
func (s *Syncer) doRequest(ctx context.Context, reqURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	s.setClusterHeaders(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", reqURL, err)
	}
	if err := s.checkPeerIdentity(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func (s *Syncer) checkPeerIdentity(resp *http.Response) error {
	if resp.Header.Get(server.HeaderProtocol) != server.ProtocolVersion {
		return fmt.Errorf("incompatible replication protocol %q; require %s", resp.Header.Get(server.HeaderProtocol), server.ProtocolVersion)
	}
	if id := resp.Header.Get(server.HeaderNodeID); id != "" && id == s.nodeID {
		return fmt.Errorf("peer has duplicate node_id %q", id)
	}
	return nil
}

func (s *Syncer) setClusterHeaders(req *http.Request) {
	req.Header.Set(server.HeaderProtocol, server.ProtocolVersion)
	req.Header.Set(server.HeaderNodeID, s.nodeID)
	req.Header.Set(server.HeaderNodeEpoch, s.store.Incarnation())
	if s.opts.Secret != "" {
		req.Header.Set(server.HeaderSecret, s.opts.Secret)
	}
}

// downloadAndApply downloads a file from a peer and writes it locally.
// The caller holds the per-path lock.
func (s *Syncer) downloadAndApply(ctx context.Context, peerURL string, meta store.FileMeta) error {
	destPath, err := s.safeLocalPath(meta.Name)
	if err != nil {
		return err
	}
	if meta.Size < 0 || meta.Size == math.MaxInt64 || !isSHA256Hex(meta.Hash) {
		return fmt.Errorf("invalid metadata for %s", meta.Name)
	}

	// URL-encode each path segment to handle special characters and subdirectories.
	reqURL := fmt.Sprintf("%s/files/%s", peerURL, encodePathSegments(meta.Name))

	// A dedicated context lets the stall watchdog abort the transfer by
	// cancelling the request, instead of abandoning a goroutine that keeps
	// writing into a buffer the caller has moved on from.
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()

	req, err := http.NewRequestWithContext(reqCtx, "GET", reqURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	s.setClusterHeaders(req)

	// Use the download client (no global timeout) for file transfers.
	resp, err := s.downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", meta.Name, err)
	}
	defer resp.Body.Close()
	if err := s.checkPeerIdentity(resp); err != nil {
		return err
	}

	if resp.StatusCode == http.StatusNotFound {
		// The peer announced this file but cannot serve it right now — a
		// half-finished write, or an ignore rule that differs from ours.
		// This is retryable: silently skipping it is how a file goes missing
		// on one node forever.
		return fmt.Errorf("download %s: peer returned 404", meta.Name)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", meta.Name, resp.StatusCode)
	}

	// Stage without changing a namespace that may be blocked by a file.
	destDir := filepath.Dir(destPath)
	tmpFile, err := s.replicaTemp(destPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer fileops.ReleaseTemp(tmpFile)
	tmpPath := tmpFile.Name()
	cleanup := func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}

	// Hash while writing, with stall detection.
	hasher := sha256.New()
	// Stop one byte beyond the advertised size. Without this bound a malicious
	// peer could stream forever while continually avoiding the stall timeout.
	src := &progressReader{r: io.LimitReader(resp.Body, meta.Size+1)}
	stalled := watchStall(src, stallTimeout, cancelReq)
	written, copyErr := io.CopyBuffer(io.MultiWriter(tmpFile, hasher), src, make([]byte, 256*1024))
	stalled.stop()

	if copyErr != nil {
		cleanup()
		if stalled.fired() {
			return fmt.Errorf("write file %s: transfer stalled, no data for %v", meta.Name, stallTimeout)
		}
		return fmt.Errorf("write file %s: %w", meta.Name, copyErr)
	}
	if written != meta.Size {
		cleanup()
		return fmt.Errorf("%w: size mismatch for %s (expected %d, got %d)",
			errContentChanged, meta.Name, meta.Size, written)
	}

	gotHash := hex.EncodeToString(hasher.Sum(nil))
	if gotHash != meta.Hash {
		cleanup()
		// The peer rewrote the file between announcing it and serving it. The
		// newer version will arrive on its own; queueing a repair re-reads the
		// peer's current metadata and converges.
		return fmt.Errorf("%w: %s (expected %s, got %s)",
			errContentChanged, meta.Name, meta.Hash[:12], gotHash[:12])
	}

	// Reproduce the source's permissions. os.CreateTemp makes 0600 files, which
	// would leave every replica unreadable to any other user or sidecar.
	if mode, ok := parseFileMode(resp.Header.Get(server.HeaderMode)); ok {
		if err := os.Chmod(tmpPath, mode); err != nil {
			s.logger.Warn("set replica mode failed", "name", meta.Name, "error", err)
		}
	}

	// Set mod time to match the source.
	modTime := time.Unix(0, meta.ModTime)
	if err := os.Chtimes(tmpPath, modTime, modTime); err != nil {
		cleanup()
		return fmt.Errorf("set modtime for %s: %w", meta.Name, err)
	}

	// Durability before visibility: without the fsync a crash can make the
	// rename durable while the bytes are not, leaving a correctly named file
	// full of zeros that carries the source's mtime — which conflict resolution
	// then reads as "same age", so the good copy is never pulled back.
	// Mode and mtime are set first so they are flushed with the data.
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync file %s: %w", meta.Name, err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close file %s: %w", meta.Name, err)
	}

	// Downloading does not reserve the destination. Reinspect after the entire
	// transfer, under the lock also used by gateways and the watcher.
	defer os.Remove(tmpPath)
	unlock := fileops.Lock(s.syncDir)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	refreshErr := s.watcher.RefreshLocked(meta.Name)
	damaged := errors.Is(refreshErr, watcher.ErrIntegrity)
	if refreshErr != nil && !damaged {
		return refreshErr
	}
	current, err := s.store.GetFile(meta.Name)
	if err != nil {
		return err
	}
	if !remoteWins(current, meta) && !(damaged && store.CompareState(&meta, current) == 0) {
		return nil
	}
	// Validate again after waiting: an ancestor may have been moved meanwhile.
	destPath, err = s.safeLocalPath(meta.Name)
	if err != nil {
		return err
	}

	apply, err := s.settleNamespaceLocked(ctx, meta, tmpPath)
	if err != nil || !apply {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	if err := s.store.StageReplica(meta); err != nil {
		return err
	}
	// Atomic rename — file appears on disk.
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename %s: %w", meta.Name, err)
	}
	// Flush the directory entry before touching the store. This keeps the store
	// behind the disk on a crash, never ahead: a store entry for a file whose
	// rename was lost would make the next periodic scan see "indexed but
	// missing" and broadcast a deletion, destroying the file on every peer.
	if err := fileops.SyncParents(destDir, s.syncDir); err != nil {
		return fmt.Errorf("persist replica directory: %w", err)
	}
	if err := s.namespaceStep("published", meta.Name); err != nil {
		return err
	}

	// Update local store AFTER rename. This ordering is critical: if PutFile
	// were called before Rename and Rename failed, the store would record a
	// file that doesn't exist on disk. On retry the syncer would see
	// local.Hash == change.Hash and SKIP the file, leaving it permanently
	// missing from disk (explaining size discrepancies between nodes).
	// With Rename-then-PutFile, a PutFile failure is self-healing: the file
	// is on disk, the watcher or periodic scan will detect it and create the
	// store entry.
	if _, err := s.store.PutRemote(meta); err != nil {
		return fmt.Errorf("update store for %s: %w", meta.Name, err)
	}

	if err := s.watcher.RefreshLocked(meta.Name); err != nil {
		return err
	}
	s.logger.Info("file synced", "name", meta.Name, "size", meta.Size, "hash", meta.Hash[:12], "peer", peerURL)
	return nil
}

// applyDeletion removes a file locally and marks it as deleted in the store.
// The caller holds the per-path lock.
func (s *Syncer) applyDeletion(ctx context.Context, meta store.FileMeta) error {
	destPath, err := s.safeLocalPath(meta.Name)
	if err != nil {
		return err
	}

	unlock := fileops.Lock(s.syncDir)
	defer unlock()
	refreshErr := s.watcher.RefreshLocked(meta.Name)
	damaged := errors.Is(refreshErr, watcher.ErrIntegrity)
	if refreshErr != nil && !damaged {
		return refreshErr
	}
	local, err := s.store.GetFile(meta.Name)
	if err != nil {
		return err
	}
	if !remoteWins(local, meta) {
		return nil
	}
	destPath, err = s.safeLocalPath(meta.Name)
	if err != nil {
		return err
	}

	return s.commitDeletionLocked(ctx, meta, destPath)
}

// Caller holds the namespace lock. Recheck each displaced generation and make
// its conflict copy durable before staging any destructive operation.
func (s *Syncer) commitDeletionLocked(ctx context.Context, meta store.FileMeta, destPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if meta.SupersededBy != "" {
		if err := s.watcher.RefreshLocked(meta.Name); err != nil {
			return err
		}
		local, err := s.store.GetFile(meta.Name)
		if err != nil {
			return err
		}
		if !remoteWins(local, meta) {
			return nil
		}
		if local != nil && !local.Deleted {
			if err := s.preserveConflictLocked(ctx, *local, destPath); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.store.StageReplica(meta); err != nil {
		return err
	}
	if meta.SupersededBy != "" {
		if err := s.namespaceStep("delete-intent", meta.Name); err != nil {
			return err
		}
	}
	// A file tombstone says nothing about implicit directories at that name.
	// Children can arrive first in a concurrent batch, or already exist locally.
	if err := fileops.RemoveReplicaFile(destPath); err != nil {
		return fmt.Errorf("remove %s: %w", meta.Name, err)
	}
	if err := fileops.SyncSurvivingParent(destPath, s.syncDir); err != nil {
		return err
	}
	if meta.SupersededBy == "" {
		watcher.CleanEmptyParentsLocked(destPath, s.syncDir, s.ignorePatterns, s.logger)
	}
	if meta.SupersededBy != "" {
		if err := s.namespaceStep("unlinked", meta.Name); err != nil {
			return err
		}
	}

	// Filesystem first; the durable intent restores the source clock if SQLite
	// fails here or the process stops before the metadata commit.
	if _, err := s.store.PutRemote(meta); err != nil {
		return fmt.Errorf("mark deleted in store %s: %w", meta.Name, err)
	}
	if meta.SupersededBy != "" {
		if err := s.namespaceStep("resolved", meta.Name); err != nil {
			return err
		}
	}

	s.logger.Info("file deletion synced", "name", meta.Name)
	return nil
}

// safeLocalPath resolves a peer-supplied file name under syncDir without
// rewriting it. Watcher-generated names are already canonical, so any change
// during cleaning signals malformed or traversal-oriented peer metadata.
func (s *Syncer) safeLocalPath(name string) (string, error) {
	normalized := filepath.ToSlash(name)
	rel, full, err := gateway.SafePath(s.syncDir, normalized, s.ignorePatterns)
	if err != nil {
		return "", fmt.Errorf("unsafe sync path %q: %w", name, err)
	}
	if rel == "" || rel != normalized {
		return "", fmt.Errorf("unsafe sync path %q: non-canonical name", name)
	}
	return full, nil
}

// --- conflict resolution ---

// remoteWins reports whether a peer's entry should replace the local one.
//
// The ordering is total and identical on every node, which is what makes
// convergence possible: the per-path logical clock orders mutations; mtime,
// live/deleted state and hash break ties between concurrent changes. Incoming
// replication preserves that clock instead of inventing a local mutation.
func remoteWins(local *store.FileMeta, remote store.FileMeta) bool {
	return store.CompareState(&remote, local) > 0
}

// needsCursorReset reports whether a peer's identity or version space changed in
// a way that invalidates our cursor.
func needsCursorReset(state store.PeerState, epoch string, peerMax int64, hasMax bool) (bool, string) {
	if state.Version == 0 {
		return false, ""
	}
	if epoch != "" && state.Epoch != "" && epoch != state.Epoch {
		return true, "peer epoch changed (metadata rebuilt)"
	}
	if hasMax && peerMax < state.Version {
		return true, "peer max version rewound (database restored)"
	}
	return false, ""
}

// collapseByName reduces a change batch to the newest entry per name and returns
// the highest version seen across the whole batch.
func collapseByName(changes []store.FileMeta) ([]store.FileMeta, int64) {
	var batchMax int64
	index := make(map[string]int, len(changes))
	out := make([]store.FileMeta, 0, len(changes))

	for _, change := range changes {
		if change.Version > batchMax {
			batchMax = change.Version
		}
		if pos, ok := index[change.Name]; ok {
			if change.Version > out[pos].Version {
				out[pos] = change
			}
			continue
		}
		index[change.Name] = len(out)
		out = append(out, change)
	}
	return out, batchMax
}

// --- transfer helpers ---

// progressReader counts bytes so the stall watchdog can tell a slow transfer
// from a dead one without touching the reader's state.
type progressReader struct {
	r io.Reader
	n atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.n.Add(int64(n))
	}
	return n, err
}

// stallWatch aborts a transfer that stops making progress.
type stallWatch struct {
	done    chan struct{}
	tripped atomic.Bool
	once    sync.Once
}

func (w *stallWatch) stop()       { w.once.Do(func() { close(w.done) }) }
func (w *stallWatch) fired() bool { return w.tripped.Load() }

// watchStall cancels the transfer when no byte arrives for the whole timeout.
// Cancelling the request context makes the in-flight Read return an error, so
// the copy unwinds normally — no goroutine is left writing into a buffer its
// caller has already abandoned.
func watchStall(src *progressReader, timeout time.Duration, cancel context.CancelFunc) *stallWatch {
	w := &stallWatch{done: make(chan struct{})}
	tick := timeout / 4
	if tick <= 0 {
		tick = time.Second
	}

	go func() {
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		last := src.n.Load()
		idle := time.Duration(0)
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				cur := src.n.Load()
				if cur != last {
					last = cur
					idle = 0
					continue
				}
				idle += tick
				if idle >= timeout {
					w.tripped.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return w
}

// --- small helpers ---

// keyedMutex serializes work per file name without holding a global lock.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*keyedEntry)}
}

// lock acquires the per-key mutex and returns its release function.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	entry, ok := k.locks[key]
	if !ok {
		entry = &keyedEntry{}
		k.locks[key] = entry
	}
	entry.refs++
	k.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// sleepCtx waits for d, returning false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// jitter spreads periodic work so a fleet of pods does not act in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(int64(d)/2+1))
}

func parseInt64Header(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseFileMode reads an octal permission string, rejecting anything outside the
// permission bits so a peer cannot ask us to create a setuid file.
func parseFileMode(v string) (os.FileMode, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 8, 32)
	if err != nil || n == 0 || n > 0o777 {
		return 0, false
	}
	return os.FileMode(n), true
}

func isSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// encodePathSegments URL-encodes each segment of a slash-separated path.
func encodePathSegments(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
