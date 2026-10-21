package store

import (
	"slices"
	"testing"
)

// A directory removed in batches leaves a tombstone per file. The commit that
// removes the directory itself must not read every one of them back.
func TestLiveSubtreeLeavesOutTombstones(t *testing.T) {
	s := newTestStore(t)
	for _, f := range []FileMeta{
		{Name: "dir", ModTime: 1, Hash: "file named like the directory"},
		{Name: "dir/live", ModTime: 1, Hash: "a"},
		{Name: "dir/sub/live", ModTime: 1, Hash: "b"},
		{Name: "dir/gone", ModTime: 1, Deleted: true},
		{Name: "dir/sub/gone", ModTime: 1, Deleted: true},
		{Name: "dir0/sibling", ModTime: 1, Hash: "c"},
		{Name: "dirx", ModTime: 1, Hash: "d"},
	} {
		if _, err := s.PutRemote(f); err != nil {
			t.Fatal(err)
		}
	}
	live, err := s.ListLiveSubtree("dir")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, meta := range live {
		names = append(names, meta.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"dir", "dir/live", "dir/sub/live"}) {
		t.Fatalf("live subtree: %v", names)
	}
	all, err := s.ListSubtree("dir")
	if err != nil || len(all) != 5 {
		t.Fatalf("full subtree: %d %v", len(all), err)
	}
}
