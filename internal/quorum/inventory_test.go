package quorum

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

type inventoryWire struct {
	wire
	copies    atomic.Int64
	calls     atomic.Int64
	restartAt int64
	malformed bool
}

func (w *inventoryWire) Receive(ctx context.Context, id raft.ServerID, ref generation.Ref, r io.Reader) error {
	w.copies.Add(1)
	return w.wire.Receive(ctx, id, ref, r)
}
func (w *inventoryWire) Inventory(ctx context.Context, id raft.ServerID, refs []generation.Ref) (generation.Inventory, error) {
	n, err := w.target(ctx, id)
	if err != nil {
		return generation.Inventory{}, err
	}
	out, err := n.LocalInventory(ctx, refs)
	count := w.calls.Add(1)
	if w.restartAt > 0 && count >= w.restartAt {
		out.Session = "simulated-restart"
	}
	if w.malformed {
		out.Missing = append(out.Missing, generation.Ref{Hash: strings.Repeat("f", 64), Size: 42})
	}
	return out, err
}
func TestPromotionTransfersOnlyCatchupDelta(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	ctx := context.Background()
	w := &inventoryWire{wire: wire{l, n.id.Node}}
	n.peers = w
	for i := 0; i < 8; i++ {
		put(t, n, fmt.Sprint(i), fmt.Sprint(i), fmt.Sprint(i))
	}
	l.start("n2", false)
	if err := n.AddLearner(ctx, "n2", "n2"); err != nil {
		t.Fatal(err)
	}
	if err := n.CatchUp(ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	if w.copies.Load() != 8 {
		t.Fatal("unexpected initial transfer count", w.copies.Load())
	}
	put(t, n, "delta", "delta", "new bytes")
	before := w.copies.Load()
	if err := n.Promote(ctx, "n2"); err != nil {
		t.Fatal(err)
	}
	if copied := w.copies.Load() - before; copied != 1 {
		t.Fatalf("promotion recopied %d objects, want delta 1", copied)
	}
}
func TestPromotionRejectsRestartedOrMalformedInventory(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			l := newLab(t)
			n := l.grow(1)
			ctx := context.Background()
			w := &inventoryWire{wire: wire{l, n.id.Node}}
			n.peers = w
			put(t, n, "one", "key", "one")
			l.start("n2", false)
			if err := n.AddLearner(ctx, "n2", "n2"); err != nil {
				t.Fatal(err)
			}
			if err := n.CatchUp(ctx, "n2"); err != nil {
				t.Fatal(err)
			}
			w.calls.Store(0)
			w.restartAt = 2
			w.malformed = malformed
			if err := n.Promote(ctx, "n2"); err == nil {
				t.Fatal("unsafe certification promoted learner")
			}
			m, _ := member(n.fsm.control().Config, "n2")
			if m.Suffrage != raft.Nonvoter {
				t.Fatal("membership changed")
			}
			if _, err := n.Delete(ctx, "blocked", "key"); !errors.Is(err, ErrTransition) {
				t.Fatal("freeze lost", err)
			}
			if err := n.CancelChange(ctx, "n2"); err != nil {
				t.Fatal(err)
			}
			w.restartAt = 0
			w.malformed = false
			if err := n.Promote(ctx, "n2"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
