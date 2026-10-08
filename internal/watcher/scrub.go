package watcher

// One pass used to do two unrelated jobs: notice what changed, and verify that
// nothing changed behind our back. Both cost O(all bytes), so the interval had
// to be either too expensive for a large node or too rare to be a guarantee.
//
// They are separate here. The sweep is stat-only and cheap, so it can run often
// and is what readiness depends on. The scrub re-reads bytes continuously at a
// byte budget the operator sets, cycling through the tree and remembering where
// it stopped. A node holding 10 TB is then an ordinary node with a slow cycle,
// not one that must read 10 TB every scan_interval.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
)

const (
	scrubPositionKey = "scrub_position"
	// scrubPage is how many names are verified between position checkpoints.
	// Losing a checkpoint only re-verifies part of one page.
	scrubPage = 64
)

// sweepFile indexes a name, reading its bytes only when its size or timestamp
// disagree with the index. A rewrite that preserves both is what the scrub is
// for; every other change moves at least one of them.
func (w *Watcher) sweepFile(name string) error {
	if w.unchangedByStat(name) {
		return nil
	}
	return w.scanFile(name)
}

// sweepKnownFile uses an index page already read by the sweep. A changed or
// missing file is still rechecked under the commit lock against current state.
func (w *Watcher) sweepKnownFile(name string, meta *store.FileMeta) error {
	if !w.NeedsRepair(name) && meta != nil && !meta.Deleted {
		info, err := os.Lstat(filepath.Join(w.dir, filepath.FromSlash(name)))
		if err == nil && info.Mode().IsRegular() && info.Size() == meta.Size && info.ModTime().UnixNano() == meta.ModTime {
			return nil
		}
	}
	return w.scanFile(name)
}

func (w *Watcher) unchangedByStat(name string) bool {
	if w.NeedsRepair(name) {
		return false
	}
	meta, err := w.store.GetFile(name)
	if err != nil || meta == nil || meta.Deleted {
		return false
	}
	info, err := os.Lstat(filepath.Join(w.dir, filepath.FromSlash(name)))
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Size() == meta.Size && info.ModTime().UnixNano() == meta.ModTime
}

// scrubLoop verifies stored bytes forever at the configured rate. It is a
// background budget, not a deadline: it never fails readiness and never blocks
// the sweep, and a name it cannot verify is quarantined like any other.
func (w *Watcher) scrubLoop(ctx context.Context) {
	if w.scrubRate.Load() <= 0 {
		w.logger.Warn("checksum scrub disabled; silent corruption will only be found by a peer")
		return
	}
	w.logger.Info("checksum scrub started", "bytes_per_second", w.scrubRate.Load())

	position, err := w.store.NodeValue(scrubPositionKey)
	if err != nil {
		w.logger.Error("read scrub position failed", "error", err)
	}
	verified := 0
	for {
		if ctx.Err() != nil {
			return
		}
		page, err := w.store.ListNonDeleted(position, scrubPage)
		if err != nil {
			w.logger.Error("scrub could not read the index", "error", err)
			if !sleepCtx(ctx, time.Minute) {
				return
			}
			continue
		}
		if len(page) == 0 {
			// An empty node completes a cycle every time it looks, so announce
			// only cycles that actually read something. Otherwise a quiet node
			// writes a log line a minute, forever.
			w.finishScrubCycle(verified)
			verified = 0
			position = ""
			if err := w.store.SetNodeValue(scrubPositionKey, position); err != nil {
				w.logger.Error("persist scrub position failed", "error", err)
			}
			if !sleepCtx(ctx, time.Minute) {
				return
			}
			continue
		}
		for _, meta := range page {
			if ctx.Err() != nil {
				return
			}
			started := time.Now()
			err := w.scanFile(meta.Name)
			switch {
			case err == nil, errors.Is(err, fileops.ErrBusy), errors.Is(err, ErrIntegrity):
				// Busy is transient and integrity is already recorded and queued.
			default:
				w.logger.Warn("scrub could not verify a file", "name", meta.Name, "error", err)
			}
			position = meta.Name
			verified++
			if !w.spendScrubBudget(ctx, meta.Size, started) {
				return
			}
		}
		if err := w.store.SetNodeValue(scrubPositionKey, position); err != nil {
			w.logger.Error("persist scrub position failed", "error", err)
		}
	}
}

// spendScrubBudget waits until the bytes just read fit the configured rate.
func (w *Watcher) spendScrubBudget(ctx context.Context, size int64, started time.Time) bool {
	if size <= 0 {
		return ctx.Err() == nil
	}
	rate := w.scrubRate.Load()
	if rate <= 0 {
		return ctx.Err() == nil
	}
	owed := time.Duration(float64(size) / float64(rate) * float64(time.Second))
	if remaining := owed - time.Since(started); remaining > 0 {
		return sleepCtx(ctx, remaining)
	}
	return ctx.Err() == nil
}

func (w *Watcher) finishScrubCycle(verified int) {
	w.statusMu.Lock()
	w.lastScrub = time.Now()
	w.statusMu.Unlock()
	if verified == 0 {
		w.logger.Debug("checksum scrub found nothing to verify")
		return
	}
	w.logger.Info("checksum scrub completed a full cycle", "files", verified)
}

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
