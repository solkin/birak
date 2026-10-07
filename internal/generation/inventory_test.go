package generation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestInventoryDurabilityCacheRestartAndInvalidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("strict directory durability requires NTFS validation")
	}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "store")
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ref, err := s.Stage(ctx, strings.NewReader("durable"), 100)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Inventory(ctx, []Ref{ref})
	if err != nil || len(first.Missing) != 0 {
		t.Fatal(first, err)
	}
	s.syncFile = func(*os.File) error { return syscall.EIO }
	if _, err = s.Inventory(ctx, []Ref{ref}); err != nil {
		t.Fatal("warm inventory needlessly re-read/re-flushed data", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.syncFile = func(*os.File) error { return syscall.EIO }
	if _, err = s.Inventory(ctx, []Ref{ref}); !errors.Is(err, syscall.EIO) {
		t.Fatal("restart accepted unverified disk presence", err)
	}
	s.syncFile = (*os.File).Sync
	restarted, err := s.Inventory(ctx, []Ref{ref})
	if err != nil || restarted.Session == first.Session || len(restarted.Missing) != 0 {
		t.Fatal(restarted, err)
	}
	if err = os.Remove(s.path(ref)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Inventory(ctx, []Ref{ref}); err == nil {
		t.Fatal("lost certified file did not revoke current session")
	}
	missing, err := s.Inventory(ctx, []Ref{ref})
	if err != nil || missing.Session == restarted.Session || len(missing.Missing) != 1 {
		t.Fatal(missing, err)
	}
	if err = s.Receive(ctx, ref, strings.NewReader("durable")); err != nil {
		t.Fatal(err)
	}
	repaired, err := s.Inventory(ctx, []Ref{ref})
	if err != nil || len(repaired.Missing) != 0 {
		t.Fatal(repaired, err)
	}
	// Same-size damage is detected on read and revokes the cached receipt.
	info, err := os.Stat(s.path(ref))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.path(ref), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(s.path(ref), info.ModTime(), info.ModTime())
	if f, err := s.Open(ctx, ref); err == nil {
		f.Close()
		t.Fatal("accepted corrupt generation")
	}
	damaged, err := s.Inventory(ctx, []Ref{ref})
	if err != nil || damaged.Session == repaired.Session || len(damaged.Missing) != 1 {
		t.Fatal(damaged, err)
	}
}
