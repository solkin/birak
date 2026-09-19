package watcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
)

func TestGatewayPreservedAttributesAreExplicitMutation(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "file")
	stamp := time.Now().Add(-time.Hour)
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, stamp, stamp)
	if err := w.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	old, _ := w.store.GetFile("file")
	tmp := filepath.Join(w.dir, ".birak-tmp-upload")
	if err := os.WriteFile(tmp, []byte("AFTER!"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(tmp, stamp, stamp)
	if err := fileops.Publish(w.dir, tmp, path); err != nil {
		t.Fatal(err)
	}
	meta, _ := w.store.GetFile("file")
	hash, _ := hashFile(path)
	if meta.Hash != hash || meta.StateClock() <= old.StateClock() || meta.ModTime != old.ModTime || w.NeedsRepair("file") {
		t.Fatalf("acknowledged mutation not indexed: old=%+v current=%+v", old, meta)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := w.store.GetFile("file")
	if after.Version != meta.Version {
		t.Fatal("rescan invented another generation")
	}
}
func TestLocalIntentRecoveryAfterRestart(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-filesystem-commit", true: "after-filesystem-commit"}[changed], func(t *testing.T) {
			w := auditWatcher(t)
			path := filepath.Join(w.dir, "file")
			stamp := time.Now().Add(-time.Hour)
			os.WriteFile(path, []byte("before"), 0o600)
			os.Chtimes(path, stamp, stamp)
			if err := w.Refresh("file"); err != nil {
				t.Fatal(err)
			}
			old, _ := w.store.GetFile("file")
			if err := w.store.BeginLocal([]string{"file"}); err != nil {
				t.Fatal(err)
			}
			if changed {
				os.WriteFile(path, []byte("AFTER!"), 0o600)
				os.Chtimes(path, stamp, stamp)
			}
			// Discard watcher memory; the mutation intent is stored in SQLite.
			restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
			if err := restarted.CheckStorage(); err != nil {
				t.Fatal(err)
			}
			meta, _ := w.store.GetFile("file")
			want, _ := hashFile(path)
			if meta.Hash != want || changed && meta.StateClock() <= old.StateClock() || !changed && meta.Version != old.Version {
				t.Fatalf("recovery old=%+v new=%+v", old, meta)
			}
			pending, err := w.store.LocalIntents()
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending intents: %v %v", pending, err)
			}
		})
	}
}
func TestReplicaIntentRetainsRemoteClock(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		for _, published := range []bool{false, true} {
			t.Run(fmt.Sprintf("deleted=%v/published=%v", deleted, published), func(t *testing.T) {
				w := auditWatcher(t)
				path := filepath.Join(w.dir, "file")
				stamp := time.Now().Add(-time.Hour)
				os.WriteFile(path, []byte("before"), 0o600)
				os.Chtimes(path, stamp, stamp)
				if err := w.Refresh("file"); err != nil {
					t.Fatal(err)
				}
				old, _ := w.store.GetFile("file")
				meta := store.FileMeta{Name: "file", ModTime: stamp.UnixNano(), Size: 6, Clock: old.StateClock() + 500, Deleted: deleted}
				tmp := filepath.Join(w.dir, ".birak-tmp-replica")
				os.WriteFile(tmp, []byte("AFTER!"), 0o600)
				os.Chtimes(tmp, stamp, stamp)
				meta.Hash, _ = hashFile(tmp)
				if deleted {
					meta.Hash = ""
					meta.Size = 0
				}
				if err := w.store.StageReplica(meta); err != nil {
					t.Fatal(err)
				}
				if published {
					if deleted {
						os.Remove(path)
					} else {
						os.Rename(tmp, path)
					}
				}
				restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
				if err := restarted.Refresh("file"); err != nil {
					t.Fatal(err)
				}
				got, _ := w.store.GetFile("file")
				want := old
				if published {
					want = &meta
				}
				if store.CompareState(got, want) != 0 {
					t.Fatalf("remote state changed on recovery: got=%+v want=%+v", got, want)
				}
				pending, err := w.store.ReplicaIntent("file")
				if err != nil || pending != nil {
					t.Fatalf("pending replica: %+v %v", pending, err)
				}
			})
		}
	}
}

func TestReplicaDeletionIntentBesideDirectory(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "file")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := store.FileMeta{Name: "file", Deleted: true, ModTime: 100, Clock: 500}
	if err := w.store.StageReplica(meta); err != nil {
		t.Fatal(err)
	}
	restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
	if err := restarted.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	got, err := w.store.GetFile("file")
	if err != nil || got == nil || store.CompareState(got, &meta) != 0 {
		t.Fatalf("lost directory-adjacent tombstone on recovery: %+v %v", got, err)
	}
	if body, err := os.ReadFile(filepath.Join(path, "child")); err != nil || string(body) != "keep" {
		t.Fatalf("child changed: %q %v", body, err)
	}
	if pending, err := w.store.ReplicaIntent("file"); err != nil || pending != nil {
		t.Fatalf("intent not cleared: %+v %v", pending, err)
	}
}
func TestGatewayRefusesMutationWhenMetadataUnavailable(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "file")
	os.WriteFile(path, []byte("old"), 0o600)
	if err := w.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	w.store.Close()
	err := fileops.Remove(w.dir, path, false)
	if err == nil {
		t.Fatal("acknowledged deletion with unavailable metadata")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("mutated before recording intent: %v", err)
	}
}
func TestExternalRollbackOfMtimeStillAdvancesClock(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "file")
	stamp := time.Now()
	os.WriteFile(path, []byte("before"), 0o600)
	os.Chtimes(path, stamp, stamp)
	if err := w.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	old, _ := w.store.GetFile("file")
	os.WriteFile(path, []byte("after"), 0o600)
	os.Chtimes(path, stamp.Add(-time.Hour), stamp.Add(-time.Hour))
	if err := w.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	got, _ := w.store.GetFile("file")
	if store.CompareState(got, old) <= 0 {
		t.Fatalf("mtime rollback lost mutation: old=%+v new=%+v", old, got)
	}
	if errors.Is(w.Refresh("file"), ErrIntegrity) {
		t.Fatal("ordinary timestamped write quarantined")
	}
}

func TestUnmappedLegacyBackupBlocksDeletion(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "file")
	os.WriteFile(path, []byte("valuable"), 0o600)
	if err := w.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(w.dir, ".birak-bak-legacy")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
	if err := restarted.Refresh("file"); err == nil {
		t.Fatal("legacy backup was treated as a deletion")
	}
	meta, _ := w.store.GetFile("file")
	if meta.Deleted {
		t.Fatal("broadcast deletion over recoverable data")
	}
	body, err := os.ReadFile(backup)
	if err != nil || string(body) != "valuable" {
		t.Fatalf("backup lost: %q %v", body, err)
	}
}

func TestRecoveryDoesNotTraverseIgnoredDirectory(t *testing.T) {
	w := auditWatcher(t)
	w.ignorePatterns = []string{"private"}
	dir := filepath.Join(w.dir, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Neither an unrelated backup nor unreadable contents in an ignored subtree
	// belong to Birak's namespace or may block volume binding/recovery.
	os.WriteFile(filepath.Join(dir, ".birak-bak-unrelated"), []byte("keep"), 0o600)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := w.CheckStorage(); err != nil {
		t.Fatalf("ignored directory blocks recovery: %v", err)
	}
}
