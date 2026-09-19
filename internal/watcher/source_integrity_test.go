package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

func TestNewSymlinkCannotBlessCorruptTarget(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "target")
	stamp := time.Now().Add(-time.Hour)
	if err := os.WriteFile(path, []byte("healthy!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("target"); err != nil {
		t.Fatal(err)
	}
	original, err := w.store.GetFile("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(w.dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("alias"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unverified alias accepted: %v", err)
	}
	if meta, err := w.store.GetFile("alias"); err != nil || meta != nil {
		t.Fatalf("corrupt alias published: %+v %v", meta, err)
	}
	if meta, err := w.store.GetFile("target"); err != nil || meta.Hash != original.Hash {
		t.Fatalf("trusted target changed: %+v %v", meta, err)
	}
	if err := os.WriteFile(path, []byte("healthy!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("alias"); err != nil {
		t.Fatal(err)
	}
	if meta, err := w.store.GetFile("alias"); err != nil || meta == nil || meta.Hash != original.Hash {
		t.Fatalf("repaired alias missing: %+v %v", meta, err)
	}
}

func TestMutationThroughDirectorySymlinkIndexesPhysicalName(t *testing.T) {
	for _, preserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-file", true: "preserved-attributes"}[preserved], func(t *testing.T) {
			w := auditWatcher(t)
			real := filepath.Join(w.dir, "real")
			if err := os.Mkdir(real, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(w.dir, "alias")); err != nil {
				t.Fatal(err)
			}
			stamp := time.Now().Add(-time.Hour)
			if preserved {
				path := filepath.Join(real, "file")
				if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				if err := w.Refresh("real/file"); err != nil {
					t.Fatal(err)
				}
			}
			tmp := filepath.Join(w.dir, ".birak-tmp-test")
			if err := os.WriteFile(tmp, []byte("AFTER!"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(tmp, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			if err := fileops.Publish(w.dir, tmp, filepath.Join(w.dir, "alias", "file")); err != nil {
				t.Fatal(err)
			}
			want, err := hashFile(filepath.Join(real, "file"))
			if err != nil {
				t.Fatal(err)
			}
			meta, err := w.store.GetFile("real/file")
			if err != nil || meta == nil || meta.Hash != want {
				t.Fatalf("ACK without physical metadata: %+v %v", meta, err)
			}
			if alias, err := w.store.GetFile("alias/file"); err != nil || alias != nil {
				t.Fatalf("directory alias leaked into namespace: %+v %v", alias, err)
			}
			if err := w.periodicScan(context.Background()); err != nil {
				t.Fatalf("acknowledged write quarantined: %v", err)
			}
			after, err := w.store.GetFile("real/file")
			if err != nil || after.Version != meta.Version {
				t.Fatalf("scan changed acknowledged generation: %+v %v", after, err)
			}
			if err := fileops.Remove(w.dir, filepath.Join(w.dir, "alias", "file"), false); err != nil {
				t.Fatal(err)
			}
			if meta, err := w.store.GetFile("real/file"); err != nil || meta == nil || !meta.Deleted {
				t.Fatalf("delete through alias not indexed: %+v %v", meta, err)
			}
		})
	}
}

func TestPublishReplacesLeafSymlinkWithoutTrustingTarget(t *testing.T) {
	w := auditWatcher(t)
	target := filepath.Join(w.dir, "target")
	stamp := time.Now().Add(-time.Hour)
	if err := os.WriteFile(target, []byte("healthy!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("target"); err != nil {
		t.Fatal(err)
	}
	original, err := w.store.GetFile("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("corrupt!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(w.dir, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(w.dir, ".birak-tmp-test")
	if err := os.WriteFile(tmp, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fileops.Publish(w.dir, tmp, alias); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(alias); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("alias not replaced: %v %v", info, err)
	}
	if meta, err := w.store.GetFile("target"); err != nil || meta == nil || meta.Hash != original.Hash {
		t.Fatalf("untouched corrupt target was trusted: %+v %v", meta, err)
	}
	if !errors.Is(w.Refresh("target"), ErrIntegrity) {
		t.Fatal("untouched target lost quarantine")
	}
	if meta, err := w.store.GetFile("alias"); err != nil || meta == nil || meta.Deleted || meta.Size != int64(len("replacement")) {
		t.Fatalf("replacement not indexed: %+v %v", meta, err)
	}
}

func TestPartialWriterCannotBlessCorruptBase(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "file")
	stamp := time.Now().Add(-time.Hour)
	os.WriteFile(path, []byte("healthy!"), 0o600)
	os.Chtimes(path, stamp, stamp)
	if err := w.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("corrupt!"), 0o600)
	os.Chtimes(path, stamp, stamp)
	f, _, err := fileops.OpenWriter(w.dir, path, os.O_WRONLY, 0o600)
	if err == nil {
		fileops.AbortWriter(w.dir, f)
		t.Fatal("partial writer accepted an unverified base")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unexpected rejection: %v", err)
	}
	// A complete replacement remains the explicit repair path.
	f, closeWriter, err := fileops.OpenWriter(w.dir, path, os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fileops.AbortWriter(w.dir, f)
	f.WriteString("repaired")
	if err = closeWriter(); err != nil {
		t.Fatal(err)
	}
	if w.NeedsRepair("file") {
		t.Fatal("full rewrite did not clear quarantine")
	}
}
