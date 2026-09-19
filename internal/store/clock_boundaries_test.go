package store

import (
	"fmt"
	"testing"
)

func TestLocalClockAdvancesAcrossEpochBoundary(t *testing.T) {
	for _, initial := range []int64{-100, -1, 0, 1} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			s := newTestStore(t)
			// Cover imported pre-clock records as well as new locally indexed files.
			if _, err := s.PutRemote(FileMeta{Name: "file", ModTime: initial, Hash: "initial"}); err != nil {
				t.Fatal(err)
			}
			previous, err := s.GetFile("file")
			if err != nil {
				t.Fatal(err)
			}
			for _, next := range []FileMeta{
				{Name: "file", ModTime: initial - 100, Hash: "rollback"},
				{Name: "file", ModTime: initial - 100, Deleted: true},
				{Name: "file", ModTime: initial - 200, Hash: "recreated"},
			} {
				if _, err := s.PutLocal(next); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetFile("file")
				if err != nil {
					t.Fatal(err)
				}
				if got.Clock == 0 || CompareState(got, previous) <= 0 {
					t.Fatalf("local mutation went backwards: before=%+v after=%+v", previous, got)
				}
				previous = got
			}
		})
	}
}
