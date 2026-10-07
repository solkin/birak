package quorum

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

func (w wire) Collect(ctx context.Context, id raft.ServerID, f CollectionFence) (generation.Collection, error) {
	n, err := w.target(ctx, id)
	if err != nil {
		return generation.Collection{}, err
	}
	return n.LocalCollect(ctx, f)
}
func TestCollectionFencesOldProofsAcrossThawAndRestoresReceipts(t *testing.T) {
	f := newMachine("test")
	f.StoreConfiguration(1, raft.Configuration{Servers: []raft.Server{{ID: "a", Address: "a", Suffrage: raft.Voter}}})
	index := uint64(1)
	apply := func(c command) any {
		index++
		c.Format = Format
		c.Cluster = "test"
		c.ConfigIndex = 1
		b, err := encodeCommand(c)
		if err != nil {
			t.Fatal(err)
		}
		return f.Apply(&raft.Log{Index: index, Data: b})
	}
	ref := generation.Ref{Hash: strings.Repeat("a", 64), Size: 1}
	original := command{Kind: "mutate", Mutation: Mutation{ID: "original", Key: "key", Ref: ref}, Copies: []raft.ServerID{"a"}}
	if _, ok := apply(original).(Entry); !ok {
		t.Fatal("write failed")
	}
	if _, ok := apply(command{Kind: "mutate", Mutation: Mutation{ID: "delete", Key: "key", Delete: true}}).(Entry); !ok {
		t.Fatal("delete failed")
	}
	if v := apply(command{Kind: "collect-begin", Before: time.Now().UnixNano()}); v != nil {
		t.Fatal(v)
	}
	stale := original
	stale.Mutation.ID = "stale"
	if v := apply(stale); v != ErrMaintenance {
		t.Fatal(v)
	}
	if v := apply(command{Kind: "collect-end", Epoch: 1}); v != nil {
		t.Fatal(v)
	}
	if v := apply(stale); v != ErrMaintenance {
		t.Fatal("old proof revived", v)
	}
	if len(f.view().Generations) != 0 {
		t.Fatal("retired bytes retained by receipt")
	}
	if _, ok := apply(original).(Entry); !ok {
		t.Fatal("retry lost original result")
	}
	if e, ok := f.entry("key"); ok && !e.Deleted {
		t.Fatal("retry republished collected bytes")
	}
	b, _ := json.Marshal(f.view())
	restored := newMachine("test")
	if err := restored.Restore(io.NopCloser(bytes.NewReader(b))); err != nil {
		t.Fatal(err)
	}
}

func TestScrubRepairsBitrotAndContinuouslySeedsLearner(t *testing.T) {
	l := newLab(t)
	n := l.grow(3)
	ctx := context.Background()
	e, err := n.Put(ctx, "put", "key", strings.NewReader("healthy payload"), 100)
	if err != nil {
		t.Fatal(err)
	}
	learner := l.start("backup", false)
	if err := n.AddLearner(ctx, "backup", "backup"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := learner.fsm.entry("key"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("learner has no metadata")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := learner.Scrub(ctx, 1<<30); err != nil {
		t.Fatal(err)
	}
	if learner.MaintenanceStatus().Repaired != 1 {
		t.Fatal(learner.MaintenanceStatus())
	}
	// Every replica is made healthy first, then damage the leader's existing inode.
	for _, node := range l.nodes {
		if err := node.Scrub(ctx, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(l.dirs[n.id.Node], "generations", "objects", e.Ref.Hash[:2], e.Ref.Hash[2:])
	reader, err := n.LocalBlob(ctx, e.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := os.WriteFile(path, []byte("damaged payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := n.Scrub(ctx, 1<<30); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, generation.ErrCorrupt) {
		t.Fatal(err)
	}
	_, r, err := n.Read(ctx, "key")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "healthy payload" {
		t.Fatal(string(b), err)
	}
}

func TestCollectionCrashResumeAndDelayedSweepRefusal(t *testing.T) {
	l := newLab(t)
	n := l.grow(3)
	ctx := context.Background()
	e, err := n.Put(ctx, "old", "key", strings.NewReader("old"), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range l.nodes {
		if err := node.Scrub(ctx, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.Delete(ctx, "delete", "key"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, dir := range l.dirs {
		p := filepath.Join(dir, "generations", "objects", e.Ref.Hash[:2], e.Ref.Hash[2:])
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	s := n.fsm.control()
	if _, err := n.apply(ctx, command{Kind: "collect-begin", ConfigIndex: s.ConfigIndex, Epoch: s.Epoch, Before: time.Now().Add(-time.Hour).UnixNano()}); err != nil {
		t.Fatal(err)
	}
	fence := *n.fsm.control().Collection
	if err := n.ReceiveBlob(ctx, e.Ref, strings.NewReader("old")); !errors.Is(err, ErrMaintenance) {
		t.Fatal(err)
	}
	if err := n.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	id := n.id.Node
	l.stop(id)
	n = l.leader()
	if n.fsm.control().Collection == nil {
		t.Fatal("freeze lost with leader")
	}
	result, err := n.Collect(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fence != fence {
		t.Fatal("resume invented a new boundary")
	}
	if len(result.Errors) != 1 {
		t.Fatal("unreachable member not reported", result)
	}
	if _, err := n.LocalCollect(ctx, fence); !errors.Is(err, ErrMaintenance) {
		t.Fatal("delayed sweep ran after thaw", err)
	}
	if _, err := n.Put(ctx, "new", "key", strings.NewReader("new"), 10); err != nil {
		t.Fatal(err)
	}
	l.start(id, false)
	l.leader()
}

func TestNoReceiptTransactionsAndOfflineUpgrade(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := n.TransactOnce(ctx, "ephemeral", []Change{{Key: "key", MetaOnly: true}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.fsm.view().Operations) != 0 {
		t.Fatal("application transactions leaked receipts")
	}
	id := n.id
	dir := l.dirs[id.Node]
	if err := UpgradeV2(dir, id); err == nil {
		t.Fatal("upgraded a running node")
	}
	l.stop(id.Node)
	legacy := id
	legacy.Format = legacyFormat
	b, _ := json.Marshal(legacy)
	for _, p := range []string{"identity.json", "generations/identity.json"} {
		if err := os.WriteFile(filepath.Join(dir, p), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := UpgradeV2(dir, id); err != nil {
		t.Fatal(err)
	}
	if err := UpgradeV2(dir, id); err != nil {
		t.Fatal("upgrade not resumable", err)
	}
	n = l.start(id.Node, false)
	l.leader()
	if _, ok, err := n.Lookup(ctx, "key"); err != nil || !ok {
		t.Fatal(ok, err)
	}
}

func TestReconcilePrioritizesNewWritesAndBoundsStalledPeers(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	ctx := context.Background()
	old, err := n.Put(ctx, "old", "old", strings.NewReader("old"), 10)
	if err != nil {
		t.Fatal(err)
	}
	learner := l.start("backup", false)
	if err := n.AddLearner(ctx, "backup", "backup"); err != nil {
		t.Fatal(err)
	}
	latest, err := n.Put(ctx, "latest", "new", strings.NewReader("new"), 10)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for learner.fsm.control().Index < latest.Index {
		if time.Now().After(deadline) {
			t.Fatal("metadata not received")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := learner.Reconcile(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if learner.ReplicationStatus().Pending != 1 {
		t.Fatal("missing backlog not reported", learner.ReplicationStatus())
	}
	if f, err := learner.LocalBlob(ctx, latest.Ref); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if f, err := learner.LocalBlob(ctx, old.Ref); err == nil {
		f.Close()
		t.Fatal("copy budget not respected")
	}
	base := learner.peers
	learner.peers = stalledPeers{base}
	learner.transferTimeout = 20 * time.Millisecond
	started := time.Now()
	if err := learner.Reconcile(ctx, 100); err == nil {
		t.Fatal("stalled peer acknowledged")
	}
	if time.Since(started) > time.Second || learner.ReplicationStatus().Failed != 1 {
		t.Fatal("unbounded repair", learner.ReplicationStatus())
	}
	learner.peers = base
	learner.transferTimeout = time.Second
	if err := learner.Reconcile(ctx, 100); err != nil {
		t.Fatal(err)
	}
}

type stalledPeers struct{ Peers }

func (p stalledPeers) Open(ctx context.Context, _ raft.ServerID, _ generation.Ref) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestScrubPersistsInterruptedProgressAndRetriesFailedCycles(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	for _, key := range []string{"a", "b", "c"} {
		if _, err := n.Put(context.Background(), key, key, strings.NewReader(key), 1); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Scrub(ctx, 1) }()
	deadline := time.Now().Add(3 * time.Second)
	for n.MaintenanceStatus().Checked < 1 {
		if time.Now().After(deadline) {
			t.Fatal("scrub did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	progress, err := n.loadScrubProgress()
	if err != nil || progress.After == "" || progress.Completed != 0 {
		t.Fatal(progress, err)
	}
	// Restart the process, then finish only the unchecked suffix.
	l.stop("n1")
	n = l.start("n1", false)
	l.leader()
	if err := n.Scrub(context.Background(), 1<<30); err != nil {
		t.Fatal(err)
	}
	if n.MaintenanceStatus().Checked != 2 {
		t.Fatal("scan restarted at first hash", n.MaintenanceStatus())
	}
	progress, err = n.loadScrubProgress()
	if err != nil || progress.After != "" || progress.Completed == 0 {
		t.Fatal(progress, err)
	}
	// A prior failure must not become an all-healthy cycle merely by restarting.
	completed := progress.Completed
	progress.After = strings.Repeat("f", 64)
	progress.Failed = true
	if err := n.saveScrubProgress(progress); err != nil {
		t.Fatal(err)
	}
	if err := n.Scrub(context.Background(), 1<<30); err == nil {
		t.Fatal("lost failed prefix")
	}
	progress, err = n.loadScrubProgress()
	if err != nil || progress.Completed != completed {
		t.Fatal(progress, err)
	}
}

func TestCollectionDeadlineWhileAnOldTransferStillHoldsGate(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	s := n.fsm.control()
	if _, err := n.apply(context.Background(), command{Kind: "collect-begin", ConfigIndex: s.ConfigIndex, Epoch: s.Epoch, Before: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}
	fence := *n.fsm.control().Collection
	n.blobGate.RLock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := n.LocalCollect(ctx, fence)
	n.blobGate.RUnlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := n.EndCollection(context.Background()); err != nil {
		t.Fatal(err)
	}
}
