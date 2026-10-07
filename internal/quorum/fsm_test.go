package quorum

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

func TestMembershipProofModel(t *testing.T) {
	// Exercise odd/even and much larger populations; duplicate receipts and
	// non-voters must never change the quorum denominator or numerator.
	for size := 1; size <= 33; size++ {
		f := newMachine("test")
		cfg := raft.Configuration{}
		for i := 0; i < size; i++ {
			cfg.Servers = append(cfg.Servers, raft.Server{ID: raft.ServerID(fmt.Sprint(i)), Address: raft.ServerAddress(fmt.Sprint(i)), Suffrage: raft.Voter})
		}
		cfg.Servers = append(cfg.Servers, raft.Server{ID: "learner", Address: "learner", Suffrage: raft.Nonvoter})
		f.StoreConfiguration(1, cfg)
		for count := 0; count <= size; count++ {
			c := command{Format: Format, Cluster: "test", Kind: "mutate", ConfigIndex: 1, Mutation: Mutation{ID: fmt.Sprint(count), Key: "key", Ref: generation.Ref{Hash: strings.Repeat("a", 64), Size: 1}}, Copies: []raft.ServerID{"learner", "unknown"}}
			for i := 0; i < count; i++ {
				c.Copies = append(c.Copies, raft.ServerID(fmt.Sprint(i)), raft.ServerID(fmt.Sprint(i)))
			}
			b, _ := encodeCommand(c)
			result := f.Apply(&raft.Log{Index: uint64(count + 2), Data: b})
			_, ack := result.(Entry)
			if ack != (count >= size/2+1) {
				t.Fatalf("N=%d copies=%d result=%v", size, count, result)
			}
		}
	}
}

func TestStaleConfigurationAndOperationIDFence(t *testing.T) {
	f := newMachine("test")
	f.StoreConfiguration(5, raft.Configuration{Servers: []raft.Server{{ID: "n1", Address: "n1", Suffrage: raft.Voter}}})
	c := command{Format: Format, Cluster: "test", Kind: "mutate", ConfigIndex: 4, Mutation: Mutation{ID: "same", Key: "key", Delete: true}}
	apply := func() interface{} { b, _ := encodeCommand(c); return f.Apply(&raft.Log{Index: 10, Data: b}) }
	if result := apply(); result != ErrMembership {
		t.Fatal(result)
	}
	c.ConfigIndex = 5
	if _, ok := apply().(Entry); !ok {
		t.Fatal("valid mutation rejected")
	}
	c.Mutation.Key = "other"
	if result := apply(); result != ErrOperationID {
		t.Fatal(result)
	}
	c.Mutation.Key = "key"
	c.ConfigIndex = 4
	if _, ok := apply().(Entry); !ok {
		t.Fatal("exact retry lost original receipt")
	}
}

func TestSnapshotFlushFailurePreservesPreviousRecoveryPoint(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	put(t, n, "one", "key", "one")
	if err := n.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := newSnapshots(l.dirs["n1"])
	if err != nil {
		t.Fatal(err)
	}
	previous, err := s.List()
	if err != nil || len(previous) == 0 {
		t.Fatal(previous, err)
	}
	s.syncDir = func(path string) error {
		for _, name := range []string{"state.bin", "meta.json"} {
			if _, err := os.Stat(filepath.Join(path, name)); err != nil {
				t.Error(err)
			}
		}
		return syscall.EIO
	}
	state := n.fsm.view()
	snap, err := s.Create(1, state.Index+1, 100, state.Config, state.ConfigIndex, l.trans["n1"])
	if err != nil {
		t.Fatal(err)
	}
	if err := (&snapshot{state}).Persist(snap); !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	now, err := s.List()
	if err != nil || len(now) != len(previous) || now[0].ID != previous[0].ID {
		t.Fatalf("lost recovery point: %+v %v", now, err)
	}
}

func TestRestoreRejectsIncompleteStateWithoutMutation(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	put(t, n, "one", "key", "one")
	s := n.fsm.view()
	delete(s.Generations, n.fsm.view().Entries["key"].Ref.Hash)
	b, _ := json.Marshal(s)
	if err := n.fsm.Restore(io.NopCloser(bytes.NewReader(b))); err == nil {
		t.Fatal("accepted dangling snapshot reference")
	}
	read(t, n, "key", "one")
}

func TestRestartRejectsLostStateAndIdentityChange(t *testing.T) {
	for _, missing := range []string{"raft.db", "generations/identity.json", "generations/objects", "snapshots"} {
		t.Run(missing, func(t *testing.T) {
			l := newLab(t)
			l.grow(1)
			l.stop("n1")
			if err := os.RemoveAll(filepath.Join(l.dirs["n1"], missing)); err != nil {
				t.Fatal(err)
			}
			_, tr := raft.NewInmemTransport("n1")
			defer tr.Close()
			if n, err := Open(Options{Dir: l.dirs["n1"], Identity: Identity{Cluster: "test-cluster", Node: "n1", Format: Format}, Transport: tr, Peers: wire{l, "n1"}}); err == nil {
				n.Close()
				t.Fatal("adopted lost state")
			}
		})
	}
	l := newLab(t)
	l.grow(1)
	l.stop("n1")
	for _, identity := range []Identity{{Cluster: "other", Node: "n1", Format: Format}, {Cluster: "test-cluster", Node: "other", Format: Format}} {
		_, tr := raft.NewInmemTransport("n1")
		if n, err := Open(Options{Dir: l.dirs["n1"], Identity: identity, Transport: tr, Peers: wire{l, "n1"}}); err == nil {
			n.Close()
			t.Fatal("adopted different identity")
		}
		tr.Close()
	}
	_, tr := raft.NewInmemTransport("n1")
	defer tr.Close()
	if n, err := Open(Options{Dir: l.dirs["n1"], Identity: Identity{Cluster: "test-cluster", Node: "n1", Format: Format}, Bootstrap: true, Transport: tr, Peers: wire{l, "n1"}}); err == nil {
		n.Close()
		t.Fatal("rebootstrapped existing cluster")
	}
}

func TestFailedPromotionCanBeCancelledBeforeMembershipChange(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	put(t, n, "first", "key", "payload")
	l.start("n2", false)
	if err := n.AddLearner(context.Background(), "n2", "n2"); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.noData["n2"] = true
	l.mu.Unlock()
	if err := n.Promote(context.Background(), "n2"); err == nil {
		t.Fatal("promoted without files")
	}
	if err := n.CancelChange(context.Background(), "n2"); err != nil {
		t.Fatal(err)
	}
	put(t, n, "resume", "key", "new")
	if len(voters(n.fsm.view().Config)) != 1 {
		t.Fatal("changed membership")
	}
}
