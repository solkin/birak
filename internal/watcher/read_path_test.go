package watcher

// A GET used to take the volume's commit lock twice, and to re-validate the
// storage sentinel and stat every quarantined name in between. These tests pin
// the replacement: reads and probes never wait for a commit, and a quarantine
// still fences every descriptor of the damaged generation even though the two
// no longer serialize.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

func indexedFile(t *testing.T, w *Watcher, name, body string) string {
	t.Helper()
	path := filepath.Join(w.dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh(name); err != nil {
		t.Fatal(err)
	}
	return path
}

// within fails the test if fn does not return before the deadline. A read that
// still took the commit lock would block here for as long as the test holds it.
func within(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s waited for the commit lock", what)
		return nil
	}
}

func TestReadsAndProbesDoNotWaitForTheCommitLock(t *testing.T) {
	w := auditWatcher(t)
	path := indexedFile(t, w, "icon.png", "icon bytes")

	unlock := fileops.Lock(w.dir)
	defer unlock()
	err := within(t, "open/read/close", func() error {
		r, err := fileops.OpenReader(w.dir, path)
		if err != nil {
			return err
		}
		if _, err := r.Read(make([]byte, 4)); err != nil {
			return err
		}
		return r.Close()
	})
	if err != nil {
		t.Fatalf("read while a commit holds the lock: %v", err)
	}
	if err := within(t, "storage probe", w.ProbeStorage); err != nil {
		t.Fatalf("probe while a commit holds the lock: %v", err)
	}

	// The probe still sees the volume go away while the lock is held: the
	// binding is checked without the lock, not taken on trust.
	if err := os.Remove(filepath.Join(w.dir, ".birak", "storage-id")); err != nil {
		t.Fatal(err)
	}
	if err := within(t, "storage probe", w.ProbeStorage); err == nil {
		t.Fatal("probe reported a volume without its sentinel as bound")
	}
}

// A failed full validation fences reads until a full validation succeeds; a
// busy lock is no reason to start serving again.
func TestFailedValidationFencesReadsWhileTheLockIsBusy(t *testing.T) {
	w := auditWatcher(t)
	path := indexedFile(t, w, "file", "bytes")
	marker := filepath.Join(w.dir, ".birak", "storage-id")
	saved, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("another volume"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.CheckStorage(); err == nil {
		t.Fatal("fixture: foreign sentinel accepted")
	}
	// Put the right sentinel back, but keep the lock busy: nothing may
	// revalidate, so the recorded failure stands.
	if err := os.WriteFile(marker, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock := fileops.Lock(w.dir)
	err = within(t, "open", func() error {
		r, err := fileops.OpenReader(w.dir, path)
		if err == nil {
			r.Close()
		}
		return err
	})
	unlock()
	if err == nil {
		t.Fatal("read served after a failed storage validation")
	}
	// Once the lock is free the next read revalidates and recovers.
	r, err := fileops.OpenReader(w.dir, path)
	if err != nil {
		t.Fatalf("read after the volume was restored: %v", err)
	}
	r.Close()
}

// Readers and a quarantine race freely now. Whatever the interleaving, once the
// quarantine has returned no descriptor of the damaged generation — opened
// before, during or after it, by name or by alias — may deliver a byte.
func TestQuarantineRacingReadersNeverLeaksDamagedBytes(t *testing.T) {
	for round := 0; round < 20; round++ {
		w := auditWatcher(t)
		path := indexedFile(t, w, "file", "original")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(w.dir, "alias")
		if err := os.Link(path, alias); err != nil {
			t.Fatal(err)
		}
		// Same size and timestamp, different bytes: the signature of damage.
		if err := os.WriteFile(path, []byte("CORRUPT!"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}

		var (
			mu     sync.Mutex
			opened []*fileops.Reader
			stop   = make(chan struct{})
			wg     sync.WaitGroup
		)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(target string) {
				defer wg.Done()
				for count := 0; count < 128; {
					select {
					case <-stop:
						return
					default:
					}
					r, err := fileops.OpenReader(w.dir, target)
					if err != nil {
						continue
					}
					mu.Lock()
					opened = append(opened, r)
					mu.Unlock()
					count++
				}
			}([]string{path, alias}[i%2])
		}
		if err := w.Refresh("file"); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("round %d: quarantine: %v", round, err)
		}
		close(stop)
		wg.Wait()
		for _, r := range opened {
			if n, err := r.ReadAt(make([]byte, 8), 0); n != 0 || !errors.Is(err, fileops.ErrUnreadable) {
				t.Fatalf("round %d: a descriptor of the damaged generation read %d bytes: %v", round, n, err)
			}
			r.Close()
		}
		for _, target := range []string{path, alias} {
			if r, err := fileops.OpenReader(w.dir, target); !errors.Is(err, fileops.ErrUnreadable) {
				if r != nil {
					r.Close()
				}
				t.Fatalf("round %d: opened %s after quarantine: %v", round, target, err)
			}
		}
	}
}

// With many names quarantined, a read compares against memory: the index is
// built once per quarantine change, not stat by stat on every GET.
func TestQuarantineAliasCheckDoesNotStatPerRead(t *testing.T) {
	w := auditWatcher(t)
	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("damaged-%03d", i)
		if err := os.WriteFile(filepath.Join(w.dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := w.store.SetQuarantined(name, "key-"+name); err != nil {
			t.Fatal(err)
		}
	}
	path := indexedFile(t, w, "healthy", "healthy bytes")
	read := func() {
		t.Helper()
		r, err := fileops.OpenReader(w.dir, path)
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
	read()
	built := w.quarantined.snapshot.Load()
	for i := 0; i < 100; i++ {
		read()
	}
	if w.quarantined.snapshot.Load() != built && time.Since(built.built) < quarantineIndexTTL {
		t.Fatal("the quarantine index was rebuilt although nothing changed")
	}

	// A new quarantine is seen by the very next read, through an alias too.
	alias := filepath.Join(w.dir, "alias-of-healthy")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := w.store.SetQuarantined("healthy", "key-healthy"); err != nil {
		t.Fatal(err)
	}
	if r, err := fileops.OpenReader(w.dir, alias); !errors.Is(err, fileops.ErrUnreadable) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("alias of a newly quarantined name opened: %v", err)
	}
}
