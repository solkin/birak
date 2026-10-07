package quorum

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func TestLogicalBackupRestoreAndCorruptionFence(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	ctx := context.Background()
	if _, err := n.TransactOnce(ctx, "bucket", []Change{{Key: "b/bucket", MetaOnly: true, Attributes: Attributes{Value: strings.Repeat("<", 6000)}}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Put(ctx, "apk", "o/bucket/app.apk", strings.NewReader("APK payload"), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := n.TransactOnce(ctx, "unfinished", []Change{{Key: "u/bucket/id", MetaOnly: true}}, nil); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := n.ExportS3(ctx, &archive); err != nil {
		t.Fatal(err)
	}
	id := Identity{Cluster: "restored", Node: "new", Format: Format}
	dir := filepath.Join(t.TempDir(), "restore")
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := RestoreS3(deadline, dir, id, "127.0.0.1:9123", bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := RestoreS3(deadline, dir, id, "127.0.0.1:9123", bytes.NewReader(archive.Bytes())); err == nil {
		t.Fatal("overwrote existing restore")
	}
	_, transport := raft.NewInmemTransport("127.0.0.1:9123")
	defer transport.Close()
	cfg := raft.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.HeartbeatTimeout = 100 * time.Millisecond
	cfg.ElectionTimeout = 100 * time.Millisecond
	cfg.LeaderLeaseTimeout = 50 * time.Millisecond
	restored, err := Open(Options{Dir: dir, Identity: id, Transport: transport, Peers: offlinePeers{}, RaftConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for restored.Ready(deadline) != nil {
		if deadline.Err() != nil {
			t.Fatal(deadline.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, f, err := restored.Read(ctx, "o/bucket/app.apk")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(data) != "APK payload" {
		t.Fatal(string(data), err)
	}
	if _, exists, err := restored.Lookup(ctx, "u/bucket/id"); err != nil || exists {
		t.Fatal("restored in-flight upload", exists, err)
	}
	conf, err := restored.Configuration()
	if err != nil || len(conf.Servers) != 1 || conf.Servers[0].Address != "127.0.0.1:9123" {
		t.Fatal(conf, err)
	}
	for _, kind := range []string{"blob", "metadata", "truncated", "same-cluster", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			damaged := append([]byte(nil), archive.Bytes()...)
			targetID := id
			switch kind {
			case "blob":
				i := bytes.Index(damaged, []byte("APK payload"))
				if i < 0 {
					t.Fatal("payload absent")
				}
				damaged[i] = 'X'
			case "metadata":
				i := bytes.Index(damaged, []byte("app.apk"))
				if i < 0 {
					t.Fatal("key absent")
				}
				damaged[i] = 'b'
			case "truncated":
				damaged = damaged[:len(damaged)/2]
			case "same-cluster":
				targetID.Cluster = n.id.Cluster
			case "trailing":
				damaged = append(damaged, 1)
			}
			dest := filepath.Join(t.TempDir(), "failed")
			if err := RestoreS3(deadline, dest, targetID, "127.0.0.1:9123", bytes.NewReader(damaged)); err == nil {
				t.Fatal("accepted damaged backup")
			}
			if _, err := os.Stat(filepath.Join(dest, "restore-incomplete")); err != nil {
				t.Fatal(err)
			}
			if node, err := Open(Options{Dir: dest, Identity: targetID, Transport: transport, Peers: offlinePeers{}}); err == nil {
				node.Close()
				t.Fatal("partial restore served data")
			}
		})
	}
}

type interceptWriter struct {
	bytes.Buffer
	once      bool
	intercept func()
}

func (w *interceptWriter) Write(p []byte) (int, error) {
	if !w.once {
		w.once = true
		w.intercept()
	}
	return w.Buffer.Write(p)
}
func TestBackupPinsOldBoundaryDuringOverwriteAndCollection(t *testing.T) {
	l := newLab(t)
	n := l.grow(1)
	ctx := context.Background()
	if _, err := n.TransactOnce(ctx, "b", []Change{{Key: "b/bucket", MetaOnly: true}}, nil); err != nil {
		t.Fatal(err)
	}
	entry, err := n.Put(ctx, "old", "o/bucket/key", strings.NewReader("original"), 100)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(l.dirs[n.id.Node], "generations", "objects", entry.Ref.Hash[:2], entry.Ref.Hash[2:])
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	w := &interceptWriter{intercept: func() {
		if _, err := n.Put(ctx, "new", "o/bucket/key", strings.NewReader("replacement"), 100); err != nil {
			t.Fatal(err)
		}
		result, err := n.Collect(ctx, time.Hour)
		if err != nil || result.Nodes[n.id.Node].Pinned != 1 {
			t.Fatal(result, err)
		}
	}}
	if err := n.ExportS3(ctx, w); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(w.Bytes()))
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "blobs/"+entry.Ref.Hash {
			data, err := io.ReadAll(tr)
			if err != nil || string(data) != "original" {
				t.Fatal(string(data), err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("backup lost its original boundary")
	}
	result, err := n.Collect(ctx, time.Hour)
	if err != nil || result.Nodes[n.id.Node].Files != 1 {
		t.Fatal("backup pin leaked", result, err)
	}
}
