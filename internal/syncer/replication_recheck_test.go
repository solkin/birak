package syncer

// Regression tests for the second replication review.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/server"
)

func TestReviewSFTPAliasMustProtectUnderlyingWriter(t *testing.T) {
	s, _ := auditSyncer(t)
	initial := auditMeta("target", "original", time.Now().Add(-time.Hour).UnixNano())
	auditIndex(t, s, initial, "original")
	alias := filepath.Join(s.syncDir, "alias")
	if err := os.Symlink("target", alias); err != nil {
		t.Fatal(err)
	}
	// SFTP's SafePath accepts a symlink to a visible regular file in the root.
	if _, err := s.safeLocalPath("alias"); err != nil {
		t.Fatal(err)
	}
	f, closeWriter, err := fileops.OpenWriter(s.syncDir, alias, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWriter()
	s.downloadClient.Transport = auditTransport(func(*http.Request) (*http.Response, error) {
		return auditBody("replica!"), nil
	})
	err = s.applyChange(context.Background(), "http://peer.invalid",
		auditMeta("target", "replica!", time.Now().Add(-time.Minute).UnixNano()))
	if err != nil && !errors.Is(err, fileops.ErrBusy) {
		t.Fatal(err)
	}
	// The client's write happens AFTER replication and close acknowledges it.
	if _, err := f.WriteAt([]byte("CLIENT!!"), 0); err != nil {
		t.Fatal(err)
	}
	if err := closeWriter(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.syncDir, "target"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "CLIENT!!" {
		t.Fatalf("acknowledged later SFTP write went to an orphaned inode: target=%q, want CLIENT!!", got)
	}
}

func TestReviewChecksumMismatchMustNotOverwriteHealthyReplica(t *testing.T) {
	source, _ := auditSyncer(t)
	replica, _ := auditSyncer(t)
	source.nodeID = "source"
	replica.nodeID = "replica"
	stamp := time.Now().Add(-time.Hour).UnixNano()
	good := "AAAA"
	meta := auditMeta("valuable", good, stamp)
	auditIndex(t, source, meta, good)
	auditIndex(t, replica, meta, good)
	// Inject a one-byte storage corruption with unchanged length and mtime.
	// Pick a corruption whose hash wins the existing equal-time tie-break.
	var corrupt string
	for b := 0; b < 256; b++ {
		candidate := string([]byte{byte(b), 'A', 'A', 'A'})
		if candidate != good && auditMeta(meta.Name, candidate, stamp).Hash > meta.Hash {
			corrupt = candidate
			break
		}
	}
	if corrupt == "" {
		t.Fatal("could not construct one-byte corruption fixture")
	}
	path := filepath.Join(source.syncDir, meta.Name)
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(0, stamp), time.Unix(0, stamp)); err != nil {
		t.Fatal(err)
	}
	// Refresh is also the exact per-file inspection used by checksum scans.
	if err := source.watcher.Refresh(meta.Name); err != nil {
		t.Logf("checksum mismatch quarantined: %v", err)
		return
	}
	advertised, err := source.store.GetFile(meta.Name)
	if err != nil || advertised == nil {
		t.Fatalf("read advertised state: %+v %v", advertised, err)
	}
	peer := httptest.NewServer(server.New(source.store, source.syncDir, source.nodeID, nil, server.Config{}, source.logger).Handler())
	defer peer.Close()
	if err := replica.applyChange(context.Background(), peer.URL, *advertised); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(replica.syncDir, meta.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != good {
		t.Fatalf("checksum scan promoted one-byte corruption and destroyed healthy replica: good=%q corrupt=%q replica=%q", good, corrupt, got)
	}
}
