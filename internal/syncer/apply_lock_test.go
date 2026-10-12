package syncer

// Applying a replica used to read the destination twice under the volume's
// commit lock: the old generation before publishing, then the downloaded bytes
// it had just verified. For a 500 MB APK that was hundreds of milliseconds of
// every gateway write waiting. These tests make any such read fail: the files
// involved are unreadable, which costs a stat-trusting apply nothing.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
)

func requireEnforcedPermissions(t *testing.T) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(probe); err == nil {
		t.Skip("file permissions are not enforced for this user")
	}
}

func TestReplicaApplyDoesNotRereadPublishedBytes(t *testing.T) {
	requireEnforcedPermissions(t)
	s, _ := auditSyncer(t)
	meta := auditMeta("app.apk", "replica bytes", 200)
	s.downloadClient.Transport = auditTransport(func(r *http.Request) (*http.Response, error) {
		response := auditBody("replica bytes")
		// Write-only for the owner: the hash taken while downloading is the
		// only one that can exist.
		response.Header.Set(server.HeaderMode, "200")
		return response, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.applyChange(ctx, "http://peer.invalid", meta); err != nil {
		t.Fatalf("apply read the verified replica again: %v", err)
	}
	got, err := s.store.GetFile(meta.Name)
	if err != nil || got == nil || got.Hash != meta.Hash || got.Size != meta.Size || got.ModTime != meta.ModTime {
		t.Fatalf("index after apply: %+v %v", got, err)
	}
}

func TestReplicaApplyTrustsAnUnchangedOldGeneration(t *testing.T) {
	requireEnforcedPermissions(t)
	s, _ := auditSyncer(t)
	old := auditMeta("app.apk", "old generation", 100)
	auditIndex(t, s, old, "old generation")
	path := filepath.Join(s.syncDir, old.Name)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	newer := auditMeta("app.apk", "new generation", 200)
	s.downloadClient.Transport = auditTransport(func(r *http.Request) (*http.Response, error) {
		return auditBody("new generation"), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.applyChange(ctx, "http://peer.invalid", newer); err != nil {
		t.Fatalf("apply re-read an old generation the index already describes: %v", err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "new generation" {
		t.Fatalf("published: %q %v", body, err)
	}
}

func TestReplicaDeletionTrustsAnUnchangedFile(t *testing.T) {
	requireEnforcedPermissions(t)
	s, _ := auditSyncer(t)
	old := auditMeta("app.apk", "doomed", 100)
	auditIndex(t, s, old, "doomed")
	path := filepath.Join(s.syncDir, old.Name)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	tombstone := old
	tombstone.Deleted, tombstone.ModTime, tombstone.Size = true, 200, 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.applyChange(ctx, "http://peer.invalid", tombstone); err != nil {
		t.Fatalf("deletion re-read the file it removes: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("file survived its deletion: %v", err)
	}
}
