package quorum

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/hashicorp/raft"
)

// AddLearner admits a unique non-voting identity. It receives the Raft log but
// cannot lead or acknowledge client operations. Raft and byte transports must
// already have authenticated the same node identity at the supplied address.
func (n *Node) AddLearner(ctx context.Context, id raft.ServerID, address raft.ServerAddress) error {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if id == "" || len(id) > 128 || !utf8.ValidString(string(id)) || address == "" || len(address) > 512 || !utf8.ValidString(string(address)) {
		return errors.New("invalid learner identity or address")
	}
	if err := n.lock(ctx); err != nil {
		return err
	}
	defer n.unlock()
	if err := n.barrier(ctx); err != nil {
		return err
	}
	if err := n.checkPeer(ctx, id); err != nil {
		return err
	}
	s := n.fsm.control()
	if s.Transition != nil {
		return ErrTransition
	}
	for _, m := range s.Config.Servers {
		if m.ID == id {
			if m.Address == address {
				return nil
			}
			return errors.New("node ID already has another address")
		}
		if m.Address == address {
			return errors.New("address already belongs to another node")
		}
	}
	if err := wait(ctx, n.raft.AddNonvoter(id, address, s.ConfigIndex, n.timeout)); err != nil {
		return errors.Join(ErrIndeterminate, err)
	}
	return n.barrier(ctx)
}

// CatchUp seeds a learner while ordinary writes continue. This is preparation,
// not a readiness certificate: promotion rechecks the final frozen boundary.
func (n *Node) CatchUp(ctx context.Context, id raft.ServerID) error {
	if err := n.barrier(ctx); err != nil {
		return err
	}
	s := n.fsm.catchupState()
	if _, ok := member(s.Config, id); !ok {
		return ErrMembership
	}
	return n.seed(ctx, s, id)
}

func (n *Node) seed(ctx context.Context, s state, id raft.ServerID) error {
	if p, ok := n.peers.(inventoryPeers); ok {
		return n.seedIncremental(ctx, s, id, p)
	}
	for _, ref := range s.Generations {
		if err := n.copyTo(ctx, id, ref); err != nil {
			return err
		}
	}
	return nil
}

// Promote freezes writes durably, copies every retained generation to the
// learner, then changes Raft membership and thaws. Thus increasing N also
// increases durable copy counts where the new majority requires it. The freeze
// survives leadership changes; a new leader can resume with the same call.
// Transports with process-bound inventory certification copy only the final
// delta after CatchUp; other transports conservatively recopy all generations.
func (n *Node) Promote(ctx context.Context, id raft.ServerID) error {
	return n.change(ctx, "promote", id)
}

// Remove conservatively requires every remaining voter to hold every retained
// generation before removing a member. It never shrinks membership merely
// because a peer is unreachable. Transfer leadership before removing yourself.
func (n *Node) Remove(ctx context.Context, id raft.ServerID) error {
	return n.change(ctx, "remove", id)
}

func (n *Node) change(ctx context.Context, kind string, id raft.ServerID) error {
	// Large catch-up is governed by the caller's deadline, not the short client
	// write deadline. Each consensus request still has an enqueue timeout.
	if err := n.lock(ctx); err != nil {
		return err
	}
	defer n.unlock()
	if err := n.barrier(ctx); err != nil {
		return err
	}
	s := n.fsm.catchupState()
	if kind == "remove" && id == n.id.Node {
		return errors.New("transfer leadership before removing the leader")
	}
	m, exists := member(s.Config, id)
	var change transition
	if s.Transition != nil {
		if s.Transition.ID != id || s.Transition.Kind != kind {
			return ErrTransition
		}
		change = *s.Transition
	} else {
		if kind == "remove" && !exists {
			return nil
		}
		if !exists {
			return ErrMembership
		}
		if kind == "promote" && m.Suffrage == raft.Voter {
			return nil
		}
		change = transition{Kind: kind, ID: id, Address: m.Address}
		if _, err := n.apply(ctx, command{Kind: "freeze", ConfigIndex: s.ConfigIndex, Change: &change}); err != nil {
			return err
		}
		s = n.fsm.catchupState()
	}
	if kind == "promote" {
		if err := n.seed(ctx, s, id); err != nil {
			return err
		}
		if !exists || m.Suffrage != raft.Voter {
			if err := wait(ctx, n.raft.AddVoter(id, change.Address, s.ConfigIndex, n.timeout)); err != nil {
				return errors.Join(ErrIndeterminate, err)
			}
		}
	} else {
		for peer := range voters(s.Config) {
			if peer != id {
				if err := n.seed(ctx, s, peer); err != nil {
					return err
				}
			}
		}
		if exists {
			if err := wait(ctx, n.raft.RemoveServer(id, s.ConfigIndex, n.timeout)); err != nil {
				return errors.Join(ErrIndeterminate, err)
			}
		}
	}
	if err := n.barrier(ctx); err != nil {
		return err
	}
	s = n.fsm.control()
	_, err := n.apply(ctx, command{Kind: "thaw", ConfigIndex: s.ConfigIndex, Change: &change})
	return err
}

// CancelChange releases a failed catch-up freeze only if the actual membership
// change has not committed. Once it has, Promote/Remove must finish the recorded
// transition. An unreachable learner therefore need not stop writes forever.
func (n *Node) CancelChange(ctx context.Context, id raft.ServerID) error {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	if err := n.lock(ctx); err != nil {
		return err
	}
	defer n.unlock()
	if err := n.barrier(ctx); err != nil {
		return err
	}
	s := n.fsm.control()
	if s.Transition == nil {
		return nil
	}
	if s.Transition.ID != id {
		return ErrTransition
	}
	_, err := n.apply(ctx, command{Kind: "abort", ConfigIndex: s.ConfigIndex, Change: s.Transition})
	return err
}
