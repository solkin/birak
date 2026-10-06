package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("strict directory durability is not yet supported on Windows")
	}
	s, err := New(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func refFor(data string) Ref {
	h := sha256.Sum256([]byte(data))
	return Ref{hex.EncodeToString(h[:]), int64(len(data))}
}

func TestDurableImmutableRestart(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, err := s.Stage(ctx, strings.NewReader("first"), 100)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Open(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = s.Stage(ctx, strings.NewReader("second"), 100); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "first" {
		t.Fatalf("old reader %q %v", b, err)
	}
	if _, err = New(s.dir); err == nil {
		t.Fatal("second owner admitted")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r, err := reopened.Open(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err = io.ReadAll(r)
	if err != nil || string(b) != "first" {
		t.Fatalf("restart %q %v", b, err)
	}
}

func TestReceiveRejectsCorruptionAndLimits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ref := refFor("payload")
	for _, data := range []string{"payloae", "payloa", "payload extra"} {
		if err := s.Receive(ctx, ref, strings.NewReader(data)); err == nil {
			t.Fatalf("accepted %q", data)
		}
		if _, err := s.Open(ctx, ref); !os.IsNotExist(err) {
			t.Fatalf("published damaged generation: %v", err)
		}
	}
	for _, ref := range []Ref{{Hash: "../../etc/passwd"}, {Hash: strings.ToUpper(ref.Hash), Size: 7}, {Hash: ref.Hash, Size: -1}} {
		if _, err := s.Open(ctx, ref); err == nil {
			t.Fatal("accepted invalid ref")
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Stage(ctx, strings.NewReader("payload"), 100); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestFailedFlushNeverReturnsReceipt(t *testing.T) {
	for _, boundary := range []string{"file", "directory"} {
		t.Run(boundary, func(t *testing.T) {
			s := testStore(t)
			ref := refFor("payload")
			ctx := context.Background()
			if boundary == "file" {
				s.syncFile = func(*os.File) error { return syscall.ENOSPC }
			} else {
				s.syncDir = func(string) error { return syscall.EIO }
			}
			if err := s.Receive(ctx, ref, strings.NewReader("payload")); err == nil {
				t.Fatal("acknowledged failed flush")
			}
			if err := s.VerifyDurable(ctx, ref); err == nil {
				t.Fatal("acknowledged partial installation")
			}
			s.syncFile = (*os.File).Sync
			s.syncDir = SyncDir
			if err := s.Receive(ctx, ref, strings.NewReader("payload")); err != nil {
				t.Fatal(err)
			}
			if err := s.VerifyDurable(ctx, ref); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentDedupAndCorruptExisting(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 1<<20)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if _, err := s.Stage(ctx, bytes.NewReader(data), int64(len(data))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	h := sha256.Sum256(data)
	ref := Ref{hex.EncodeToString(h[:]), int64(len(data))}
	if err := os.WriteFile(s.path(ref), bytes.Repeat([]byte("y"), len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, ref); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if err := s.Receive(ctx, ref, bytes.NewReader(data)); !errors.Is(err, ErrCorrupt) {
		t.Fatal("silently replaced published generation", err)
	}
}

func TestRunningStoreFencesReplacedVolume(t *testing.T) {
	for _, sub := range []string{"", "objects", "staging"} {
		t.Run(sub, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			ref, err := s.Stage(ctx, strings.NewReader("old"), 100)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.dir, sub)
			if err := os.Rename(path, path+"-detached"); err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(path + "-detached")
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Stage(ctx, strings.NewReader("new"), 100); err == nil {
				t.Fatal("accepted replaced storage")
			}
			if err := s.VerifyDurable(ctx, ref); err == nil {
				t.Fatal("receipted displaced generation")
			}
			if f, err := s.Open(ctx, ref); err == nil {
				f.Close()
				t.Fatal("read through storage fence")
			}
		})
	}
}
