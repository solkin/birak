package watcher

// Deleting a directory through a gateway used to hold the volume's commit lock
// for the whole tree — five minutes for 300k files, during which nothing else
// on the node could be written. It is now a series of bounded commits.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

func gatewayWrite(t *testing.T, dir, path, body string) error {
	t.Helper()
	f, closeWriter, err := fileops.OpenWriter(dir, path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		return err
	}
	return closeWriter()
}

// indexDirectly records files the way a sweep would, without a sweep's fsync of
// every file and directory: thousands of files are set up in seconds.
func indexDirectly(t *testing.T, w *Watcher, prefix string, names []string) {
	t.Helper()
	for _, name := range names {
		name = prefix + name
		path := filepath.Join(w.dir, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if _, err := w.store.PutFile(name, info.ModTime().UnixNano(), info.Size(), hex.EncodeToString(sum[:]), false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemovingALargeTreeLetsOtherWritesThrough(t *testing.T) {
	w := auditWatcher(t)
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(w.dir, "tree")
	names := writeTree(t, tree, 3000)
	indexDirectly(t, w, "tree/", names)

	removed := make(chan error, 1)
	go func() { removed <- fileops.Remove(w.dir, tree, true) }()
	// Wait until the removal is under way.
	first := filepath.Join(tree, filepath.FromSlash(names[0]))
	for {
		if _, err := os.Lstat(first); os.IsNotExist(err) {
			break
		}
		select {
		case err := <-removed:
			t.Fatalf("removal finished before anything else could be tried: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}

	if err := gatewayWrite(t, w.dir, filepath.Join(w.dir, "other.txt"), "written meanwhile"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-removed:
		t.Fatalf("a small write waited for the whole tree to be removed (%v)", err)
	default:
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree still present: %v", err)
	}
	for _, name := range names {
		meta, err := w.store.GetFile("tree/" + name)
		if err != nil || meta == nil || !meta.Deleted {
			t.Fatalf("tree/%s not tombstoned: %+v %v", name, meta, err)
		}
	}
	if meta, err := w.store.GetFile("other.txt"); err != nil || meta == nil || meta.Deleted {
		t.Fatalf("the concurrent write was not indexed: %+v %v", meta, err)
	}
	if intents, err := w.store.LocalIntents(); err != nil || len(intents) != 0 {
		t.Fatalf("intents left behind: %v %v", intents, err)
	}
	// None of it counts as an external deletion.
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A removal that stops halfway leaves both halves consistent: what is gone
// is tombstoned, what is left is on disk and still indexed.
func TestInterruptedTreeRemovalLeavesAConsistentIndex(t *testing.T) {
	w := auditWatcher(t)
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(w.dir, "tree")
	gone := writeTree(t, filepath.Join(tree, "a"), 300)
	indexDirectly(t, w, "tree/a/", gone)
	kept := []string{"z/1", "z/2"}
	writeFiles(t, tree, kept...)
	indexDirectly(t, w, "tree/", kept)
	// Files in z cannot be unlinked, so the removal fails when it gets there.
	lockOut(t, filepath.Join(tree, "z"), 0o500)

	if err := fileops.Remove(w.dir, tree, true); err == nil {
		t.Fatal("a removal that could not finish reported success")
	}
	for _, name := range gone {
		meta, err := w.store.GetFile("tree/a/" + name)
		if err != nil || meta == nil || !meta.Deleted {
			t.Fatalf("removed tree/a/%s not tombstoned: %+v %v", name, meta, err)
		}
		if _, err := os.Lstat(filepath.Join(tree, "a", filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("tree/a/%s tombstoned but present: %v", name, err)
		}
	}
	assertLive(t, w, "tree/z/1", "tree/z/2")
	if intents, err := w.store.LocalIntents(); err != nil || len(intents) != 0 {
		t.Fatalf("intents left behind: %v %v", intents, err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatalf("index and disk disagree after the interrupted removal: %v", err)
	}
}
