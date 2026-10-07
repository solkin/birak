package generation

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRepairRevokesDamagedReadersAndDurabilityReceipts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ref, err := s.Stage(ctx, strings.NewReader("correct"), 7)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := s.Open(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	session := s.proofSession()
	if err := os.WriteFile(s.path(ref), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Repair(ctx, ref, strings.NewReader("invalid")); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if err := s.Receive(ctx, ref, strings.NewReader("correct")); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 2)); !errors.Is(err, ErrCorrupt) {
		t.Fatal("damaged reader not revoked", err)
	}
	if err := s.Repair(ctx, ref, strings.NewReader("correct")); err != nil {
		t.Fatal(err)
	}
	if session == s.proofSession() {
		t.Fatal("old receipt survived repair")
	}
	if _, err := reader.Seek(0, 0); !errors.Is(err, ErrCorrupt) {
		t.Fatal("old inode revived", err)
	}
	f, err := s.Open(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "correct" {
		t.Fatal(string(b), err)
	}
}

func TestRepairFlushFailureNeverAcknowledged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ref, _ := s.Stage(ctx, strings.NewReader("correct"), 7)
	if err := os.WriteFile(s.path(ref), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("directory fsync failed")
	s.syncDir = func(string) error { return injected }
	if err := s.Repair(ctx, ref, strings.NewReader("correct")); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	s.syncDir = SyncDir
	if err := s.Repair(ctx, ref, strings.NewReader("correct")); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionPreservesLiveRecentAndPinnedAndReportsPartialFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	refs := map[string]Ref{}
	old := time.Now().Add(-48 * time.Hour)
	for _, data := range []string{"live", "pinned", "old", "recent"} {
		ref, err := s.Stage(ctx, strings.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		refs[data] = ref
		if data != "recent" {
			if err := os.Chtimes(s.path(ref), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	f, err := s.Open(ctx, refs["pinned"])
	if err != nil {
		t.Fatal(err)
	}
	keep := map[string]Ref{refs["live"].Hash: refs["live"]}
	out, err := s.Collect(ctx, keep, time.Now().Add(-time.Hour))
	if err != nil || out.Files != 1 || out.Pinned != 1 || out.Recent != 1 {
		t.Fatal(out, err)
	}
	if _, err := s.Open(ctx, refs["old"]); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "pinned" {
		t.Fatal(string(b), err)
	}
	f.Close()
	s.syncDir = func(string) error { return errors.New("unlink flush failed") }
	out, err = s.Collect(ctx, keep, time.Now().Add(-time.Hour))
	if err == nil || out.Files != 1 {
		t.Fatal(out, err)
	}
	s.syncDir = SyncDir
	if _, err = s.Collect(ctx, keep, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestSpaceReserveRejectsBeforeReadingBody(t *testing.T) {
	s := testStore(t)
	s.SetReserve(^uint64(0))
	if _, err := s.Stage(context.Background(), panicReader{}, 100); !errors.Is(err, ErrNoSpace) {
		t.Fatal(err)
	}
	space, err := s.Space()
	if err != nil || space.Reserved != 0 {
		t.Fatal(space, err)
	}
	s.SetReserve(1)
	release, err := s.reserve(100)
	if err != nil {
		t.Fatal(err)
	}
	space, _ = s.Space()
	if space.Reserved != 100 {
		t.Fatal(space)
	}
	release()
	space, _ = s.Space()
	if space.Reserved != 0 {
		t.Fatal(space)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("body read without space") }
