package watcher

// A file that is indexed and no longer on disk becomes a tombstone, and the
// tombstone deletes the file on every peer. That is right when somebody deleted
// it. It is a disaster when the disk just stopped showing it: a directory that
// was a separate mount and is no longer mounted, a dataset that failed to
// import, a backup restored over only part of the tree. The storage sentinel
// guards the root against that; nothing guarded a subtree, and a vanished
// subtree became tens of thousands of tombstones within one sweep.
//
// Deletions this node infers from absence — the event stream, the sweep — are
// therefore collected and decided as a group. A group that fits the budget is
// published; one that does not is held entirely, nothing of it published, and
// the node reports a fault until the files come back or an operator raises the
// budget. The scrub infers nothing and leaves absence to those two. Deletions
// made through a gateway or applied from a peer are explicit and never pass
// through here.
//
// The syncer's own look at a local name before it applies a peer's change
// (RefreshLocked and friends) still publishes a deletion it finds, one name at
// a time and only for names a peer is changing; it is not counted here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/birak/birak/internal/fileops"
)

// DefaultMaxExternalDeletions is how many inferred deletions a node publishes
// within externalDeletionWindow before it stops and asks.
const DefaultMaxExternalDeletions = 1000

// externalDeletionWindow is the period the budget applies to. Short enough
// that ordinary housekeeping on the host never waits long, long enough that a
// subtree disappearing over a few sweeps still adds up to one large number.
const externalDeletionWindow = 10 * time.Minute

// externalDeletionsKey persists what was published within the window, so a
// restart — or a restart loop — does not hand out a fresh budget.
const externalDeletionsKey = "external_deletions_window"

// deletionGroup collects names found missing that are decided together. It
// keeps at most keep names: a group larger than the budget is held whatever
// its names are, so only its size is needed then.
type deletionGroup struct {
	keep  int
	names []string
	seen  map[string]bool
	count int
}

func newDeletionGroup(keep int) *deletionGroup {
	return &deletionGroup{keep: keep, seen: make(map[string]bool)}
}

func (g *deletionGroup) add(name string) {
	if g.seen[name] {
		return
	}
	g.count++
	if len(g.names) < g.keep {
		g.seen[name] = true
		g.names = append(g.names, name)
	}
}

func (g *deletionGroup) merge(other *deletionGroup) {
	if other == nil {
		return
	}
	for _, name := range other.names {
		g.add(name)
	}
	// Names beyond what other kept are counted, not known; they may repeat
	// names already here, which only makes a hold more likely.
	g.count += other.count - len(other.names)
}

func (g *deletionGroup) samples() []string {
	n := min(len(g.names), maxSkipSamples)
	out := make([]string, n)
	for i := range n {
		out[i] = strconv.Quote(g.names[i])
	}
	return out
}

// publication is one published group within the window.
type publication struct {
	At    int64 `json:"at"`
	Count int   `json:"count"`
}

// deletionBudget is the breaker's state. settleMu serializes decisions,
// including the publishing they lead to; mu guards the fields, so Status never
// waits for a group being published.
type deletionBudget struct {
	settleMu sync.Mutex

	mu        sync.Mutex
	events    *deletionGroup // inferred by the event path, not yet decided
	window    []publication
	loaded    bool
	held      int
	heldError string
}

// errHeldDeletions marks a group the breaker refused to publish.
var errHeldDeletions = errors.New("external deletions held")

// SetMaxExternalDeletions sets how many deletions inferred from absence the
// node publishes within ten minutes. Zero publishes every one of them at once,
// as releases before the breaker did.
func (w *Watcher) SetMaxExternalDeletions(limit int) {
	w.maxExternalDeletions.Store(int64(max(0, limit)))
}

// sweepDeletions returns the group a sweep collects its inferred deletions
// in, or nil when the breaker is off and they are published as found.
func (w *Watcher) sweepDeletions() *deletionGroup {
	limit := w.maxExternalDeletions.Load()
	if limit <= 0 {
		return nil
	}
	return newDeletionGroup(int(limit))
}

// deferEventDeletion collects a deletion the event path inferred. The group is
// decided once the backlog drains; see settleEventDeletions.
func (w *Watcher) deferEventDeletion(name string) {
	b := &w.deletions
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.events == nil {
		b.events = newDeletionGroup(int(max(1, w.maxExternalDeletions.Load())))
	}
	b.events.add(name)
}

// takeEventDeletions hands over what the event path has collected.
func (w *Watcher) takeEventDeletions() *deletionGroup {
	b := &w.deletions
	b.mu.Lock()
	defer b.mu.Unlock()
	group := b.events
	b.events = nil
	return group
}

// settleEventDeletions decides the deletions the event path collected. A held
// group is a fault like any other the event path reports.
func (w *Watcher) settleEventDeletions(ctx context.Context) {
	group := w.takeEventDeletions()
	if group == nil || group.count == 0 {
		return
	}
	if err := w.settleDeletions(ctx, group, false); err != nil && !errors.Is(err, errHeldDeletions) {
		w.setError(err)
		w.logger.Error("publish inferred deletions failed", "error", err)
	}
}

// settleDeletions publishes group if it fits what is left of the budget, and
// holds all of it otherwise. A complete sweep is authoritative: it found every
// name that is missing, so its group replaces whatever was held before, and an
// empty one clears the fault. The event path only ever adds to a hold.
func (w *Watcher) settleDeletions(ctx context.Context, group *deletionGroup, authoritative bool) error {
	b := &w.deletions
	b.settleMu.Lock()
	defer b.settleMu.Unlock()
	limit := int(w.maxExternalDeletions.Load())

	b.mu.Lock()
	w.loadDeletionWindowLocked()
	used := w.publishedInWindowLocked()
	alreadyHeld := b.held > 0
	b.mu.Unlock()

	if group == nil || group.count == 0 {
		if authoritative {
			w.setHeldDeletions(0, "")
		}
		return nil
	}
	if !authoritative && alreadyHeld {
		// While a group is held, the next sweep decides about these too: they
		// are missing for the same reason or not, and only its full count can
		// tell. It finds them again, so they are not counted twice here.
		_, message := w.heldDeletions()
		w.logger.Debug("deletions found by the event path wait for the next scan", "missing", group.count)
		return fmt.Errorf("%w: %s", errHeldDeletions, message)
	}
	if limit > 0 && group.count > limit-used {
		message := fmt.Sprintf("holding %d external deletions (limit %d per %d minutes", group.count, limit, int(externalDeletionWindow/time.Minute))
		if used > 0 {
			message += fmt.Sprintf(", %d already published", used)
		}
		message += "): restore the files, or restart with sync.max_external_deletions raised to accept them"
		w.setHeldDeletions(group.count, message)
		w.logger.Error("files missing from disk are not published as deletions",
			"missing", group.count, "limit", limit, "published_in_window", used,
			"examples", group.samples(), "action", "restore the files, or restart with sync.max_external_deletions raised to accept them")
		return fmt.Errorf("%w: %s", errHeldDeletions, message)
	}
	published, err := w.publishDeletions(ctx, group.names)
	if published > 0 {
		b.mu.Lock()
		b.window = append(b.window, publication{At: w.now().UnixNano(), Count: published})
		w.saveDeletionWindowLocked()
		b.mu.Unlock()
	}
	if err != nil {
		return err
	}
	if authoritative {
		w.setHeldDeletions(0, "")
	}
	return nil
}

// publishDeletions indexes each name again under the commit lock, one at a
// time so writers are not held up behind the group. A file that came back is
// indexed as present; only one still missing becomes a tombstone.
func (w *Watcher) publishDeletions(ctx context.Context, names []string) (int, error) {
	published := 0
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return published, err
		}
		deleted, err := w.publishDeletion(name)
		if deleted {
			published++
		}
		switch {
		case err == nil, errors.Is(err, fileops.ErrBusy), errors.Is(err, ErrIntegrity):
		case unindexable(err) != "":
			w.logger.Warn("file cannot be indexed and is left alone",
				"name", strconv.Quote(name), "reason", unindexable(err), "error", err)
		default:
			return published, err
		}
	}
	return published, nil
}

func (w *Watcher) publishDeletion(name string) (bool, error) {
	unlock := fileops.Lock(w.dir)
	defer unlock()
	if err := w.prepareStorageLocked(); err != nil {
		return false, &storageFault{err}
	}
	before, err := w.store.GetFile(name)
	if err != nil || before == nil || before.Deleted {
		return false, err
	}
	if err := w.refreshFileLocked(name, indexOptions{}); err != nil {
		return false, err
	}
	after, err := w.store.GetFile(name)
	return err == nil && after != nil && after.Deleted, err
}

func (w *Watcher) setHeldDeletions(count int, message string) {
	b := &w.deletions
	b.mu.Lock()
	b.held, b.heldError = count, message
	b.mu.Unlock()
}

func (w *Watcher) heldDeletions() (int, string) {
	b := &w.deletions
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held, b.heldError
}

// publishedInWindowLocked drops publications older than the window and counts
// the rest. Caller holds deletions.mu.
func (w *Watcher) publishedInWindowLocked() int {
	b := &w.deletions
	cutoff := w.now().Add(-externalDeletionWindow).UnixNano()
	kept := b.window[:0]
	used := 0
	for _, p := range b.window {
		if p.At > cutoff {
			kept = append(kept, p)
			used += p.Count
		}
	}
	b.window = kept
	return used
}

// loadDeletionWindowLocked reads the persisted window once. A window that
// cannot be read is logged and treated as empty: the budget still applies to
// everything from here on. Caller holds deletions.mu.
func (w *Watcher) loadDeletionWindowLocked() {
	b := &w.deletions
	if b.loaded {
		return
	}
	b.loaded = true
	raw, err := w.store.NodeValue(externalDeletionsKey)
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &b.window)
	}
	if err != nil {
		w.logger.Warn("could not read the external deletion budget; starting a new window", "error", err)
		b.window = nil
	}
	sort.Slice(b.window, func(i, j int) bool { return b.window[i].At < b.window[j].At })
}

// saveDeletionWindowLocked persists the window. Caller holds deletions.mu.
func (w *Watcher) saveDeletionWindowLocked() {
	b := &w.deletions
	w.publishedInWindowLocked()
	data, err := json.Marshal(b.window)
	if err == nil {
		err = w.store.SetNodeValue(externalDeletionsKey, string(data))
	}
	if err != nil {
		w.logger.Warn("could not persist the external deletion budget", "error", err)
	}
}
