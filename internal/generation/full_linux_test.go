//go:build linux

package generation

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestRealENOSPCKeepsAcknowledgedGeneration(t *testing.T) {
	volume := os.Getenv("BIRAK_TEST_QUORUM_FULL_VOLUME")
	if volume == "" {
		t.Skip("requires separate bounded tmpfs BIRAK_TEST_QUORUM_FULL_VOLUME")
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(volume, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Type != unix.TMPFS_MAGIC || uint64(stat.Bsize)*stat.Blocks > 64<<20 {
		t.Fatal("test volume must be tmpfs <=64 MiB")
	}
	dir, err := os.MkdirTemp(volume, "generations-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ref, err := s.Stage(context.Background(), strings.NewReader("already acknowledged"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(context.Background(), zeros{}, 128<<20); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("expected actual ENOSPC, got %v", err)
	}
	if err := s.VerifyDurable(context.Background(), ref); err != nil {
		t.Fatal("lost previous generation", err)
	}
	if _, err := s.Stage(context.Background(), strings.NewReader("retry after space reclaimed"), 100); err != nil {
		t.Fatal(err)
	}
}
