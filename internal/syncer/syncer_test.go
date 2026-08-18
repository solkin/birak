package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/store"
)

func TestSafeLocalPathRejectsPeerTraversalAndProtectedTargets(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Syncer{syncDir: root, ignorePatterns: []string{"*.tmp"}, logger: logger}

	valid, err := s.safeLocalPath("bucket/object.txt")
	if err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if want := filepath.Join(root, "bucket", "object.txt"); valid != want {
		t.Fatalf("valid path=%q, want %q", valid, want)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, name := range []string{
		"../outside.txt",
		"sub/../../outside.txt",
		"/absolute.txt",
		"./rewritten.txt",
		".birak/multipart/upload.json",
		"object.tmp",
		"outside-alias",
	} {
		if _, err := s.safeLocalPath(name); err == nil {
			t.Errorf("unsafe peer path %q was accepted", name)
		}
	}

	// applyDeletion must validate before touching disk (or its watcher field), so
	// even hostile metadata cannot remove a target outside syncDir.
	if err := s.applyDeletion(store.FileMeta{Name: "outside-alias", Deleted: true}); err == nil {
		t.Fatal("deletion through an external symlink was accepted")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "keep" {
		t.Fatalf("external target was changed: data=%q err=%v", got, err)
	}
}

func TestDownloadAndApplyRejectsMalformedMetadataAndSizeMismatch(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Syncer{syncDir: root, logger: logger, downloadClient: http.DefaultClient}

	if err := s.downloadAndApply(context.Background(), "http://unused.invalid", store.FileMeta{
		Name: "object.txt", Size: 1, Hash: "short",
	}); err == nil {
		t.Fatal("malformed hash was accepted")
	}

	body := []byte("payload")
	sum := sha256.Sum256(body)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer peer.Close()
	s.downloadClient = peer.Client()

	err := s.downloadAndApply(context.Background(), peer.URL, store.FileMeta{
		Name: "object.txt", Size: int64(len(body) + 1), Hash: hex.EncodeToString(sum[:]),
	})
	if err == nil {
		t.Fatal("peer size mismatch was accepted")
	}
	if _, statErr := os.Stat(filepath.Join(root, "object.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("size-mismatched file was published: %v", statErr)
	}
}
