package fileops

// Opening a file for a write that does not truncate has to seed the staged
// generation from the published one. While that copy ran under the commit lock,
// appending to a large file stopped every other write on the volume for as long
// as the copy took.

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestSeedingDoesNotHoldTheCommitLock(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs a second processor to observe lock availability")
	}
	root := t.TempDir()
	blob := make([]byte, 64<<20)
	rand.Read(blob)
	target := filepath.Join(root, "large.bin")
	if err := os.WriteFile(target, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	var acquired atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			unlock := Lock(root)
			acquired.Add(1)
			unlock()
			time.Sleep(time.Millisecond)
		}
	}()

	f, commit, err := OpenWriter(root, target, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	seeded := acquired.Load()
	close(stop)
	<-done

	// A staged handle is positioned at the start; SFTP, its only caller, always
	// writes at an explicit offset. Extend the file the same way.
	if _, err := f.WriteAt([]byte("appended"), int64(len(blob))); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}

	// Copying 64 MiB takes far longer than a handful of lock round-trips at one
	// millisecond each. While the copy ran under the lock this was 0 or 1.
	if seeded < 5 {
		t.Fatalf("commit lock was available only %d times while seeding 64 MiB", seeded)
	}

	// And the staged generation really was seeded: the published file must be
	// the original bytes plus what was appended, not just the appended bytes.
	published, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(blob) + len("appended")); published.Size() != want {
		t.Fatalf("published size = %d, want %d", published.Size(), want)
	}
}

// A destination replaced while the copy was running must not be silently
// overwritten with a generation seeded from bytes that are already gone.
func TestSeedingLosesToAnExternalReplacement(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "contended.bin")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, commit, err := OpenWriter(root, target, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Something outside Birak replaces the file after it was staged.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(target, []byte("replaced by someone else"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("!"), 0)

	if err := commit(); err == nil {
		t.Fatal("a staged generation overwrote an external replacement")
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "replaced by someone else" {
		t.Fatalf("external write was lost: %q %v", body, err)
	}
}
