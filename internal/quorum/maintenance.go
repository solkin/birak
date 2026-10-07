package quorum

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

type MaintenanceStatus struct {
	Pending                    int64
	Cursor                     string
	Running                    bool
	LastStarted, LastCompleted int64
	Checked, Repaired, Failed  int64
	LastError                  string
}

func (n *Node) MaintenanceStatus() MaintenanceStatus {
	n.maintenanceMu.Lock()
	defer n.maintenanceMu.Unlock()
	return n.maintenance
}

// Scrub verifies every retained local generation, fetching a verified copy on
// damage or absence. Followers and learners run it too: asynchronous redundancy
// must not depend on a subsequent client read or membership change.
// bytesPerSecond limits average work, including hashing healthy bytes. One file
// is the maximum burst; a cycle uses a fixed reference boundary.
func (n *Node) Scrub(ctx context.Context, bytesPerSecond int64) (err error) {
	if bytesPerSecond <= 0 {
		return errors.New("scrub rate must be positive")
	}
	n.maintenanceMu.Lock()
	if n.maintenance.Running {
		n.maintenanceMu.Unlock()
		return errors.New("scrub already running")
	}
	n.maintenance = MaintenanceStatus{Running: true, LastStarted: time.Now().UnixNano()}
	n.maintenanceMu.Unlock()
	defer func() {
		n.maintenanceMu.Lock()
		defer n.maintenanceMu.Unlock()
		n.maintenance.Running = false
		if err == nil {
			n.maintenance.LastCompleted = time.Now().UnixNano()
		}
		if err != nil {
			n.maintenance.LastError = err.Error()
		}
	}()
	progress, e := n.loadScrubProgress()
	if e != nil {
		return e
	}
	n.maintenanceMu.Lock()
	n.maintenance.Cursor = progress.After
	n.maintenance.LastCompleted = progress.Completed
	if progress.Failed {
		n.maintenance.Failed = 1
	}
	n.maintenanceMu.Unlock()
	// Persist progress on cancellation too, so planned restarts do not continually
	// restart a multi-day scan at the first hash. Checkpoint granularity is ten
	// seconds plus one file; no partially verified file advances the cursor.
	dirty := false
	defer func() {
		if dirty {
			err = errors.Join(err, n.saveScrubProgress(progress))
		}
	}()
	checkpoint := time.Now()
	s := n.fsm.catchupState()
	if s.Collection != nil {
		return ErrMaintenance
	}
	refs := make([]generation.Ref, 0, len(s.Generations))
	for _, ref := range s.Generations {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Hash < refs[j].Hash })
	var first error
	for _, ref := range refs {
		if ref.Hash <= progress.After {
			continue
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if n.fsm.control().Collection != nil {
			return ErrMaintenance
		}
		started := time.Now()
		f, e := n.objects.Open(ctx, ref)
		repaired := e != nil
		if e != nil {
			f2, e2 := n.openGeneration(ctx, ref)
			e = e2
			if e2 == nil {
				e = f2.Close()
			}
		} else {
			e = f.Close()
		}
		n.maintenanceMu.Lock()
		n.maintenance.Checked++
		if e != nil {
			n.maintenance.Failed++
			n.maintenance.LastError = e.Error()
			if first == nil {
				first = e
			}
		} else if repaired {
			n.maintenance.Repaired++
		}
		n.maintenanceMu.Unlock()
		// Retry failures on the next cycle while continuing through healthy files.
		progress.After = ref.Hash
		if e != nil {
			progress.Failed = true
		}
		dirty = true
		n.maintenanceMu.Lock()
		n.maintenance.Cursor = progress.After
		n.maintenanceMu.Unlock()
		delay := time.Duration(float64(ref.Size)/float64(bytesPerSecond)*float64(time.Second)) - time.Since(started)
		if time.Since(checkpoint)+max(delay, 0) >= 10*time.Second {
			if e := n.saveScrubProgress(progress); e != nil {
				return e
			}
			dirty = false
			checkpoint = time.Now()
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	progress.After = ""
	if progress.Failed && first == nil {
		first = errors.New("scrub cycle contains failed checks from before restart")
	}
	progress.Failed = false
	if first == nil {
		progress.Completed = time.Now().UnixNano()
	}
	dirty = true
	n.maintenanceMu.Lock()
	n.maintenance.Cursor = ""
	n.maintenanceMu.Unlock()
	return first
}

func (n *Node) RunMaintenance(ctx context.Context, interval time.Duration, bytesPerSecond int64, report func(error)) {
	if interval <= 0 || bytesPerSecond <= 0 {
		return
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			if err := n.Scrub(ctx, bytesPerSecond); err != nil && ctx.Err() == nil && report != nil {
				report(err)
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	defer workers.Wait()
	budget := int64(float64(bytesPerSecond) * interval.Seconds())
	if budget < 1 {
		budget = 1
	}
	for {
		started := time.Now()
		if err := n.Reconcile(ctx, budget); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		// Cycle start spacing bounds aggregate repair bytes, with one-object burst.
		delay := interval - time.Since(started)
		if delay < time.Millisecond {
			delay = time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (n *Node) ReplicationStatus() MaintenanceStatus {
	n.reconcileMu.Lock()
	defer n.reconcileMu.Unlock()
	return n.replication
}

// Reconcile prioritizes the newest live references and checks existence without
// hashing healthy files. Its bounded copy budget prevents a large learner
// backlog from delaying new writes for the duration of a full integrity scan.
func (n *Node) Reconcile(ctx context.Context, budget int64) (err error) {
	if budget <= 0 {
		return errors.New("repair budget must be positive")
	}
	n.reconcileMu.Lock()
	if n.replication.Running {
		n.reconcileMu.Unlock()
		return errors.New("reconciliation already running")
	}
	n.replication = MaintenanceStatus{Running: true, LastStarted: time.Now().UnixNano()}
	n.reconcileMu.Unlock()
	defer func() {
		n.reconcileMu.Lock()
		defer n.reconcileMu.Unlock()
		n.replication.Running = false
		n.replication.LastCompleted = time.Now().UnixNano()
		if err != nil {
			n.replication.LastError = err.Error()
		}
	}()
	n.fsm.mu.RLock()
	latest := map[generation.Ref]uint64{}
	for _, e := range n.fsm.state.Entries {
		if !e.Deleted && !e.MetaOnly && e.Index > latest[e.Ref] {
			latest[e.Ref] = e.Index
		}
	}
	n.fsm.mu.RUnlock()
	refs := make([]generation.Ref, 0, len(latest))
	for ref := range latest {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if latest[refs[i]] == latest[refs[j]] {
			return refs[i].Hash < refs[j].Hash
		}
		return latest[refs[i]] > latest[refs[j]]
	})
	var copied int64
	var first error
	for _, ref := range refs {
		if e := ctx.Err(); e != nil {
			return e
		}
		if n.fsm.control().Collection != nil {
			return ErrMaintenance
		}
		present, e := n.objects.Present(ctx, ref)
		repaired := false
		if e == nil && !present && copied < budget {
			f, e2 := n.openGeneration(ctx, ref)
			e = e2
			if e2 == nil {
				e = f.Close()
			}
			copied += ref.Size
			repaired = e == nil
		}
		n.reconcileMu.Lock()
		n.replication.Checked++
		if !present && !repaired {
			n.replication.Pending++
		}
		if e != nil {
			n.replication.Failed++
			n.replication.LastError = e.Error()
			if first == nil {
				first = e
			}
		} else if repaired {
			n.replication.Repaired++
		}
		n.reconcileMu.Unlock()
	}
	return first
}

type collectionPeers interface {
	Collect(context.Context, raft.ServerID, CollectionFence) (generation.Collection, error)
}
type CollectionResult struct {
	Fence  CollectionFence
	Nodes  map[raft.ServerID]generation.Collection
	Errors map[raft.ServerID]string
}

// Collect freezes publication in Raft and retires obsolete proof epochs. A
// crash leaves the fence installed. Repeating Collect resumes that exact fence;
// EndCollection can release it without deleting anything else.
func (n *Node) Collect(ctx context.Context, grace time.Duration) (CollectionResult, error) {
	out := CollectionResult{Nodes: map[raft.ServerID]generation.Collection{}, Errors: map[raft.ServerID]string{}}
	if grace < time.Hour {
		return out, errors.New("collection grace must be at least one hour")
	}
	if err := n.lock(ctx); err != nil {
		return out, err
	}
	defer n.unlock()
	if err := n.barrier(ctx); err != nil {
		return out, err
	}
	s := n.fsm.control()
	if s.Collection == nil {
		if _, err := n.apply(ctx, command{Kind: "collect-begin", ConfigIndex: s.ConfigIndex, Epoch: s.Epoch, Before: time.Now().Add(-grace).UnixNano()}); err != nil {
			return out, err
		}
		s = n.fsm.control()
	}
	out.Fence = *s.Collection
	for _, m := range s.Config.Servers {
		var result generation.Collection
		var err error
		if m.ID == n.id.Node {
			result, err = n.LocalCollect(ctx, out.Fence)
		} else if p, ok := n.peers.(collectionPeers); ok {
			result, err = p.Collect(ctx, m.ID, out.Fence)
		} else {
			err = errors.New("peer does not support collection")
		}
		out.Nodes[m.ID] = result
		if err != nil {
			out.Errors[m.ID] = err.Error()
		}
	}
	// Never hide an uncertain end behind a successful local sweep. The operator
	// must check the returned fence/status and resume or explicitly release it.
	_, err := n.apply(ctx, command{Kind: "collect-end", ConfigIndex: s.ConfigIndex, Epoch: out.Fence.Epoch})
	return out, err
}

func (n *Node) EndCollection(ctx context.Context) error {
	if err := n.lock(ctx); err != nil {
		return err
	}
	defer n.unlock()
	if err := n.barrier(ctx); err != nil {
		return err
	}
	s := n.fsm.control()
	if s.Collection == nil {
		return nil
	}
	_, err := n.apply(ctx, command{Kind: "collect-end", ConfigIndex: s.ConfigIndex, Epoch: s.Collection.Epoch})
	return err
}

// LocalCollect never trusts a caller's reference list. It uses its own applied
// frozen state and prevents both local FSM advancement and blob ACKs during
// unlink. A delayed sweep cannot run after thaw or delete a newly ACKed blob.
func (n *Node) LocalCollect(ctx context.Context, fence CollectionFence) (generation.Collection, error) {
	if fence.Epoch == 0 {
		return generation.Collection{}, ErrMaintenance
	}
	for n.fsm.control().Epoch < fence.Epoch {
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return generation.Collection{}, ctx.Err()
		case <-timer.C:
		}
	}
	for !n.blobGate.TryLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return generation.Collection{}, ctx.Err()
		case <-timer.C:
		}
	}
	defer n.blobGate.Unlock()
	n.fsm.mu.RLock()
	defer n.fsm.mu.RUnlock()
	s := &n.fsm.state
	if s.Collection == nil || *s.Collection != fence {
		return generation.Collection{}, ErrMaintenance
	}
	out, err := n.objects.Collect(ctx, s.Generations, time.Unix(0, fence.Before))
	if err != nil {
		return out, fmt.Errorf("local collection: %w", err)
	}
	return out, nil
}
