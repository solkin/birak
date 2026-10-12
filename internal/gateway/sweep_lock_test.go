package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

// The hourly sweep reads every directory of the volume. While it held the
// commit lock for that walk, every write on the node waited for it.
func TestSweepDoesNotHoldTheCommitLockWhileWalking(t *testing.T) {
	root := t.TempDir()
	for d := 0; d < 40; d++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", d))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 250; f++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d", f)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	stale := filepath.Join(root, "d39", ".birak-tmp-orphan")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	// Commit from another goroutine for the whole sweep and record the longest
	// wait for the lock.
	var walked time.Duration
	var worst time.Duration
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
			began := time.Now()
			unlock := fileops.Lock(root)
			if wait := time.Since(began); wait > worst {
				worst = wait
			}
			time.Sleep(time.Millisecond)
			unlock()
		}
	}()
	began := time.Now()
	SweepTempFiles(root, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	walked = time.Since(began)
	close(stop)
	<-done

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale scratch survived: %v", err)
	}
	// The walk over 10k entries takes far longer than one removal. A sweep that
	// held the lock for all of it made a commit wait for the whole walk.
	if worst > walked/2 && walked > 20*time.Millisecond {
		t.Fatalf("a commit waited %v during a %v sweep", worst, walked)
	}
}

// Under the lock, a candidate is checked again: a scratch file that became
// active, or was replaced by something fresh, after the walk saw it survives.
func TestSweepRechecksCandidatesUnderTheLock(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(root, ".birak-tmp-reused")
	if err := os.WriteFile(path, []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour)
	stale := func(info os.FileInfo) bool { return !info.ModTime().After(cutoff) }
	// The walk judged it stale; by the time the lock is held it is fresh.
	if err := removeScratch(root, path, false, stale, logger); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fresh scratch removed: %v", err)
	}
	// A directory where the walk saw a file is not removed as one.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(path, old, old)
	if err := removeScratch(root, path, false, stale, logger); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("directory removed as a scratch file: %v", err)
	}
}
