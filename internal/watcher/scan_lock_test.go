package watcher

// The periodic scan reads every byte it owns. While it held the shared commit
// lock to do so, a gateway write waited for the largest file in the tree. These
// tests pin both halves of the fix: the lock stays available during a scan, and
// bytes read outside it are still verified before they are indexed.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

func TestScanDoesNotHoldTheCommitLockWhileHashing(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs a second processor to observe lock availability")
	}
	w := auditWatcher(t)

	// Enough bytes that hashing dominates the scan by orders of magnitude.
	blob := make([]byte, 8<<20)
	rand.Read(blob)
	for i := 0; i < 16; i++ {
		if err := os.WriteFile(filepath.Join(w.dir, fmt.Sprintf("big-%02d.bin", i)), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	acquired := make(chan int, 1)
	go func() {
		count := 0
		for {
			select {
			case <-stop:
				acquired <- count
				return
			default:
			}
			unlock := fileops.Lock(w.dir)
			count++
			unlock()
			time.Sleep(time.Millisecond)
		}
	}()

	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(stop)
	got := <-acquired

	// 128 MiB of hashing takes far longer than 50 lock round-trips at 1ms each.
	// While hashing ran under the lock this number was in the single digits.
	if got < 50 {
		t.Fatalf("commit lock was available only %d times while scanning 128 MiB", got)
	}
}

// Bytes read outside the lock are trusted only while the file's generation is
// unchanged. A file rewritten between the read and the commit must be re-read,
// never indexed with the hash of the generation that is already gone.
func TestPrecomputedHashIsRejectedAfterTheFileChanges(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "rewritten.bin")
	if err := os.WriteFile(path, []byte("first generation"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := &hashed{info: info, hash: hex.EncodeToString(sha256Of("first generation"))}

	// The file is replaced before the hint is used, exactly as a gateway commit
	// would replace it between the scan's read and the scan's commit.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("second generation, different bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	gotInfo, gotHash, err := w.snapshot(path, stale)
	if err != nil {
		t.Fatal(err)
	}
	if gotHash == stale.hash {
		t.Fatal("a hash from a replaced generation was reused")
	}
	if want := hex.EncodeToString(sha256Of("second generation, different bytes")); gotHash != want {
		t.Fatalf("hash = %s, want the current bytes %s", gotHash, want)
	}
	if gotInfo.Size() != int64(len("second generation, different bytes")) {
		t.Fatalf("stat came from the wrong generation: %d bytes", gotInfo.Size())
	}
}

// An unchanged file must reuse the bytes already read, or the work moved out of
// the lock would simply be done twice.
func TestPrecomputedHashIsReusedWhenNothingChanged(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "stable.bin")
	if err := os.WriteFile(path, []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A deliberately wrong hash proves reuse: if the file were read again, the
	// real hash of "stable" would come back instead.
	hint := &hashed{info: info, hash: "sentinel"}
	_, gotHash, err := w.snapshot(path, hint)
	if err != nil {
		t.Fatal(err)
	}
	if gotHash != "sentinel" {
		t.Fatalf("hash = %q; the file was read again despite being unchanged", gotHash)
	}
}

func sha256Of(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
