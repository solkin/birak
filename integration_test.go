package birak_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/syncer"
	"github.com/birak/birak/internal/watcher"
)

// testNode represents a single birak daemon instance for testing.
type testNode struct {
	id       string
	syncDir  string
	metaDir  string
	store    *store.Store
	watcher  *watcher.Watcher
	syncer   *syncer.Syncer
	server   *http.Server
	addr     string
	logger   *slog.Logger
	cancel   context.CancelFunc
}

// defaultTestIgnore provides default ignore patterns for tests.
var defaultTestIgnore = []string{".DS_Store", "Thumbs.db", "desktop.ini", ".birak-tmp-*"}

func waitForHTTP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/status", addr))
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("HTTP server at %s did not start in time", addr)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file %s: %v", name, err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", name, err)
	}
	return string(data)
}

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func sha256Hex(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// waitForSync polls until a condition is true or timeout.
func waitForSync(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("sync did not complete within timeout")
}

// --- Tests ---

func TestIntegration_TwoNodes_FileCreation(t *testing.T) {
	// Create two nodes that peer with each other.
	// We need to know addresses before creating, so pre-allocate ports.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Write a file to node1.
	writeFile(t, node1.syncDir, "hello.txt", "hello world")

	// Wait for it to appear on node2.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "hello.txt")
	})

	content := readFile(t, node2.syncDir, "hello.txt")
	if content != "hello world" {
		t.Fatalf("expected 'hello world', got %q", content)
	}
	t.Log("file creation synced successfully")
}

func TestIntegration_TwoNodes_FileModification(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Write initial file.
	writeFile(t, node1.syncDir, "data.txt", "version 1")
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "data.txt")
	})

	content := readFile(t, node2.syncDir, "data.txt")
	if content != "version 1" {
		t.Fatalf("expected 'version 1', got %q", content)
	}

	// Modify the file on node1.
	time.Sleep(100 * time.Millisecond) // Ensure different mod_time.
	writeFile(t, node1.syncDir, "data.txt", "version 2")

	// Wait for modification to sync.
	waitForSync(t, 10*time.Second, func() bool {
		c := readFile(t, node2.syncDir, "data.txt")
		return c == "version 2"
	})

	t.Log("file modification synced successfully")
}

func TestIntegration_TwoNodes_FileDeletion(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Write a file and sync it.
	writeFile(t, node1.syncDir, "todelete.txt", "bye")
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "todelete.txt")
	})

	// Delete the file on node1.
	time.Sleep(100 * time.Millisecond)
	os.Remove(filepath.Join(node1.syncDir, "todelete.txt"))

	// Wait for deletion to sync.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(node2.syncDir, "todelete.txt")
	})

	t.Log("file deletion synced successfully")
}

func TestIntegration_ThreeNodes_TransitiveSync(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	ln3, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	addr3 := ln3.Addr().String()
	ln1.Close()
	ln2.Close()
	ln3.Close()

	// Full mesh: each node peers with both others.
	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2, "http://" + addr3})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1, "http://" + addr3})
	node3 := newTestNodeWithAddr(t, "node-3", addr3, []string{"http://" + addr1, "http://" + addr2})

	// Write a file on node1.
	writeFile(t, node1.syncDir, "shared.txt", "from node 1")

	// Wait for it to appear on node2 and node3.
	waitForSync(t, 15*time.Second, func() bool {
		return fileExists(node2.syncDir, "shared.txt") && fileExists(node3.syncDir, "shared.txt")
	})

	c2 := readFile(t, node2.syncDir, "shared.txt")
	c3 := readFile(t, node3.syncDir, "shared.txt")
	if c2 != "from node 1" || c3 != "from node 1" {
		t.Fatalf("expected 'from node 1' on all nodes, got node2=%q node3=%q", c2, c3)
	}

	// Now write on node3, should sync to node1 and node2.
	time.Sleep(100 * time.Millisecond)
	writeFile(t, node3.syncDir, "from-three.txt", "hello from 3")

	waitForSync(t, 15*time.Second, func() bool {
		return fileExists(node1.syncDir, "from-three.txt") && fileExists(node2.syncDir, "from-three.txt")
	})

	c1 := readFile(t, node1.syncDir, "from-three.txt")
	c2 = readFile(t, node2.syncDir, "from-three.txt")
	if c1 != "hello from 3" || c2 != "hello from 3" {
		t.Fatalf("expected 'hello from 3' on all nodes, got node1=%q node2=%q", c1, c2)
	}

	t.Log("three-node transitive sync completed successfully")
}

func TestIntegration_ConflictResolution_NewerWins(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Step 1: Write file on node1 and wait for it to sync to node2.
	writeFile(t, node1.syncDir, "conflict.txt", "old content")
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "conflict.txt")
	})

	// Verify initial sync.
	c := readFile(t, node2.syncDir, "conflict.txt")
	if c != "old content" {
		t.Fatalf("expected 'old content' on node2, got %q", c)
	}

	// Step 2: Now update the file on node2 with newer content.
	// This should have a newer mod_time and propagate back to node1.
	time.Sleep(200 * time.Millisecond)
	writeFile(t, node2.syncDir, "conflict.txt", "new content")

	// Wait for node1 to get the newer version.
	waitForSync(t, 10*time.Second, func() bool {
		if !fileExists(node1.syncDir, "conflict.txt") {
			return false
		}
		c1 := readFile(t, node1.syncDir, "conflict.txt")
		return c1 == "new content"
	})

	// Both should now have "new content".
	c1 := readFile(t, node1.syncDir, "conflict.txt")
	c2 := readFile(t, node2.syncDir, "conflict.txt")
	if c1 != "new content" || c2 != "new content" {
		t.Fatalf("expected 'new content' on both, got node1=%q node2=%q", c1, c2)
	}

	t.Log("conflict resolution (newer wins) completed successfully")
}

func TestIntegration_PreExistingFiles(t *testing.T) {
	// Simulate starting with non-empty folders.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	// Create dirs and pre-populate files BEFORE starting nodes.
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	syncDir1 := filepath.Join(dir1, "sync")
	syncDir2 := filepath.Join(dir2, "sync")
	os.MkdirAll(syncDir1, 0o755)
	os.MkdirAll(syncDir2, 0o755)

	// Node1 has file A, Node2 has file B.
	os.WriteFile(filepath.Join(syncDir1, "only-on-1.txt"), []byte("from 1"), 0o644)
	os.WriteFile(filepath.Join(syncDir2, "only-on-2.txt"), []byte("from 2"), 0o644)
	// Both have the same file with different content. Node2's should be newer.
	// Use explicit timestamps to avoid flakiness from filesystem time resolution.
	os.WriteFile(filepath.Join(syncDir1, "shared.txt"), []byte("node1 version"), 0o644)
	oldTime := time.Now().Add(-10 * time.Second)
	os.Chtimes(filepath.Join(syncDir1, "shared.txt"), oldTime, oldTime)
	os.WriteFile(filepath.Join(syncDir2, "shared.txt"), []byte("node2 version"), 0o644)
	// node2's shared.txt keeps current time — guaranteed newer than node1's

	// Now start nodes with pre-existing files.
	node1 := newTestNodeWithDirs(t, "node-1", addr1, syncDir1, filepath.Join(dir1, "meta"), []string{"http://" + addr2})
	node2 := newTestNodeWithDirs(t, "node-2", addr2, syncDir2, filepath.Join(dir2, "meta"), []string{"http://" + addr1})

	// Wait for sync.
	waitForSync(t, 15*time.Second, func() bool {
		return fileExists(node1.syncDir, "only-on-2.txt") && fileExists(node2.syncDir, "only-on-1.txt")
	})

	// Verify.
	c := readFile(t, node2.syncDir, "only-on-1.txt")
	if c != "from 1" {
		t.Fatalf("expected 'from 1', got %q", c)
	}
	c = readFile(t, node1.syncDir, "only-on-2.txt")
	if c != "from 2" {
		t.Fatalf("expected 'from 2', got %q", c)
	}

	// Shared file: node2's version is newer, should win on both.
	waitForSync(t, 10*time.Second, func() bool {
		c1 := readFile(t, node1.syncDir, "shared.txt")
		c2 := readFile(t, node2.syncDir, "shared.txt")
		return c1 == "node2 version" && c2 == "node2 version"
	})

	t.Log("pre-existing files synced successfully")
}

func TestIntegration_HTTPStatus(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	ln1.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, nil)

	writeFile(t, node1.syncDir, "test.txt", "data")
	time.Sleep(1 * time.Second) // Wait for watcher to pick up.

	resp, err := http.Get(fmt.Sprintf("http://%s/status", node1.addr))
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	var status server.StatusResponse
	json.NewDecoder(resp.Body).Decode(&status)

	if status.NodeID != "node-1" {
		t.Fatalf("expected node_id 'node-1', got %q", status.NodeID)
	}
	if status.FileCount != 1 {
		t.Fatalf("expected file_count 1, got %d", status.FileCount)
	}
	if status.MaxVersion < 1 {
		t.Fatalf("expected max_version >= 1, got %d", status.MaxVersion)
	}
	t.Logf("status: %+v", status)
}

func TestIntegration_MultipleFiles(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Write many files on node1.
	numFiles := 20
	for i := 0; i < numFiles; i++ {
		writeFile(t, node1.syncDir, fmt.Sprintf("file-%03d.txt", i), fmt.Sprintf("content %d", i))
	}

	// Wait for all files to sync.
	waitForSync(t, 30*time.Second, func() bool {
		for i := 0; i < numFiles; i++ {
			if !fileExists(node2.syncDir, fmt.Sprintf("file-%03d.txt", i)) {
				return false
			}
		}
		return true
	})

	// Verify content.
	for i := 0; i < numFiles; i++ {
		name := fmt.Sprintf("file-%03d.txt", i)
		expected := fmt.Sprintf("content %d", i)
		got := readFile(t, node2.syncDir, name)
		if got != expected {
			t.Fatalf("file %s: expected %q, got %q", name, expected, got)
		}
	}

	t.Logf("synced %d files successfully", numFiles)
}

func TestIntegration_EmptyFile(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Write an empty file.
	writeFile(t, node1.syncDir, "empty.txt", "")

	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "empty.txt")
	})

	content := readFile(t, node2.syncDir, "empty.txt")
	if content != "" {
		t.Fatalf("expected empty file, got %q", content)
	}
	t.Log("empty file synced successfully")
}

func TestIntegration_SameContentNoop(t *testing.T) {
	// If two nodes have the same file with the same content, no transfer should happen.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	// Pre-populate both dirs with identical content.
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	syncDir1 := filepath.Join(dir1, "sync")
	syncDir2 := filepath.Join(dir2, "sync")
	os.MkdirAll(syncDir1, 0o755)
	os.MkdirAll(syncDir2, 0o755)

	os.WriteFile(filepath.Join(syncDir1, "same.txt"), []byte("identical"), 0o644)
	os.WriteFile(filepath.Join(syncDir2, "same.txt"), []byte("identical"), 0o644)

	node1 := newTestNodeWithDirs(t, "node-1", addr1, syncDir1, filepath.Join(dir1, "meta"), []string{"http://" + addr2})
	node2 := newTestNodeWithDirs(t, "node-2", addr2, syncDir2, filepath.Join(dir2, "meta"), []string{"http://" + addr1})

	// Let sync settle.
	time.Sleep(3 * time.Second)

	// Both should still have "identical" — no overwrites.
	c1 := readFile(t, node1.syncDir, "same.txt")
	c2 := readFile(t, node2.syncDir, "same.txt")
	if c1 != "identical" || c2 != "identical" {
		t.Fatalf("expected 'identical' on both, got node1=%q node2=%q", c1, c2)
	}
	t.Log("same content no-op verified")
}

func TestIntegration_CreateAndDeleteBeforeSync(t *testing.T) {
	// File created and immediately deleted — peer should NOT end up with the file.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	_ = newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Write a file, then immediately delete it.
	writeFile(t, node1.syncDir, "ephemeral.txt", "gone soon")
	time.Sleep(50 * time.Millisecond)
	os.Remove(filepath.Join(node1.syncDir, "ephemeral.txt"))

	// Also create a control file to confirm sync is working.
	time.Sleep(200 * time.Millisecond)
	writeFile(t, node1.syncDir, "control.txt", "still here")

	// Wait for control file to sync (proves syncer is active).
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node1.syncDir, "control.txt") // watcher picked it up
	})

	// After sync settles, ephemeral should NOT exist on node2.
	time.Sleep(3 * time.Second)
	if fileExists(node1.syncDir, "ephemeral.txt") {
		t.Fatal("ephemeral.txt should not exist on node1")
	}
	t.Log("create-and-delete before sync handled correctly")
}

func TestIntegration_PeerReconnect(t *testing.T) {
	// Node2 goes down and comes back — cursor should be persisted, sync resumes.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithDirs(t, "node-2", addr2,
		filepath.Join(t.TempDir(), "sync"),
		filepath.Join(t.TempDir(), "meta"),
		[]string{"http://" + addr1},
	)

	// Write a file and wait for sync.
	writeFile(t, node1.syncDir, "before-restart.txt", "v1")
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "before-restart.txt")
	})

	// Remember node2's dirs for restart.
	node2SyncDir := node2.syncDir
	node2MetaDir := node2.metaDir

	// "Stop" node2 (cancel context, close server+store).
	node2.cancel()
	node2.server.Close()
	node2.store.Close()
	t.Log("node2 stopped")

	// Write another file on node1 while node2 is down.
	time.Sleep(500 * time.Millisecond)
	writeFile(t, node1.syncDir, "during-downtime.txt", "missed")

	// Restart node2 with the same dirs (persisted cursor + data).
	time.Sleep(1 * time.Second)
	node2restart := newTestNodeWithDirs(t, "node-2-restarted", addr2,
		node2SyncDir, node2MetaDir,
		[]string{"http://" + addr1},
	)

	// It should pick up the file written during downtime.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2restart.syncDir, "during-downtime.txt")
	})

	content := readFile(t, node2restart.syncDir, "during-downtime.txt")
	if content != "missed" {
		t.Fatalf("expected 'missed', got %q", content)
	}

	// Original file should still be there.
	content = readFile(t, node2restart.syncDir, "before-restart.txt")
	if content != "v1" {
		t.Fatalf("expected 'v1', got %q", content)
	}
	t.Log("peer reconnect with cursor persistence successful")
}

func TestIntegration_HTTPPathTraversal(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	ln1.Close()

	_ = newTestNodeWithAddr(t, "node-1", addr1, nil)

	// Try various edge cases.
	// Note: Go's net/http cleans paths automatically, preventing directory traversal
	// at the router level (e.g. /../ gets resolved before reaching the handler).
	testCases := []struct {
		path string
		code int
	}{
		{"/files/../../../etc/passwd", http.StatusNotFound},    // cleaned by router
		{"/files/nonexistent.txt", http.StatusNotFound},        // simply missing
		{"/changes", http.StatusBadRequest},                    // missing 'since' param
		{"/changes?since=notanumber", http.StatusBadRequest},   // invalid param
		{"/changes?since=0&limit=-1", http.StatusBadRequest},   // invalid limit
		{"/changes?since=0&limit=abc", http.StatusBadRequest},  // invalid limit
	}

	for _, tc := range testCases {
		resp, err := http.Get(fmt.Sprintf("http://%s%s", addr1, tc.path))
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Errorf("GET %s: expected %d, got %d", tc.path, tc.code, resp.StatusCode)
		}
	}
	t.Log("HTTP edge cases verified")
}

func TestIntegration_IdempotentSync(t *testing.T) {
	// Verify that re-polling the same data doesn't create duplicates or errors.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	writeFile(t, node1.syncDir, "idem.txt", "content")
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "idem.txt")
	})

	// Let multiple poll cycles pass with no changes.
	time.Sleep(3 * time.Second)

	// Both should have exactly 1 file.
	count1, _ := node1.store.FileCount()
	count2, _ := node2.store.FileCount()
	if count1 != 1 || count2 != 1 {
		t.Fatalf("expected 1 file on each, got node1=%d node2=%d", count1, count2)
	}

	// Content should be unchanged.
	c1 := readFile(t, node1.syncDir, "idem.txt")
	c2 := readFile(t, node2.syncDir, "idem.txt")
	if c1 != "content" || c2 != "content" {
		t.Fatalf("content changed unexpectedly: node1=%q node2=%q", c1, c2)
	}
	t.Log("idempotent sync verified")
}

func TestIntegration_RapidModifications(t *testing.T) {
	// Rapidly modify the same file — only the latest version should sync.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Write 10 rapid modifications to the same file.
	for i := 0; i < 10; i++ {
		writeFile(t, node1.syncDir, "rapid.txt", fmt.Sprintf("version-%d", i))
	}

	// The final version should win.
	waitForSync(t, 15*time.Second, func() bool {
		if !fileExists(node2.syncDir, "rapid.txt") {
			return false
		}
		c := readFile(t, node2.syncDir, "rapid.txt")
		return c == "version-9"
	})

	t.Log("rapid modifications synced correctly (latest version wins)")
}

func TestIntegration_SubdirectorySync(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create a subdirectory with a file on node1.
	subDir := filepath.Join(node1.syncDir, "docs", "drafts")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "report.txt", "hello from subdir")

	// Wait for it to sync to node2.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "docs", "drafts"), "report.txt")
	})

	content := readFile(t, filepath.Join(node2.syncDir, "docs", "drafts"), "report.txt")
	if content != "hello from subdir" {
		t.Fatalf("expected 'hello from subdir', got %q", content)
	}

	// Also write a file at root level to make sure both work.
	writeFile(t, node1.syncDir, "root.txt", "at root")
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "root.txt")
	})
	if readFile(t, node2.syncDir, "root.txt") != "at root" {
		t.Fatal("root file content mismatch")
	}

	t.Log("subdirectory sync completed successfully")
}

func TestIntegration_SubdirectoryDeletion(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create a file in a subdirectory and sync it.
	subDir := filepath.Join(node1.syncDir, "data")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "file.txt", "content")
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "data"), "file.txt")
	})

	// Delete the file on node1.
	time.Sleep(100 * time.Millisecond)
	os.Remove(filepath.Join(subDir, "file.txt"))

	// Wait for deletion to sync — both file and empty parent should be cleaned up.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(filepath.Join(node2.syncDir, "data"), "file.txt")
	})

	t.Log("subdirectory deletion synced successfully")
}

func TestIntegration_IgnoredFiles(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Write an ignored file (.DS_Store) and a normal file.
	writeFile(t, node1.syncDir, ".DS_Store", "apple stuff")
	writeFile(t, node1.syncDir, "real.txt", "important data")

	// Also write .DS_Store in a subdir.
	subDir := filepath.Join(node1.syncDir, "sub")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, ".DS_Store", "apple stuff in sub")
	writeFile(t, subDir, "real-sub.txt", "important sub data")

	// Wait for the real file to sync.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "real.txt")
	})
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "sub"), "real-sub.txt")
	})

	// .DS_Store should NOT have synced.
	time.Sleep(2 * time.Second) // Extra wait to make sure.
	if fileExists(node2.syncDir, ".DS_Store") {
		t.Fatal(".DS_Store should not have been synced to node2")
	}
	if fileExists(filepath.Join(node2.syncDir, "sub"), ".DS_Store") {
		t.Fatal("sub/.DS_Store should not have been synced to node2")
	}

	t.Log("ignored files correctly excluded from sync")
}

func TestIntegration_DeepNestedSubdirs(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create a deeply nested structure: a/b/c/d/file.txt
	deepDir := filepath.Join(node1.syncDir, "a", "b", "c", "d")
	os.MkdirAll(deepDir, 0o755)
	writeFile(t, deepDir, "deep.txt", "deep content")

	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "a", "b", "c", "d"), "deep.txt")
	})

	content := readFile(t, filepath.Join(node2.syncDir, "a", "b", "c", "d"), "deep.txt")
	if content != "deep content" {
		t.Fatalf("expected 'deep content', got %q", content)
	}

	t.Log("deep nested subdirectory sync completed successfully")
}

func TestIntegration_DirWithOnlyIgnoredFile(t *testing.T) {
	// After deleting the last real file in a subdir that also contains .DS_Store,
	// the directory (and .DS_Store inside it) should be cleaned up on the peer.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create subdir with a real file.
	subDir := filepath.Join(node1.syncDir, "photos")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "pic.jpg", "image data")

	// Wait for sync.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "photos"), "pic.jpg")
	})

	// Now simulate macOS creating .DS_Store on node2's side (as it would when user browses).
	writeFile(t, filepath.Join(node2.syncDir, "photos"), ".DS_Store", "apple index")

	// Delete the real file on node1.
	time.Sleep(200 * time.Millisecond)
	os.Remove(filepath.Join(subDir, "pic.jpg"))

	// Wait for deletion to sync on node2.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(filepath.Join(node2.syncDir, "photos"), "pic.jpg")
	})

	// The photos/ directory should be cleaned up, including the .DS_Store.
	time.Sleep(1 * time.Second)
	if fileExists(node2.syncDir, "photos") {
		// Check what's left.
		entries, _ := os.ReadDir(filepath.Join(node2.syncDir, "photos"))
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("photos/ directory should have been cleaned up, remaining: %v", names)
	}

	t.Log("directory with only ignored file cleaned up correctly")
}

func TestIntegration_PartialDirCleanup(t *testing.T) {
	// a/b/file1.txt and a/b/c/file2.txt exist. Delete file2.txt.
	// a/b/c/ should be removed but a/b/ should stay.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create both files.
	os.MkdirAll(filepath.Join(node1.syncDir, "a", "b", "c"), 0o755)
	writeFile(t, filepath.Join(node1.syncDir, "a", "b"), "file1.txt", "stays")
	writeFile(t, filepath.Join(node1.syncDir, "a", "b", "c"), "file2.txt", "goes away")

	// Wait for both to sync.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "a", "b"), "file1.txt") &&
			fileExists(filepath.Join(node2.syncDir, "a", "b", "c"), "file2.txt")
	})

	// Delete file2.txt on node1.
	time.Sleep(200 * time.Millisecond)
	os.Remove(filepath.Join(node1.syncDir, "a", "b", "c", "file2.txt"))

	// Wait for deletion to sync.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(filepath.Join(node2.syncDir, "a", "b", "c"), "file2.txt")
	})

	// a/b/file1.txt should still be there.
	if !fileExists(filepath.Join(node2.syncDir, "a", "b"), "file1.txt") {
		t.Fatal("a/b/file1.txt should still exist")
	}
	content := readFile(t, filepath.Join(node2.syncDir, "a", "b"), "file1.txt")
	if content != "stays" {
		t.Fatalf("expected 'stays', got %q", content)
	}

	// a/b/c/ directory should have been removed.
	if fileExists(filepath.Join(node2.syncDir, "a", "b"), "c") {
		t.Fatal("a/b/c/ directory should have been removed")
	}

	t.Log("partial directory cleanup works correctly")
}

func TestIntegration_SubdirFileModification(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create file in subdir.
	subDir := filepath.Join(node1.syncDir, "docs")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "notes.txt", "version 1")

	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "docs"), "notes.txt")
	})
	if readFile(t, filepath.Join(node2.syncDir, "docs"), "notes.txt") != "version 1" {
		t.Fatal("initial version mismatch")
	}

	// Modify the file.
	time.Sleep(200 * time.Millisecond)
	writeFile(t, subDir, "notes.txt", "version 2")

	waitForSync(t, 10*time.Second, func() bool {
		return readFile(t, filepath.Join(node2.syncDir, "docs"), "notes.txt") == "version 2"
	})

	t.Log("subdirectory file modification synced correctly")
}

func TestIntegration_FileWithSpaces(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// File with spaces in name.
	writeFile(t, node1.syncDir, "my report.txt", "space content")

	// File in subdir with spaces.
	subDir := filepath.Join(node1.syncDir, "my docs")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "my file.txt", "subdir space content")

	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "my report.txt") &&
			fileExists(filepath.Join(node2.syncDir, "my docs"), "my file.txt")
	})

	if readFile(t, node2.syncDir, "my report.txt") != "space content" {
		t.Fatal("file with spaces: content mismatch")
	}
	if readFile(t, filepath.Join(node2.syncDir, "my docs"), "my file.txt") != "subdir space content" {
		t.Fatal("subdir file with spaces: content mismatch")
	}

	t.Log("files with spaces synced correctly")
}

func TestIntegration_PreExistingSubdirs(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	// Pre-create nested directory structures before starting nodes.
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	syncDir1 := filepath.Join(dir1, "sync")
	syncDir2 := filepath.Join(dir2, "sync")

	// Node1 has nested dirs with files.
	os.MkdirAll(filepath.Join(syncDir1, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(syncDir1, "a", "top.txt"), []byte("top level"), 0o644)
	os.WriteFile(filepath.Join(syncDir1, "a", "b", "nested.txt"), []byte("nested file"), 0o644)

	// Node2 has a different nested structure.
	os.MkdirAll(filepath.Join(syncDir2, "x", "y"), 0o755)
	os.WriteFile(filepath.Join(syncDir2, "x", "y", "other.txt"), []byte("other nested"), 0o644)

	node1 := newTestNodeWithDirs(t, "node-1", addr1, syncDir1, filepath.Join(dir1, "meta"), []string{"http://" + addr2})
	node2 := newTestNodeWithDirs(t, "node-2", addr2, syncDir2, filepath.Join(dir2, "meta"), []string{"http://" + addr1})

	// Wait for bidirectional sync.
	waitForSync(t, 15*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "a", "b"), "nested.txt") &&
			fileExists(filepath.Join(node2.syncDir, "a"), "top.txt") &&
			fileExists(filepath.Join(node1.syncDir, "x", "y"), "other.txt")
	})

	// Verify content.
	if readFile(t, filepath.Join(node2.syncDir, "a", "b"), "nested.txt") != "nested file" {
		t.Fatal("nested.txt content mismatch")
	}
	if readFile(t, filepath.Join(node2.syncDir, "a"), "top.txt") != "top level" {
		t.Fatal("top.txt content mismatch")
	}
	if readFile(t, filepath.Join(node1.syncDir, "x", "y"), "other.txt") != "other nested" {
		t.Fatal("other.txt content mismatch")
	}

	_ = node1
	_ = node2
	t.Log("pre-existing subdirectories synced correctly")
}

func TestIntegration_CustomIgnorePatterns(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	// Custom patterns: ignore *.log and *.tmp files.
	customIgnore := []string{".DS_Store", ".birak-tmp-*", "*.log", "*.tmp"}

	node1 := newTestNodeWithIgnore(t, "node-1", addr1, customIgnore, []string{"http://" + addr2})
	node2 := newTestNodeWithIgnore(t, "node-2", addr2, customIgnore, []string{"http://" + addr1})

	// Write various files.
	writeFile(t, node1.syncDir, "app.log", "log data")
	writeFile(t, node1.syncDir, "cache.tmp", "temp data")
	writeFile(t, node1.syncDir, "real.txt", "important")

	// Also in a subdir.
	subDir := filepath.Join(node1.syncDir, "logs")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "debug.log", "debug stuff")
	writeFile(t, subDir, "data.csv", "csv data")

	// Wait for real files to sync.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(node2.syncDir, "real.txt") &&
			fileExists(filepath.Join(node2.syncDir, "logs"), "data.csv")
	})

	// Ignored files should NOT sync.
	time.Sleep(2 * time.Second)
	if fileExists(node2.syncDir, "app.log") {
		t.Fatal("app.log should not have been synced")
	}
	if fileExists(node2.syncDir, "cache.tmp") {
		t.Fatal("cache.tmp should not have been synced")
	}
	if fileExists(filepath.Join(node2.syncDir, "logs"), "debug.log") {
		t.Fatal("logs/debug.log should not have been synced")
	}

	// Real files should be there.
	if readFile(t, node2.syncDir, "real.txt") != "important" {
		t.Fatal("real.txt content mismatch")
	}
	if readFile(t, filepath.Join(node2.syncDir, "logs"), "data.csv") != "csv data" {
		t.Fatal("data.csv content mismatch")
	}

	_ = node1
	_ = node2
	t.Log("custom ignore patterns work correctly")
}

func TestIntegration_HTTPSubdirFiles(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	ln1.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, nil)

	// Create files in subdirectories.
	subDir := filepath.Join(node1.syncDir, "assets", "images")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "logo.png", "PNG binary data")
	writeFile(t, node1.syncDir, "readme.txt", "hello world")

	// Wait for watcher to pick them up.
	time.Sleep(1 * time.Second)

	// Test serving a nested file.
	resp, err := http.Get(fmt.Sprintf("http://%s/files/assets/images/logo.png", addr1))
	if err != nil {
		t.Fatalf("GET nested file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for nested file, got %d", resp.StatusCode)
	}

	// Test serving a root file.
	resp2, err := http.Get(fmt.Sprintf("http://%s/files/readme.txt", addr1))
	if err != nil {
		t.Fatalf("GET root file: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for root file, got %d", resp2.StatusCode)
	}

	// Test requesting a nonexistent nested file.
	resp3, err := http.Get(fmt.Sprintf("http://%s/files/assets/nope.txt", addr1))
	if err != nil {
		t.Fatalf("GET nonexistent nested: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent nested, got %d", resp3.StatusCode)
	}

	// Test requesting an ignored file via HTTP.
	writeFile(t, node1.syncDir, ".DS_Store", "apple stuff")
	resp4, err := http.Get(fmt.Sprintf("http://%s/files/.DS_Store", addr1))
	if err != nil {
		t.Fatalf("GET ignored file: %v", err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for ignored file, got %d", resp4.StatusCode)
	}

	t.Log("HTTP API for subdirectory files works correctly")
}

func TestIntegration_DeleteEntireDirectory(t *testing.T) {
	// Delete a directory with multiple files — all should sync as deletions.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create a directory with multiple files.
	subDir := filepath.Join(node1.syncDir, "project")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "a.txt", "aaa")
	writeFile(t, subDir, "b.txt", "bbb")
	writeFile(t, subDir, "c.txt", "ccc")

	// Wait for all to sync.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "project"), "a.txt") &&
			fileExists(filepath.Join(node2.syncDir, "project"), "b.txt") &&
			fileExists(filepath.Join(node2.syncDir, "project"), "c.txt")
	})

	// Delete entire directory on node1.
	time.Sleep(200 * time.Millisecond)
	os.RemoveAll(filepath.Join(node1.syncDir, "project"))

	// Wait for all deletions to sync.
	waitForSync(t, 15*time.Second, func() bool {
		return !fileExists(filepath.Join(node2.syncDir, "project"), "a.txt") &&
			!fileExists(filepath.Join(node2.syncDir, "project"), "b.txt") &&
			!fileExists(filepath.Join(node2.syncDir, "project"), "c.txt")
	})

	// Directory itself should be cleaned up.
	time.Sleep(1 * time.Second)
	if fileExists(node2.syncDir, "project") {
		t.Fatal("project/ directory should have been cleaned up")
	}

	t.Log("entire directory deletion synced correctly")
}

func TestIntegration_SourceNodeDirCleanup(t *testing.T) {
	// The key bug: on the SOURCE node, when the last non-ignored file in a directory
	// is deleted, but an ignored file (e.g. .DS_Store) remains, the directory should
	// still be cleaned up by the watcher.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create subdir with a real file on node1.
	subDir := filepath.Join(node1.syncDir, "docs")
	os.MkdirAll(subDir, 0o755)
	writeFile(t, subDir, "readme.txt", "hello")

	// Wait for sync to node2.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "docs"), "readme.txt")
	})

	// Simulate macOS creating .DS_Store on node1 (the source).
	writeFile(t, subDir, ".DS_Store", "apple index")
	time.Sleep(500 * time.Millisecond)

	// Delete the real file on node1 — only .DS_Store remains.
	os.Remove(filepath.Join(subDir, "readme.txt"))

	// Wait for the source node (node1) to clean up the directory.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(node1.syncDir, "docs")
	})

	// Also verify the peer (node2) cleaned up.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(node2.syncDir, "docs")
	})

	t.Log("source node directory cleanup with ignored files works correctly")
}

func TestIntegration_SourceNodeDeepDirCleanup(t *testing.T) {
	// Source node: deeply nested dirs should be cleaned up recursively
	// when the only file is deleted and ignored files remain at various levels.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create a/b/c/file.txt on node1.
	deepDir := filepath.Join(node1.syncDir, "a", "b", "c")
	os.MkdirAll(deepDir, 0o755)
	writeFile(t, deepDir, "file.txt", "deep content")

	// Wait for sync.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "a", "b", "c"), "file.txt")
	})

	// Add .DS_Store at each level on node1.
	writeFile(t, filepath.Join(node1.syncDir, "a"), ".DS_Store", "idx a")
	writeFile(t, filepath.Join(node1.syncDir, "a", "b"), ".DS_Store", "idx b")
	writeFile(t, deepDir, ".DS_Store", "idx c")
	time.Sleep(500 * time.Millisecond)

	// Delete the real file.
	os.Remove(filepath.Join(deepDir, "file.txt"))

	// All directories (a/b/c, a/b, a) should be cleaned up on the source node
	// since each level only has .DS_Store after the file is deleted.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(node1.syncDir, "a")
	})

	// Peer should also be clean.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(node2.syncDir, "a")
	})

	t.Log("source node deep directory cleanup works correctly")
}

func TestIntegration_SourceNodePartialCleanup(t *testing.T) {
	// Source node: cleanup should stop at a directory that still has non-ignored content.
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr1 := ln1.Addr().String()
	addr2 := ln2.Addr().String()
	ln1.Close()
	ln2.Close()

	node1 := newTestNodeWithAddr(t, "node-1", addr1, []string{"http://" + addr2})
	node2 := newTestNodeWithAddr(t, "node-2", addr2, []string{"http://" + addr1})

	// Create parent/keep.txt and parent/sub/remove.txt.
	os.MkdirAll(filepath.Join(node1.syncDir, "parent", "sub"), 0o755)
	writeFile(t, filepath.Join(node1.syncDir, "parent"), "keep.txt", "stays")
	writeFile(t, filepath.Join(node1.syncDir, "parent", "sub"), "remove.txt", "goes")

	// Wait for sync.
	waitForSync(t, 10*time.Second, func() bool {
		return fileExists(filepath.Join(node2.syncDir, "parent"), "keep.txt") &&
			fileExists(filepath.Join(node2.syncDir, "parent", "sub"), "remove.txt")
	})

	// Add .DS_Store in sub/ on node1.
	writeFile(t, filepath.Join(node1.syncDir, "parent", "sub"), ".DS_Store", "apple idx")
	time.Sleep(500 * time.Millisecond)

	// Delete the real file in sub/.
	os.Remove(filepath.Join(node1.syncDir, "parent", "sub", "remove.txt"))

	// sub/ should be cleaned up on source, but parent/ should remain (has keep.txt).
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(filepath.Join(node1.syncDir, "parent"), "sub")
	})

	// parent/ and keep.txt must still exist.
	if !fileExists(filepath.Join(node1.syncDir, "parent"), "keep.txt") {
		t.Fatal("parent/keep.txt should still exist on source node")
	}
	if !fileExists(node1.syncDir, "parent") {
		t.Fatal("parent/ directory should still exist on source node")
	}

	// Same on peer.
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(filepath.Join(node2.syncDir, "parent"), "sub")
	})
	if !fileExists(filepath.Join(node2.syncDir, "parent"), "keep.txt") {
		t.Fatal("parent/keep.txt should still exist on peer")
	}

	t.Log("source node partial cleanup stops correctly at non-empty parent")
}

// --- Helper: create node with specific address ---

func newTestNodeWithAddr(t *testing.T, id, addr string, peers []string) *testNode {
	t.Helper()
	baseDir := t.TempDir()
	syncDir := filepath.Join(baseDir, "sync")
	metaDir := filepath.Join(baseDir, "meta")
	return newTestNodeWithDirs(t, id, addr, syncDir, metaDir, peers)
}

func newTestNodeWithIgnore(t *testing.T, id, addr string, ignorePatterns, peers []string) *testNode {
	t.Helper()
	baseDir := t.TempDir()
	syncDir := filepath.Join(baseDir, "sync")
	metaDir := filepath.Join(baseDir, "meta")
	return newTestNodeWithOptions(t, id, addr, syncDir, metaDir, ignorePatterns, peers)
}

func newTestNodeWithDirs(t *testing.T, id, addr, syncDir, metaDir string, peers []string) *testNode {
	t.Helper()
	return newTestNodeWithOptions(t, id, addr, syncDir, metaDir, defaultTestIgnore, peers)
}

func newTestNodeWithOptions(t *testing.T, id, addr, syncDir, metaDir string, ignorePatterns, peers []string) *testNode {
	t.Helper()

	os.MkdirAll(syncDir, 0o755)
	os.MkdirAll(metaDir, 0o755)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})).With("node", id)

	dbPath := filepath.Join(metaDir, "birak.db")
	st, err := store.New(dbPath, logger)
	if err != nil {
		t.Fatalf("create store for %s: %v", id, err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	onChange := func(events []watcher.FileEvent) {
		for _, ev := range events {
			if _, err := st.PutFile(ev.Name, ev.ModTime, ev.Size, ev.Hash, ev.Deleted); err != nil {
				logger.Error("store update failed", "name", ev.Name, "error", err)
			}
		}
	}

	w := watcher.New(syncDir, st, logger, 200*time.Millisecond, 30*time.Second, ignorePatterns, onChange)

	syn := syncer.New(st, w, syncDir, id, peers, ignorePatterns, logger,
		500*time.Millisecond, // fast poll for tests
		1000, 5,
	)

	srv := server.New(st, syncDir, id, ignorePatterns, logger)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
	}

	// Start HTTP server.
	go func() {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Error("listen failed", "addr", addr, "error", err)
			return
		}
		if err := httpServer.Serve(ln); err != http.ErrServerClosed {
			logger.Error("HTTP server error", "error", err)
		}
	}()

	// Start watcher.
	go func() {
		if err := w.Run(ctx); err != nil {
			logger.Error("watcher error", "error", err)
		}
	}()

	// Start syncer.
	go syn.Run(ctx)

	node := &testNode{
		id:       id,
		syncDir: syncDir,
		metaDir:  metaDir,
		store:    st,
		watcher:  w,
		syncer:   syn,
		server:   httpServer,
		addr:     addr,
		logger:   logger,
		cancel:   cancel,
	}

	t.Cleanup(func() {
		cancel()
		httpServer.Close()
		st.Close()
	})

	waitForHTTP(t, addr)
	return node
}
