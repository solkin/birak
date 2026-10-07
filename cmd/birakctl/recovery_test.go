package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/cluster"
	"github.com/birak/birak/internal/quorum"
	"github.com/hashicorp/raft"
)

func TestCLIBackupRestoreAndIncompleteStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("strict NTFS durability unsupported")
	}
	dir := t.TempDir()
	if err := cluster.CreateCA(dir, "source"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []cluster.Principal{{Cluster: "source", Role: "admin", ID: "op"}, {Cluster: "source", Role: "node", ID: "n1"}} {
		if err := cluster.IssueCertificate(dir, p, []string{"127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
	}
	credentials, err := cluster.LoadCredentials(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "node-n1.crt"), filepath.Join(dir, "node-n1.key"), cluster.Principal{Cluster: "source", Role: "node", ID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := raft.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.HeartbeatTimeout = 100 * time.Millisecond
	cfg.ElectionTimeout = 100 * time.Millisecond
	cfg.LeaderLeaseTimeout = 50 * time.Millisecond
	state := filepath.Join(dir, "state")
	s, err := cluster.Open(cluster.Options{Dir: state, Cluster: "source", ID: "n1", Listen: "127.0.0.1:0", Bootstrap: true, Credentials: credentials, MaxBlobBytes: 1024, RaftConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for s.Node().Ready(ctx) != nil {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := s.Node().TransactOnce(ctx, "bucket", []quorum.Change{{Key: "b/bucket", MetaOnly: true}}, nil); err != nil {
		t.Fatal(err)
	}
	e, err := s.Node().Put(ctx, "key", "o/bucket/key", strings.NewReader("payload"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "archive.tar")
	args := []string{"backup", "--cluster", "source", "--node", "n1", "--address", s.Address(), "--operator", "op", "--ca", filepath.Join(dir, "ca.crt"), "--cert", filepath.Join(dir, "admin-op.crt"), "--key", filepath.Join(dir, "admin-op.key"), "--file", archive}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(args); err == nil {
		t.Fatal("existing archive overwritten")
	}
	after, _ := os.ReadFile(archive)
	if string(after) != string(original) {
		t.Fatal("archive changed")
	}
	dest := filepath.Join(dir, "restored")
	if err := run([]string{"restore", "--file", archive, "--dir", dest, "--cluster", "restored", "--node", "new", "--address", "127.0.0.1:9222", "--timeout", "10s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "restore-incomplete")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(state, "generations", "objects", e.Ref.Hash[:2], e.Ref.Hash[2:])); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(dir, "partial.tar")
	args[len(args)-1] = failed
	if err := run(args); err == nil {
		t.Fatal("incomplete backup reported success")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatal("partial backup published", err)
	}
}
