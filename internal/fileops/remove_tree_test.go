package fileops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFlatTree(t *testing.T, root string, dirs, perDir int) {
	t.Helper()
	for d := range dirs {
		for f := range perDir {
			mustWrite(t, filepath.Join(root, fmt.Sprintf("d%02d", d), fmt.Sprintf("f%03d", f)), "x")
		}
	}
}

// Something written into a directory the removal has already walked is kept,
// and the removal says so instead of reporting a clean delete.
func TestTreeRemovalKeepsWhatIsWrittenIntoIt(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	writeFlatTree(t, tree, 3, 100)
	late := filepath.Join(tree, "d00", "late")
	batches := 0
	removeTreeBetweenBatches = func() {
		batches++
		if batches == 1 {
			// The walk has listed d00 and is part way through it.
			mustWrite(t, late, "written while the tree was being removed")
		}
	}
	t.Cleanup(func() { removeTreeBetweenBatches = nil })

	err := Remove(root, tree, true)
	if !errors.Is(err, ErrTreeChanged) {
		t.Fatalf("removal of a tree that gained a file: %v", err)
	}
	wantBody(t, late, "written while the tree was being removed")
	entries, err := os.ReadDir(tree)
	if err != nil || len(entries) != 1 || entries[0].Name() != "d00" {
		t.Fatalf("everything but the new file's directory should be gone: %v %v", entries, err)
	}
	if batches < 2 {
		t.Fatalf("a 300-file tree was removed in %d batch(es)", batches)
	}
}

// The lock is let go between batches: counted, not timed, so the test means
// the same on any machine.
func TestTreeRemovalReleasesTheLockBetweenBatches(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	writeFlatTree(t, tree, 20, 100)
	before := LockStats(root)
	if err := Remove(root, tree, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree still present: %v", err)
	}
	// One acquisition per batch of at most removeBatchMax files, plus the
	// up-front writer check and the commit that removes the directories.
	if got, want := LockStats(root).Acquisitions-before.Acquisitions, int64(2000/removeBatchMax+2); got < want {
		t.Fatalf("removing 2000 files took the lock %d times, want at least %d", got, want)
	}
}

func TestTreeRemovalRefusesATreeWithAWriter(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	writeFlatTree(t, tree, 2, 10)
	f, _, err := OpenWriter(root, filepath.Join(tree, "d01", "f000"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { AbortWriter(root, f) })
	if err := Remove(root, tree, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("removal under an open writer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tree, "d00", "f000")); err != nil {
		t.Fatalf("a refused removal deleted files: %v", err)
	}
}

// A writer that opens a file after the removal began keeps that file; the rest
// of the tree is still removed, and the removal reports the file as busy.
func TestTreeRemovalKeepsAFileOpenedForWritingMeanwhile(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	writeFlatTree(t, tree, 3, 100)
	target := filepath.Join(tree, "d02", "f050")
	var writer *os.File
	removeTreeBetweenBatches = func() {
		if writer == nil {
			f, _, err := OpenWriter(root, target, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			writer = f
		}
	}
	t.Cleanup(func() {
		removeTreeBetweenBatches = nil
		if writer != nil {
			AbortWriter(root, writer)
		}
	})
	if err := Remove(root, tree, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("removal with a writer that started meanwhile: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the file being written was removed: %v", err)
	}
	for _, gone := range []string{filepath.Join(tree, "d00"), filepath.Join(tree, "d01"), filepath.Join(tree, "d02", "f049")} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived the removal: %v", gone, err)
		}
	}
}

func TestTreeRemovalOfMissingPathMatchesRemoveAll(t *testing.T) {
	root := t.TempDir()
	if err := Remove(root, filepath.Join(root, "absent"), true); err != nil {
		t.Fatalf("recursive removal of a missing path: %v", err)
	}
	mustWrite(t, filepath.Join(root, "file"), "x")
	if err := Remove(root, filepath.Join(root, "file"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "file")); !os.IsNotExist(err) {
		t.Fatalf("file survived: %v", err)
	}
}

func TestRemoveBatchSizeTracksTheTarget(t *testing.T) {
	for _, tc := range []struct {
		size int
		held time.Duration
		want int
	}{
		{64, 10 * time.Millisecond, 128},       // fast: grow, at most doubling
		{64, removeBatchTarget, 64},            // on target: stay
		{64, 200 * time.Millisecond, 32},       // slow: shrink in proportion
		{64, 10 * time.Second, removeBatchMin}, // very slow: floor
		{512, time.Microsecond, removeBatchMax},
	} {
		if got := nextRemoveBatch(tc.size, tc.held); got != tc.want {
			t.Errorf("nextRemoveBatch(%d, %v)=%d, want %d", tc.size, tc.held, got, tc.want)
		}
	}
}
