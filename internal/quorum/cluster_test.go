package quorum

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

// Each node owns real fsynced generation files, Bolt log/stable storage and
// snapshots. Only links are in-memory, so partitions are deterministic and the
// suite runs without privileged networking or fixed listening ports.
type lab struct {
	t       *testing.T
	mu      sync.RWMutex
	nodes   map[raft.ServerID]*Node
	trans   map[raft.ServerID]*raft.InmemTransport
	dirs    map[raft.ServerID]string
	blocked map[string]bool
	noData  map[raft.ServerID]bool
	gates   map[raft.ServerID]chan struct{}
	entered chan struct{}
}
type wire struct {
	lab  *lab
	from raft.ServerID
}

func (w wire) target(ctx context.Context, id raft.ServerID) (*Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.lab.mu.RLock()
	defer w.lab.mu.RUnlock()
	n := w.lab.nodes[id]
	if n == nil || w.lab.blocked[string(w.from)+"/"+string(id)] {
		return nil, errors.New("partitioned")
	}
	return n, nil
}
func (w wire) Identity(ctx context.Context, id raft.ServerID) (Identity, error) {
	n, err := w.target(ctx, id)
	if err != nil {
		return Identity{}, err
	}
	return n.id, nil
}
func (w wire) Receive(ctx context.Context, id raft.ServerID, ref generation.Ref, r io.Reader) error {
	n, err := w.target(ctx, id)
	if err != nil {
		return err
	}
	w.lab.mu.RLock()
	fail := w.lab.noData[id]
	gate := w.lab.gates[id]
	w.lab.mu.RUnlock()
	if fail {
		return errors.New("injected data disk/transfer failure")
	}
	if gate != nil {
		select {
		case w.lab.entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return n.ReceiveBlob(ctx, ref, r)
}
func (w wire) Open(ctx context.Context, id raft.ServerID, ref generation.Ref) (io.ReadCloser, error) {
	n, err := w.target(ctx, id)
	if err != nil {
		return nil, err
	}
	return n.objects.Open(ctx, ref)
}

func newLab(t *testing.T) *lab {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("strict directory durability is not yet supported on Windows")
	}
	l := &lab{t: t, nodes: map[raft.ServerID]*Node{}, trans: map[raft.ServerID]*raft.InmemTransport{}, dirs: map[raft.ServerID]string{}, blocked: map[string]bool{}, noData: map[raft.ServerID]bool{}, gates: map[raft.ServerID]chan struct{}{}, entered: make(chan struct{}, 1)}
	t.Cleanup(func() {
		for _, n := range l.nodes {
			if n != nil {
				n.Close()
			}
		}
		for _, tr := range l.trans {
			tr.Close()
		}
	})
	return l
}
func (l *lab) start(id raft.ServerID, bootstrap bool) *Node {
	l.t.Helper()
	if l.dirs[id] == "" {
		l.dirs[id] = filepath.Join(l.t.TempDir(), "node")
	}
	_, tr := raft.NewInmemTransportWithTimeout(raft.ServerAddress(id), 500*time.Millisecond)
	cfg := raft.DefaultConfig()
	// Leave scheduling headroom for parallel -race packages on shared runners.
	cfg.HeartbeatTimeout = 500 * time.Millisecond
	cfg.ElectionTimeout = 500 * time.Millisecond
	cfg.LeaderLeaseTimeout = 400 * time.Millisecond
	cfg.CommitTimeout = 5 * time.Millisecond
	cfg.LogOutput = io.Discard
	cfg.SnapshotThreshold = 1024
	cfg.TrailingLogs = 2
	n, err := Open(Options{Dir: l.dirs[id], Identity: Identity{Cluster: "test-cluster", Node: id, Format: Format}, Bootstrap: bootstrap, Transport: tr, Peers: wire{l, id}, RaftConfig: cfg, Timeout: 3 * time.Second})
	if err != nil {
		l.t.Fatal(err)
	}
	l.mu.Lock()
	l.nodes[id] = n
	l.trans[id] = tr
	for peer, pt := range l.trans {
		if peer != id && l.nodes[peer] != nil && !l.blocked[string(id)+"/"+string(peer)] {
			tr.Connect(raft.ServerAddress(peer), pt)
			pt.Connect(raft.ServerAddress(id), tr)
		}
	}
	l.mu.Unlock()
	return n
}
func (l *lab) stop(id raft.ServerID) {
	l.mu.Lock()
	n := l.nodes[id]
	l.nodes[id] = nil
	tr := l.trans[id]
	for peer, pt := range l.trans {
		if peer != id {
			pt.Disconnect(raft.ServerAddress(id))
		}
	}
	l.mu.Unlock()
	if n != nil {
		if err := n.Close(); err != nil {
			l.t.Fatal(err)
		}
	}
	tr.Close()
}
func (l *lab) isolate(id raft.ServerID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for peer, tr := range l.trans {
		if peer != id {
			l.blocked[string(id)+"/"+string(peer)] = true
			l.blocked[string(peer)+"/"+string(id)] = true
			tr.Disconnect(raft.ServerAddress(id))
			l.trans[id].Disconnect(raft.ServerAddress(peer))
		}
	}
}
func (l *lab) leader(exclude ...raft.ServerID) *Node {
	l.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.RLock()
		var candidates []*Node
		for id, n := range l.nodes {
			skip := false
			for _, x := range exclude {
				if id == x {
					skip = true
				}
			}
			if n != nil && !skip {
				candidates = append(candidates, n)
			}
		}
		l.mu.RUnlock()
		for _, n := range candidates {
			if n.raft.State() == raft.Leader {
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				err := n.barrier(ctx)
				cancel()
				if err == nil {
					return n
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.t.Fatal("no committed leader")
	return nil
}
func (l *lab) grow(count int) *Node {
	n := l.start("n1", true)
	l.leader()
	for i := 2; i <= count; i++ {
		id := raft.ServerID(fmt.Sprintf("n%d", i))
		l.start(id, false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := n.AddLearner(ctx, id, raft.ServerAddress(id))
		if err == nil {
			err = n.Promote(ctx, id)
		}
		cancel()
		if err != nil {
			l.t.Fatal(err)
		}
	}
	return n
}
func put(t *testing.T, n *Node, id, key, data string) Entry {
	t.Helper()
	e, err := n.Put(context.Background(), id, key, strings.NewReader(data), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func read(t *testing.T, n *Node, key, want string) Entry {
	t.Helper()
	e, r, err := n.Read(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil || string(b) != want {
		t.Fatalf("got %q want %q: %v", b, want, err)
	}
	return e
}

func TestMajoritiesAndAcknowledgedLoss(t *testing.T) {
	for size := 1; size <= 5; size++ {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			l := newLab(t)
			n := l.grow(size)
			e := put(t, n, "write", "bucket/App.apk", "payload")
			if got := len(voters(n.fsm.view().Config)); got != size {
				t.Fatalf("voters=%d", got)
			}
			// Every successful ACK must have an independent durable byte majority.
			copies := 0
			for _, peer := range l.nodes {
				if err := peer.objects.VerifyDurable(context.Background(), e.Ref); err == nil {
					copies++
				}
			}
			if copies < size/2+1 {
				t.Fatalf("ACK with %d/%d copies", copies, size)
			}
			losses := (size - 1) / 2
			if losses > 0 {
				l.stop(n.id.Node)
			}
			for i := 1; i < losses; i++ {
				l.stop(raft.ServerID(fmt.Sprintf("n%d", i+1)))
			}
			n = l.leader()
			read(t, n, "bucket/App.apk", "payload")
			put(t, n, "next", "bucket/next", "survived")
		})
	}
}

func TestDataQuorumSeparateFromRaftQuorum(t *testing.T) {
	l := newLab(t)
	n := l.grow(3)
	l.mu.Lock()
	l.noData["n2"] = true
	l.noData["n3"] = true
	l.mu.Unlock()
	if _, err := n.Put(context.Background(), "lost", "key", strings.NewReader("payload"), 100); !errors.Is(err, ErrDataQuorum) {
		t.Fatal(err)
	}
	if _, exists := n.fsm.view().Entries["key"]; exists {
		t.Fatal("published without byte quorum")
	}
	l.mu.Lock()
	l.noData["n2"] = false
	l.mu.Unlock()
	put(t, n, "lost", "key", "payload")
	l.stop("n1")
	read(t, l.leader(), "key", "payload")
}

func TestPartitionRejectsMinorityAndLearnerVotes(t *testing.T) {
	l := newLab(t)
	old := l.grow(3)
	put(t, old, "before", "key", "old")
	l.isolate(old.id.Node)
	majority := l.leader(old.id.Node)
	put(t, majority, "after", "key", "new")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := old.Put(ctx, "minority", "key", strings.NewReader("bad"), 100); err == nil {
		t.Fatal("minority acknowledged")
	}
	if _, r, err := old.Read(ctx, "key"); err == nil {
		r.Close()
		t.Fatal("minority served linearizable read")
	}
	read(t, l.leader(old.id.Node), "key", "new")
}

func TestLearnerNotCountedForDataQuorum(t *testing.T) {
	l := newLab(t)
	n := l.grow(2)
	l.start("n3", false)
	if err := n.AddLearner(context.Background(), "n3", "n3"); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.noData["n2"] = true
	l.mu.Unlock()
	if _, err := n.Put(context.Background(), "test", "key", strings.NewReader("data"), 100); !errors.Is(err, ErrDataQuorum) {
		t.Fatal(err)
	}
}

func TestSnapshotRestartIdempotencyAndOpaqueKeys(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	first := put(t, n, "first", "bucket/App.apk", "v1")
	put(t, n, "second", "bucket/App.apk", "v2")
	put(t, n, "case", "bucket/app.apk", "different")
	if _, err := n.Delete(context.Background(), "delete", "deleted"); err != nil {
		t.Fatal(err)
	}
	if err := n.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.stop("n1")
	n = l.start("n1", false)
	l.leader()
	read(t, n, "bucket/App.apk", "v2")
	read(t, n, "bucket/app.apk", "different")
	retry := put(t, n, "first", "bucket/App.apk", "v1")
	if retry != first {
		t.Fatalf("retry changed result: %+v %+v", retry, first)
	}
	read(t, n, "bucket/App.apk", "v2")
	if _, err := n.Put(context.Background(), "first", "bucket/App.apk", strings.NewReader("different"), 100); !errors.Is(err, ErrOperationID) {
		t.Fatal(err)
	}
	if _, _, err := n.Read(context.Background(), "deleted"); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := n.objects.VerifyDurable(context.Background(), first.Ref); err != nil {
		t.Fatal("lost overwritten generation", err)
	}
}

func TestPromotionFailurePersistsFreezeAndCanResume(t *testing.T) {
	l := newLab(t)
	n := l.grow(3)
	put(t, n, "initial", "key", "data")
	l.start("n4", false)
	if err := n.AddLearner(context.Background(), "n4", "n4"); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.noData["n4"] = true
	l.mu.Unlock()
	if err := n.Promote(context.Background(), "n4"); err == nil {
		t.Fatal("promoted empty learner")
	}
	if _, err := n.Delete(context.Background(), "blocked", "key"); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	l.stop(n.id.Node)
	n = l.leader()
	if _, err := n.Delete(context.Background(), "still-blocked", "key"); !errors.Is(err, ErrTransition) {
		t.Fatal("lost freeze after election", err)
	}
	l.mu.Lock()
	l.noData["n4"] = false
	l.mu.Unlock()
	// Restore the old voter: promotion from 3 to 4 needs a new majority of 3.
	l.start("n1", false)
	if err := n.Promote(context.Background(), "n4"); err != nil {
		t.Fatal(err)
	}
	if len(voters(n.fsm.view().Config)) != 4 {
		t.Fatal("promotion missing")
	}
	read(t, n, "key", "data")
	put(t, n, "resumed", "key", "new")
}

func TestRemovalSeedsRemainingVotersBeforeChangingMembership(t *testing.T) {
	l := newLab(t)
	n := l.grow(3)
	e := put(t, n, "data", "key", "payload")
	if err := n.Remove(context.Background(), "n2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := member(n.fsm.view().Config, "n2"); ok {
		t.Fatal("member still present")
	}
	for _, id := range []raft.ServerID{"n1", "n3"} {
		if err := l.nodes[id].objects.VerifyDurable(context.Background(), e.Ref); err != nil {
			t.Fatal(id, err)
		}
	}
	put(t, n, "after-remove", "key", "new")
	if err := n.Remove(context.Background(), n.id.Node); err == nil {
		t.Fatal("removed serving leader")
	}
}

func TestOnlineCatchUpDoesNotBlockWritesAndPromotionIncludesDelta(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	first := put(t, n, "first", "old", "first")
	l.start("n2", false)
	if err := n.AddLearner(context.Background(), "n2", "n2"); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	l.mu.Lock()
	l.gates["n2"] = gate
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.CatchUp(ctx, "n2") }()
	select {
	case <-l.entered:
	case <-ctx.Done():
		t.Fatal("catch-up did not start")
	}
	second := put(t, n, "second", "new", "during catch-up")
	read(t, n, "old", "first")
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := n.Promote(ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{first, second} {
		if err := l.nodes["n2"].objects.VerifyDurable(ctx, e.Ref); err != nil {
			t.Fatal("promotion omitted generation", err)
		}
	}
}

func TestNewLeaderFetchesBytesMissingFromItsAppliedLog(t *testing.T) {
	l := newLab(t)
	n := l.grow(3)
	l.mu.Lock()
	l.noData["n3"] = true
	l.mu.Unlock()
	e := put(t, n, "first", "key", "data on n1 and n2")
	if f, err := l.nodes["n3"].objects.Open(context.Background(), e.Ref); err == nil {
		f.Close()
		t.Fatal("expected metadata-only replica")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := wait(ctx, n.raft.LeadershipTransferToServer("n3", "n3")); err != nil {
		t.Fatal(err)
	}
	n = l.leader()
	if n.id.Node != "n3" {
		t.Fatalf("unexpected leader %s", n.id.Node)
	}
	read(t, n, "key", "data on n1 and n2")
	if err := n.objects.VerifyDurable(ctx, e.Ref); err != nil {
		t.Fatal("read did not hydrate leader", err)
	}
}

func TestCancelledCommitRetriesExactlyOnce(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	ref, err := n.objects.Stage(context.Background(), strings.NewReader("data"), 100)
	if err != nil {
		t.Fatal(err)
	}
	c := command{Kind: "mutate", ConfigIndex: n.fsm.control().ConfigIndex, Mutation: Mutation{ID: "retry", Key: "key", Ref: ref}, Copies: []raft.ServerID{"n1"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = n.apply(ctx, c)
	if err != nil && !errors.Is(err, ErrIndeterminate) {
		t.Fatal(err)
	}
	retried := put(t, n, "retry", "key", "data")
	again := put(t, n, "retry", "key", "data")
	if retried != again || len(n.fsm.view().Operations) != 1 {
		t.Fatal("ambiguous retry duplicated operation")
	}
}
