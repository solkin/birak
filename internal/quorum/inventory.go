package quorum

import (
	"context"
	"errors"
	"sort"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

type inventoryPeers interface {
	Inventory(context.Context, raft.ServerID, []generation.Ref) (generation.Inventory, error)
}

func (n *Node) LocalInventory(ctx context.Context, refs []generation.Ref) (generation.Inventory, error) {
	return n.objects.Inventory(ctx, refs)
}

// Final receipts all belong to one live target process/storage epoch. The
// metadata freeze fixes the reference set, and changes to a cached receipt or a
// process restart invalidate the operation before Raft membership can change.
func (n *Node) seedIncremental(ctx context.Context, s state, id raft.ServerID, p inventoryPeers) error {
	refs := make([]generation.Ref, 0, len(s.Generations))
	for _, ref := range s.Generations {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Hash < refs[j].Hash })
	session := ""
	inventory := func(batch []generation.Ref) ([]generation.Ref, error) {
		var out generation.Inventory
		var err error
		if id == n.id.Node {
			out, err = n.LocalInventory(ctx, batch)
		} else {
			out, err = p.Inventory(ctx, id, batch)
		}
		if err != nil {
			return nil, err
		}
		if out.Session == "" || len(out.Session) > 128 {
			return nil, errors.New("invalid certification session")
		}
		if session == "" {
			session = out.Session
		} else if session != out.Session {
			return nil, errors.New("peer restarted or revoked durability receipts; retry catch-up")
		}
		expected := map[generation.Ref]bool{}
		for _, ref := range batch {
			expected[ref] = true
		}
		for _, ref := range out.Missing {
			if !expected[ref] {
				return nil, errors.New("invalid missing generation response")
			}
			delete(expected, ref)
		}
		return out.Missing, nil
	}
	// Empty inventories also check the storage fence and authenticate a session.
	if len(refs) == 0 {
		_, err := inventory(nil)
		return err
	}
	for offset := 0; offset < len(refs); offset += generation.InventoryBatch {
		batch := refs[offset:min(offset+generation.InventoryBatch, len(refs))]
		missing, err := inventory(batch)
		if err != nil {
			return err
		}
		for _, ref := range missing {
			if err := n.copyTo(ctx, id, ref); err != nil {
				return err
			}
		}
	}
	for offset := 0; offset < len(refs); offset += generation.InventoryBatch {
		missing, err := inventory(refs[offset:min(offset+generation.InventoryBatch, len(refs))])
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return errors.New("peer lost a generation during catch-up")
		}
	}
	return nil
}
