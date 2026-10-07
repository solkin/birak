package quorum

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

type Record struct {
	Key   string
	Entry Entry
}
type Status struct {
	StorageError string
	BackupActive bool
	Replication  MaintenanceStatus
	Collection   *CollectionFence
	Maintenance  MaintenanceStatus
	Space        generation.Space
	SpaceError   string
	Identity     Identity
	State        string
	Leader       raft.ServerID
	Address      raft.ServerAddress
	Members      raft.Configuration
	Index        uint64
	Transition   string
	Voter        bool
}

func (n *Node) Identity() Identity { return n.id }
func (n *Node) Configuration() (raft.Configuration, error) {
	f := n.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return raft.Configuration{}, err
	}
	return f.Configuration(), nil
}
func (n *Node) Status() Status {
	addr, id := n.raft.LeaderWithID()
	s := n.fsm.control()
	status := Status{Identity: n.id, State: n.raft.State().String(), Leader: id, Address: addr, Members: s.Config, Index: s.Index, Voter: voters(s.Config)[n.id.Node]}
	if s.Transition != nil {
		status.Transition = s.Transition.Kind + ":" + string(s.Transition.ID)
	}
	status.Collection = s.Collection
	status.Maintenance = n.MaintenanceStatus()
	status.Replication = n.ReplicationStatus()
	status.BackupActive = n.exporting.Load()
	if e := n.objects.CheckStorage(); e != nil {
		status.StorageError = e.Error()
	}
	var err error
	status.Space, err = n.objects.Space()
	if err != nil {
		status.SpaceError = err.Error()
	}
	return status
}
func (n *Node) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if err := n.barrier(ctx); err != nil {
		return err
	}
	if n.fsm.control().Collection != nil {
		return ErrMaintenance
	}
	if n.fsm.control().Transition != nil {
		return ErrTransition
	}
	return n.objects.CheckStorage()
}
func (n *Node) TransferLeadership(ctx context.Context, id raft.ServerID) error {
	c, err := n.Configuration()
	if err != nil {
		return err
	}
	m, ok := member(c, id)
	if !ok || m.Suffrage != raft.Voter {
		return ErrMembership
	}
	return wait(ctx, n.raft.LeadershipTransferToServer(id, m.Address))
}

func (n *Node) Stage(ctx context.Context, r io.Reader, limit int64) (generation.Ref, error) {
	if err := n.Ready(ctx); err != nil {
		return generation.Ref{}, err
	}
	return n.objects.Stage(ctx, r, limit)
}
func (n *Node) Transact(ctx context.Context, id string, changes []Change, conditions []Condition) (Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if len(changes) == 0 {
		return Entry{}, errors.New("empty transaction")
	}
	return n.mutate(ctx, Mutation{ID: id, Changes: changes, Conditions: conditions})
}

// View copies matching live records at one committed boundary. Callers needing
// conditional writes must submit the returned key indices as conditions.
func (n *Node) View(ctx context.Context, prefixes ...string) ([]Record, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if err := n.barrier(ctx); err != nil {
		return nil, err
	}
	n.fsm.mu.RLock()
	var records []Record
	for key, e := range n.fsm.state.Entries {
		if e.Deleted {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				records = append(records, Record{key, e})
				break
			}
		}
	}
	n.fsm.mu.RUnlock()
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	return records, nil
}

func (n *Node) Lookup(ctx context.Context, key string) (Entry, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if err := n.barrier(ctx); err != nil {
		return Entry{}, false, err
	}
	e, ok := n.fsm.entry(key)
	return e, ok && !e.Deleted, nil
}

// Blob methods expose no mutable file path. The network adapter must authorize
// each request before calling these, including on reused TLS connections.
func (n *Node) ReceiveBlob(ctx context.Context, ref generation.Ref, r io.Reader) error {
	n.blobGate.RLock()
	defer n.blobGate.RUnlock()
	if n.fsm.control().Collection != nil {
		return ErrMaintenance
	}
	return n.objects.Repair(ctx, ref, r)
}
func (n *Node) LocalBlob(ctx context.Context, ref generation.Ref) (io.ReadCloser, error) {
	return n.objects.Open(ctx, ref)
}
func (n *Node) OpenBlob(ctx context.Context, ref generation.Ref) (io.ReadCloser, error) {
	return n.openGeneration(ctx, ref)
}

// TransactOnce uses application CAS instead of retaining an unbounded core
// receipt. An uncertain result must be reconciled through application state.
func (n *Node) TransactOnce(ctx context.Context, id string, changes []Change, conditions []Condition) (Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if len(changes) == 0 {
		return Entry{}, errors.New("empty transaction")
	}
	return n.mutate(ctx, Mutation{ID: id, Changes: changes, Conditions: conditions, NoReceipt: true})
}
func (n *Node) SetReserve(bytes uint64) { n.objects.SetReserve(bytes) }
