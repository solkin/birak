// Package quorum provides durable majority writes for birakd quorum mode.
// The embedding transport authenticates node/cluster identity and encrypts
// both Raft metadata and immutable generation traffic.
package quorum

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

var ErrIndeterminate = errors.New("commit outcome unknown; retry with the same operation ID")

type Identity struct {
	Cluster string
	Node    raft.ServerID
	Format  string
}

// Peers transports immutable generations, independently of Raft's small log
// entries. Receive must verify identity, hash, size and durable storage before
// returning nil. Implementations must honor cancellation and never redirect
// credentials. A process with a lost/replaced data directory must be fenced and
// re-admitted, never continue under an old voting identity.
type Peers interface {
	Identity(context.Context, raft.ServerID) (Identity, error)
	Receive(context.Context, raft.ServerID, generation.Ref, io.Reader) error
	Open(context.Context, raft.ServerID, generation.Ref) (io.ReadCloser, error)
}

type Options struct {
	Dir        string
	Identity   Identity
	Bootstrap  bool // explicit, once, on a fresh state directory only
	Transport  raft.Transport
	Peers      Peers
	RaftConfig *raft.Config
	Timeout    time.Duration
}

type Node struct {
	id        Identity
	raft      *raft.Raft
	objects   *generation.Store
	fsm       *machine
	log       *raftboltdb.BoltStore
	peers     Peers
	lease     func() error
	ops       chan struct{}
	timeout   time.Duration
	closeOnce sync.Once
	closeErr  error
}

func Open(o Options) (_ *Node, err error) {
	if o.Identity.Cluster == "" || len(o.Identity.Cluster) > 128 || !utf8.ValidString(o.Identity.Cluster) || o.Identity.Node == "" || len(o.Identity.Node) > 128 || !utf8.ValidString(string(o.Identity.Node)) || o.Identity.Format != Format || o.Transport == nil || o.Peers == nil {
		return nil, errors.New("invalid quorum options")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if err := generation.MakeDir(o.Dir); err != nil {
		return nil, err
	}
	lease, err := fileops.AcquireLease(o.Dir)
	if err != nil {
		return nil, err
	}
	n := &Node{id: o.Identity, peers: o.Peers, lease: lease, ops: make(chan struct{}, 1), timeout: o.Timeout, fsm: newMachine(o.Identity.Cluster)}
	defer func() {
		if err != nil {
			n.Close()
		}
	}()
	identityPath := filepath.Join(o.Dir, "identity.json")
	b, readErr := os.ReadFile(identityPath)
	if readErr == nil {
		var saved Identity
		if json.Unmarshal(b, &saved) != nil || saved != o.Identity {
			return nil, errors.New("state belongs to another node, cluster or format")
		}
		if o.Bootstrap {
			return nil, errors.New("bootstrap requires a fresh state directory")
		}
		// Missing state is data loss, never an invitation to manufacture a new
		// Raft term/vote history or recreate an empty voting disk in place.
		for _, p := range []string{"raft.db", "generations", "generations/objects", "generations/staging", "snapshots"} {
			if _, e := os.Lstat(filepath.Join(o.Dir, p)); e != nil {
				return nil, fmt.Errorf("incomplete node state (%s); re-admission required: %w", p, e)
			}
		}
		paired, e := os.ReadFile(filepath.Join(o.Dir, "generations", "identity.json"))
		if e != nil || string(paired) != string(b) {
			return nil, errors.New("generation volume identity missing or mismatched")
		}
	} else if !os.IsNotExist(readErr) {
		return nil, readErr
	} else {
		entries, e := os.ReadDir(o.Dir)
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			if entry.Name() != "daemon.lock" {
				return nil, errors.New("refusing to adopt unrecognized quorum state")
			}
		}
		b, _ = json.Marshal(o.Identity)
		f, e := os.OpenFile(identityPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(b)
		if e == nil {
			e = f.Sync()
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			return nil, e
		}
		if e = generation.SyncDir(o.Dir); e != nil {
			return nil, e
		}
	}
	n.objects, err = generation.New(filepath.Join(o.Dir, "generations"))
	if err != nil {
		return nil, err
	}
	if os.IsNotExist(readErr) {
		f, e := os.OpenFile(filepath.Join(o.Dir, "generations", "identity.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(b)
		if e == nil {
			e = f.Sync()
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			return nil, e
		}
		if e = generation.SyncDir(filepath.Join(o.Dir, "generations")); e != nil {
			return nil, e
		}
	}
	n.log, err = raftboltdb.NewBoltStore(filepath.Join(o.Dir, "raft.db"))
	if err != nil {
		return nil, err
	}
	if err = generation.SyncDir(o.Dir); err != nil {
		return nil, err
	}
	snapshots, e := newSnapshots(o.Dir)
	if e != nil {
		return nil, e
	}
	cfg := raft.DefaultConfig()
	if o.RaftConfig != nil {
		copy := *o.RaftConfig
		cfg = &copy
	}
	cfg.LocalID = o.Identity.Node
	cfg.NoSnapshotRestoreOnStart = false
	cfg.ShutdownOnRemove = true
	if o.Bootstrap {
		err = raft.BootstrapCluster(cfg, n.log, n.log, snapshots, o.Transport, raft.Configuration{Servers: []raft.Server{{ID: o.Identity.Node, Address: o.Transport.LocalAddr(), Suffrage: raft.Voter}}})
		if err != nil {
			return nil, err
		}
	}
	n.raft, err = raft.NewRaft(cfg, n.fsm, n.log, n.log, snapshots, o.Transport)
	if err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		if n.raft != nil {
			n.closeErr = errors.Join(n.closeErr, n.raft.Shutdown().Error())
		}
		if n.log != nil {
			n.closeErr = errors.Join(n.closeErr, n.log.Close())
		}
		if n.objects != nil {
			n.closeErr = errors.Join(n.closeErr, n.objects.Close())
		}
		if n.lease != nil {
			n.closeErr = errors.Join(n.closeErr, n.lease())
		}
	})
	return n.closeErr
}

func (n *Node) lock(ctx context.Context) error {
	select {
	case n.ops <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (n *Node) unlock() { <-n.ops }

func wait(ctx context.Context, f raft.Future) error {
	done := make(chan error, 1)
	go func() { done <- f.Error() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Barrier commits through the current voting configuration, so an isolated
// former leader cannot return a successful write or a stale linearizable read.
func (n *Node) barrier(ctx context.Context) error {
	if err := n.objects.CheckStorage(); err != nil {
		return err
	}
	if err := wait(ctx, n.raft.Barrier(n.timeout)); err != nil {
		return err
	}
	return n.objects.CheckStorage()
}

func (n *Node) apply(ctx context.Context, c command) (interface{}, error) {
	c.Format = Format
	c.Cluster = n.id.Cluster
	b, err := encodeCommand(c)
	if err != nil {
		return nil, err
	}
	f := n.raft.Apply(b, n.timeout)
	if err = wait(ctx, f); err != nil {
		return nil, errors.Join(ErrIndeterminate, err)
	}
	result := f.Response()
	if err, ok := result.(error); ok {
		return nil, err
	}
	return result, nil
}

// Put acknowledges only after exact immutable bytes exist on a majority of
// current voters AND the corresponding metadata command has committed/applied.
func (n *Node) Put(ctx context.Context, id, key string, r io.Reader, maxBytes int64) (Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	// Reject followers and invalid requests before accepting large uploads.
	if err := (Mutation{ID: id, Key: key, Delete: true}).validate(); err != nil {
		return Entry{}, err
	}
	if err := n.barrier(ctx); err != nil {
		return Entry{}, err
	}
	ref, err := n.objects.Stage(ctx, r, maxBytes)
	if err != nil {
		return Entry{}, err
	}
	return n.mutate(ctx, Mutation{ID: id, Key: key, Ref: ref})
}

func (n *Node) Delete(ctx context.Context, id, key string) (Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	return n.mutate(ctx, Mutation{ID: id, Key: key, Delete: true})
}

func (n *Node) mutate(ctx context.Context, m Mutation) (Entry, error) {
	if err := m.validate(); err != nil {
		return Entry{}, err
	}
	if err := n.lock(ctx); err != nil {
		return Entry{}, err
	}
	defer n.unlock()
	if err := n.barrier(ctx); err != nil {
		return Entry{}, err
	}
	s := n.fsm.control()
	if old, ok := n.fsm.operation(m.ID); ok {
		if old.Fingerprint != m.fingerprint() {
			return Entry{}, ErrOperationID
		}
		return old.Entry, nil
	}
	if s.Transition != nil {
		return Entry{}, ErrTransition
	}
	n.fsm.mu.RLock()
	err := checkConditions(n.fsm.state.Entries, m.Conditions)
	n.fsm.mu.RUnlock()
	if err != nil {
		return Entry{}, err
	}
	c := command{Kind: "mutate", ConfigIndex: s.ConfigIndex, Mutation: m, Timestamp: time.Now().UnixNano(), Proofs: make(map[string][]raft.ServerID)}
	for _, change := range m.changes() {
		if change.Delete || change.MetaOnly {
			continue
		}
		if _, ok := c.Proofs[change.Ref.Hash]; ok {
			continue
		}
		copies, err := n.replicate(ctx, s.Config, change.Ref)
		if err != nil {
			return Entry{}, err
		}
		c.Proofs[change.Ref.Hash] = copies
		if len(m.Changes) == 0 {
			c.Copies = copies
		}
	}
	result, err := n.apply(ctx, c)
	if err != nil {
		return Entry{}, err
	}
	return result.(Entry), nil
}

func (n *Node) replicate(ctx context.Context, config raft.Configuration, ref generation.Ref) ([]raft.ServerID, error) {
	v := voters(config)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		id  raft.ServerID
		err error
	}
	done := make(chan result, len(v))
	for id := range v {
		go func() { done <- result{id, n.copyTo(ctx, id, ref)} }()
	}
	var copies []raft.ServerID
	var failures error
	for left := len(v); left > 0 && len(copies) < len(v)/2+1; left-- {
		select {
		case result := <-done:
			if result.err == nil {
				copies = append(copies, result.id)
			} else {
				failures = errors.Join(failures, result.err)
			}
		case <-ctx.Done():
			return nil, errors.Join(ErrDataQuorum, ctx.Err(), failures)
		}
	}
	if len(v) == 0 || len(copies) < len(v)/2+1 {
		return nil, errors.Join(ErrDataQuorum, failures)
	}
	return copies, nil
}

func (n *Node) checkPeer(ctx context.Context, id raft.ServerID) error {
	peer, err := n.peers.Identity(ctx, id)
	if err != nil {
		return err
	}
	if peer != (Identity{Cluster: n.id.Cluster, Node: id, Format: Format}) {
		return errors.New("peer identity mismatch")
	}
	return nil
}

func (n *Node) openGeneration(ctx context.Context, ref generation.Ref) (io.ReadCloser, error) {
	if f, err := n.objects.Open(ctx, ref); err == nil {
		return f, nil
	}
	// The new leader may have the metadata but not the data. Fetch and verify
	// from surviving members; never confuse an applied log with local readiness.
	s := n.fsm.control()
	var failures error
	for _, peer := range s.Config.Servers {
		if peer.ID == n.id.Node {
			continue
		}
		if err := n.checkPeer(ctx, peer.ID); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		f, err := n.peers.Open(ctx, peer.ID, ref)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		err = n.objects.Receive(ctx, ref, f)
		err = errors.Join(err, f.Close())
		if err == nil {
			return n.objects.Open(ctx, ref)
		}
		failures = errors.Join(failures, err)
	}
	return nil, fmt.Errorf("generation %s unavailable: %w", ref.Hash, failures)
}

func (n *Node) copyTo(ctx context.Context, id raft.ServerID, ref generation.Ref) error {
	f, err := n.openGeneration(ctx, ref)
	if err != nil {
		return err
	}
	defer f.Close()
	if id == n.id.Node {
		return n.objects.VerifyDurable(ctx, ref)
	}
	if err = n.checkPeer(ctx, id); err != nil {
		return err
	}
	return n.peers.Receive(ctx, id, ref, f)
}

// Read is linearizable at the metadata boundary and uses the selected immutable
// generation even if the key is concurrently overwritten or deleted.
func (n *Node) Read(ctx context.Context, key string) (Entry, io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if err := n.barrier(ctx); err != nil {
		return Entry{}, nil, err
	}
	e, ok := n.fsm.entry(key)
	if !ok || e.Deleted {
		return e, nil, os.ErrNotExist
	}
	f, err := n.openGeneration(ctx, e.Ref)
	return e, f, err
}

// Snapshot persists metadata and deduplication state. Generation bytes are
// deliberately retained separately, including overwritten/deleted generations.
func (n *Node) Snapshot(ctx context.Context) error { return wait(ctx, n.raft.Snapshot()) }
