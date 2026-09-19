package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNamespaceTombstoneHasExactlyWinningRank(t *testing.T) {
	states := []FileMeta{
		{Name: "a", Clock: 100, ModTime: 50, Hash: "aaa"},
		{Name: "a/b", Clock: 100, ModTime: 50, Hash: "bbb"},
		{Name: "a", Clock: 100, ModTime: 50, Hash: "ccc"},
		{Name: "a/b", Clock: 101, ModTime: 1, Hash: "aaa"},
	}
	for i := range states {
		for j := range states {
			if !NamespaceConflict(states[i].Name, states[j].Name) || CompareState(&states[i], &states[j]) <= 0 {
				continue
			}
			tombstone := Superseded(states[j].Name, states[i])
			if CompareState(&tombstone, &states[i]) != 0 {
				t.Fatalf("resolution inflated winning rank: %+v %+v", tombstone, states[i])
			}
			if CompareState(&tombstone, &states[j]) <= 0 {
				t.Fatal("resolution did not suppress loser")
			}
			for _, newer := range states {
				if newer.Name == tombstone.Name && CompareState(&newer, &states[i]) > 0 && CompareState(&newer, &tombstone) <= 0 {
					t.Fatal("resolution suppressed newer third-node write")
				}
			}
		}
	}
	parent := FileMeta{Name: "a", Clock: 1, ModTime: 1, Hash: "same"}
	child := parent
	child.Name = "a/b"
	if CompareState(&child, &parent) <= 0 {
		t.Fatal("equal states need a deterministic namespace tie-break")
	}
}

func TestConflictMetadataSurvivesStoreAndWire(t *testing.T) {
	s := newTestStore(t)
	winner := FileMeta{Name: "a/b", Clock: 200, ModTime: 100, Hash: strings.Repeat("a", 64), Size: 7}
	for _, meta := range []FileMeta{Superseded("a", winner), ConflictCopy(winner)} {
		if _, err := s.PutRemote(meta); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetFile(meta.Name)
		if err != nil || got == nil {
			t.Fatalf("get: %+v %v", got, err)
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip FileMeta
		if err := json.Unmarshal(data, &roundtrip); err != nil {
			t.Fatal(err)
		}
		if CompareState(&roundtrip, &meta) != 0 || roundtrip.SupersededBy != meta.SupersededBy || roundtrip.ConflictOf != meta.ConflictOf {
			t.Fatalf("lost fields: %+v", roundtrip)
		}
	}
}

func TestLocalMutationAdvancesPastObservedNamespace(t *testing.T) {
	s := newTestStore(t)
	for _, meta := range []FileMeta{
		{Name: "a/deep/child", Clock: 500, ModTime: 400, Hash: "old"},
		{Name: "ab/unrelated", Clock: 9000, ModTime: 9000, Hash: "unrelated"},
	} {
		if _, err := s.PutRemote(meta); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutLocal(FileMeta{Name: "a", ModTime: 1, Hash: "replacement"}); err != nil {
		t.Fatal(err)
	}
	parent, err := s.GetFile("a")
	if err != nil || parent.Clock != 501 {
		t.Fatalf("old-mtime parent did not supersede observed tree: %+v %v", parent, err)
	}
	if _, err := s.PutLocal(FileMeta{Name: "a/new", ModTime: 1, Hash: "new child"}); err != nil {
		t.Fatal(err)
	}
	child, err := s.GetFile("a/new")
	if err != nil || child.Clock != 502 {
		t.Fatalf("child did not supersede observed parent: %+v %v", child, err)
	}
}

func TestUnicodeNamespaceSubtree(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"日本😀", "日本😀/子/孫", "日本😀/別", "日本😀別/子", "日本😀0/子"} {
		if _, err := s.PutRemote(FileMeta{Name: name, Clock: 100, ModTime: 1, Hash: "body"}); err != nil {
			t.Fatal(err)
		}
	}
	files, err := s.ListSubtree("日本😀")
	if err != nil || len(files) != 3 {
		t.Fatalf("Unicode subtree: %+v %v", files, err)
	}
	if _, err := s.PutLocal(FileMeta{Name: "日本😀", ModTime: 1, Hash: "replacement"}); err != nil {
		t.Fatal(err)
	}
	meta, err := s.GetFile("日本😀")
	if err != nil || meta.Clock != 101 {
		t.Fatalf("Unicode namespace clock: %+v %v", meta, err)
	}
}
