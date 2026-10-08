package birak_test

// Restoring a backup is the operation nobody rehearses and everybody eventually
// performs. It is also the one where this design is least obvious: the restored
// node comes back with metadata that has forgotten writes its peers remember,
// and with a version counter that has already handed out numbers its peers have
// consumed. If that is mishandled the node is silently invisible to the cluster
// — its stream says "nothing new" forever.
//
// This test performs the operation: back both directories up, keep writing,
// restore the backup over the node, and require it to rejoin and hold
// everything the cluster holds.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
)

func TestRestoredBackupRejoinsTheCluster(t *testing.T) {
	binary := daemonBinary(t)
	writer := newCrashNode(t, binary)
	restored := newCrashNode(t, binary)
	writer.pairWith(t, restored)
	restored.pairWith(t, writer)
	writer.start(t)
	restored.start(t)
	writer.awaitReady(t)
	restored.awaitReady(t)

	expected := map[string][]byte{}
	write := func(generation string, count int) {
		t.Helper()
		for i := range count {
			name := fmt.Sprintf("%s-%03d.bin", generation, i)
			body := bytes.Repeat(fmt.Appendf(nil, "%s %03d ", generation, i), 40)
			if !writer.put(name, body) {
				t.Fatalf("write %q was refused", name)
			}
			expected[name] = body
		}
	}
	holdsEverything := func(n *crashNode) func() bool {
		return func() bool {
			// Replica publication writes and flushes bytes before indexing them.
			// Observing the destination path alone is not a replication ACK.
			indexed := map[string]store.FileMeta{}
			for _, entry := range n.manifest(t) {
				indexed[entry.Name] = entry
			}
			for name, want := range expected {
				entry, ok := indexed[name]
				digest := sha256.Sum256(want)
				if !ok || entry.Deleted || entry.Hash != hex.EncodeToString(digest[:]) {
					return false
				}

				got, err := os.ReadFile(filepath.Join(n.syncDir, filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(got, want) {
					return false
				}
			}
			return true
		}
	}

	write("before", 12)
	waitFor(t, 60*time.Second, "the peer to hold the first generation", holdsEverything(restored))

	// A backup is taken with the node stopped, which is the only way to copy
	// both directories consistently.
	restored.kill()
	backup := t.TempDir()
	copyTree(t, restored.syncDir, filepath.Join(backup, "sync"))
	copyTree(t, filepath.Join(restored.root, "meta"), filepath.Join(backup, "meta"))
	restored.start(t)
	restored.awaitReady(t)

	// Life goes on: more writes land while the backup gets older.
	write("after", 12)
	waitFor(t, 60*time.Second, "the peer to hold the second generation", holdsEverything(restored))

	// Now restore. Both directories go back to the older copy together — the
	// metadata and the data must come from the same moment.
	restored.kill()
	if err := os.RemoveAll(restored.syncDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(restored.root, "meta")); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join(backup, "sync"), restored.syncDir)
	copyTree(t, filepath.Join(backup, "meta"), filepath.Join(restored.root, "meta"))

	restored.start(t)
	restored.awaitReady(t)

	// The restored node must catch up on everything it forgot, without anyone
	// intervening and without the writer losing anything either.
	waitFor(t, 120*time.Second, "the restored node to rejoin and catch up", holdsEverything(restored))
	waitFor(t, 60*time.Second, "the writer to still hold everything", holdsEverything(writer))
	restored.checkIndexIsBehindTheDisk(t, 0)
	restored.checkAcknowledgedWritesSurvived(t, 0, expected)

	// And new writes still reach it: the stream is live, not merely caught up
	// once by a manifest comparison.
	write("afterwards", 5)
	waitFor(t, 60*time.Second, "new writes to reach the restored node", holdsEverything(restored))
	t.Logf("restored a backup that was %d writes behind; the node rejoined and holds all %d",
		12, len(expected))
}

// The other half of restoring a backup: it still holds files the cluster has
// since deleted. Bringing them back would be worse than losing them — a delete
// that undoes itself is the failure operators never forgive.
func TestRestoredBackupDoesNotResurrectDeletedFiles(t *testing.T) {
	for _, restamp := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost-timestamps=%v", restamp), func(t *testing.T) { testRestoredBackupDeletion(t, restamp) })
	}
}

func testRestoredBackupDeletion(t *testing.T, restamp bool) {
	binary := daemonBinary(t)
	writer := newCrashNode(t, binary)
	restored := newCrashNode(t, binary)
	writer.pairWith(t, restored)
	restored.pairWith(t, writer)
	writer.start(t)
	restored.start(t)
	writer.awaitReady(t)
	restored.awaitReady(t)

	doomed := "doomed.bin"
	body := bytes.Repeat([]byte("this file is going away "), 40)
	if !writer.put(doomed, body) {
		t.Fatal("initial write was refused")
	}
	waitFor(t, 60*time.Second, "the peer to hold the file", func() bool {
		got, err := os.ReadFile(filepath.Join(restored.syncDir, doomed))
		return err == nil && bytes.Equal(got, body)
	})

	// Back the peer up while the file still exists.
	restored.kill()
	backup := t.TempDir()
	copyTree(t, restored.syncDir, filepath.Join(backup, "sync"))
	copyTree(t, filepath.Join(restored.root, "meta"), filepath.Join(backup, "meta"))
	restored.start(t)
	restored.awaitReady(t)

	// Delete it, everywhere.
	if err := os.Remove(filepath.Join(writer.syncDir, doomed)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "the deletion to reach the peer", func() bool {
		_, err := os.Stat(filepath.Join(restored.syncDir, doomed))
		return os.IsNotExist(err)
	})

	// Restore the backup, which remembers the file as live.
	restored.kill()
	if err := os.RemoveAll(restored.syncDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(restored.root, "meta")); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join(backup, "sync"), restored.syncDir)
	copyTree(t, filepath.Join(backup, "meta"), filepath.Join(restored.root, "meta"))
	if restamp {
		stamp := time.Now().Add(time.Hour)
		if err := filepath.WalkDir(restored.syncDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				return os.Chtimes(path, stamp, stamp)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	restored.start(t)
	restored.awaitReady(t)

	// The cluster's deletion has to win, on both nodes, and stay won.
	waitFor(t, 120*time.Second, "the restored copy to be deleted again", func() bool {
		_, err := os.Stat(filepath.Join(restored.syncDir, doomed))
		return os.IsNotExist(err)
	})
	time.Sleep(3 * time.Second)
	for _, node := range []*crashNode{writer, restored} {
		if _, err := os.Stat(filepath.Join(node.syncDir, doomed)); !os.IsNotExist(err) {
			t.Fatalf("a restored backup resurrected a deleted file on %s: %v", node.id, err)
		}
	}
	t.Log("a backup taken before a delete did not bring the file back")
}

// copyTree copies a directory tree the way a backup tool must: contents,
// permissions **and modification times**.
//
// Preserving timestamps avoids rehashing every restored file. With matching
// metadata or a durable replica intent, unchanged bytes also recover their
// recorded timestamp when a backup tool loses it. Without metadata, a restored
// file is indistinguishable from a new local write.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		sink, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(sink, source); err != nil {
			sink.Close()
			return err
		}
		if err := sink.Close(); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
	if err != nil {
		t.Fatalf("copy %s to %s: %v", from, to, err)
	}
}
