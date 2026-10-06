package watcher

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/fileops"
)

func TestQuarantineRevokesOpenReadersAndAliases(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"hard", "symbolic"} {
		var err error
		if alias == "hard" {
			err = os.Link(path, filepath.Join(w.dir, alias))
		} else {
			err = os.Symlink(path, filepath.Join(w.dir, alias))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var readers []*fileops.Reader
	for _, name := range []string{"file", "hard", "symbolic"} {
		r, err := fileops.OpenReader(w.dir, filepath.Join(w.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		readers = append(readers, r)
	}
	if err := os.WriteFile(path, []byte("CORRUPT!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("file"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("quarantine: %v", err)
	}
	for _, name := range []string{"file", "hard", "symbolic"} {
		if r, err := fileops.OpenReader(w.dir, filepath.Join(w.dir, name)); !errors.Is(err, fileops.ErrUnreadable) {
			if r != nil {
				r.Close()
			}
			t.Fatalf("opened quarantined alias %s: %v", name, err)
		}
	}
	// A repaired path must not rehabilitate descriptors naming its old inode.
	scratch := filepath.Join(w.dir, ".birak-tmp-repair")
	if err := os.WriteFile(scratch, []byte("repaired"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fileops.Publish(w.dir, scratch, path); err != nil {
		t.Fatal(err)
	}
	if w.NeedsRepair("file") {
		t.Fatal("repair did not clear quarantine")
	}
	if r, err := fileops.OpenReader(w.dir, filepath.Join(w.dir, "hard")); !errors.Is(err, fileops.ErrUnreadable) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("hard link reopened the displaced damaged inode: %v", err)
	}
	if err := w.Refresh("hard"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("indexed damaged inode as a new alias: %v", err)
	}
	for _, r := range readers {
		if n, err := r.ReadAt(make([]byte, 8), 0); n != 0 || !errors.Is(err, fileops.ErrUnreadable) {
			t.Fatalf("old descriptor leaked bytes after repair: %d %v", n, err)
		}
	}
	fresh, err := fileops.OpenReader(w.dir, path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	body, err := io.ReadAll(fresh)
	if err != nil || string(body) != "repaired" {
		t.Fatalf("fresh read: %q %v", body, err)
	}
}
