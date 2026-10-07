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
	"sync"
	"testing"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

func TestTransactionRequiresEveryGenerationAndPublishesAtomically(t *testing.T) {
	f := newMachine("test")
	f.StoreConfiguration(1, raft.Configuration{Servers: []raft.Server{{ID: "a", Address: "a", Suffrage: raft.Voter}, {ID: "b", Address: "b", Suffrage: raft.Voter}, {ID: "c", Address: "c", Suffrage: raft.Voter}}})
	one := generation.Ref{Hash: strings.Repeat("a", 64), Size: 1}
	two := generation.Ref{Hash: strings.Repeat("b", 64), Size: 2}
	c := command{Format: Format, Cluster: "test", Kind: "mutate", ConfigIndex: 1, Timestamp: 42, Mutation: Mutation{ID: "atomic", Changes: []Change{{Key: "object", Ref: one, Attributes: Attributes{ETag: "tag", ContentType: "application/test"}}, {Key: "part", Ref: two}, {Key: "upload", MetaOnly: true, Attributes: Attributes{Value: "record"}}}}, Proofs: map[string][]raft.ServerID{one.Hash: {"a", "b"}, two.Hash: {"a", "a", "outsider"}}}
	apply := func() any {
		b, err := encodeCommand(c)
		if err != nil {
			t.Fatal(err)
		}
		return f.Apply(&raft.Log{Index: 2, Data: b})
	}
	if result := apply(); result != ErrDataQuorum {
		t.Fatal(result)
	}
	if len(f.view().Entries) != 0 || len(f.view().Generations) != 0 || len(f.view().Operations) != 0 {
		t.Fatal("partial transaction escaped")
	}
	c.Proofs[two.Hash] = []raft.ServerID{"b", "c"}
	if _, ok := apply().(Entry); !ok {
		t.Fatal("valid transaction rejected")
	}
	snapshot, _ := json.Marshal(f.view())
	restored := newMachine("test")
	if err := restored.Restore(io.NopCloser(bytes.NewReader(snapshot))); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"object", "part", "upload"} {
		e, ok := restored.entry(key)
		if !ok || e.Index != 2 || e.Modified != 42 {
			t.Fatal(key, e)
		}
	}
	c.Mutation.Changes[1].Ref = one
	c.Mutation.Changes[1].Ref.Size++
	if err := c.Mutation.validate(); err == nil {
		t.Fatal("accepted inconsistent size for same hash")
	}
}

func TestConditionalTransactionRaceRetryAndRestart(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	ctx := context.Background()
	original, err := n.Transact(ctx, "init", []Change{{Key: "upload", MetaOnly: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := n.Stage(ctx, strings.NewReader("payload"), 100)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, e := n.Transact(ctx, id, []Change{{Key: id, Ref: ref}, {Key: "upload", Delete: true}}, []Condition{{Key: "upload", Index: original.Index}})
			out <- e
		}(id)
	}
	wg.Wait()
	close(out)
	success, failed := 0, 0
	for err := range out {
		if err == nil {
			success++
		} else if errors.Is(err, ErrCondition) {
			failed++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || failed != 1 {
		t.Fatalf("winners=%d conflicts=%d", success, failed)
	}
	if err := n.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	l.stop("n1")
	n = l.start("n1", false)
	n = l.leader()
	records, err := n.View(ctx, "a", "b", "upload")
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	winner := records[0].Key
	retry, err := n.Transact(ctx, winner, []Change{{Key: winner, Ref: ref}, {Key: "upload", Delete: true}}, []Condition{{Key: "upload", Index: original.Index}})
	if err != nil || retry != records[0].Entry {
		t.Fatal("lost original receipt", retry, err)
	}
	if _, err := n.Transact(ctx, "guard", []Change{{Key: "bucket", Delete: true}}, []Condition{{EmptyPrefix: winner}}); !errors.Is(err, ErrCondition) {
		t.Fatal(err)
	}
}

func TestReplacedVolumeFencesMetadataOnlyRequests(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	ctx := context.Background()
	if _, err := n.Transact(ctx, "create", []Change{{Key: "bucket", MetaOnly: true}}, nil); err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(l.dirs["n1"], "generations", "objects")
	if err := os.Rename(objects, objects+"-detached"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(objects, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Transact(ctx, "delete", []Change{{Key: "bucket", Delete: true}}, nil); err == nil {
		t.Fatal("metadata ACK on replaced volume")
	}
	if _, _, err := n.Lookup(ctx, "bucket"); err == nil {
		t.Fatal("read on replaced volume")
	}
	e, exists := n.fsm.entry("bucket")
	if !exists || e.Deleted {
		t.Fatal("fenced deletion applied")
	}
}
