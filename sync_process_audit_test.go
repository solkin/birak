package birak_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

// This is an actual daemon-process test: SIGKILL, persistent SQLite/filesystem,
// writes and deletes during a peer outage, and restart using the same identity.
// It does not simulate a kernel/power failure or a dropped filesystem cache.
func TestAuditProcessKillAndOfflineMutationsConverge(t *testing.T) {
	binary := os.Getenv("BIRAK_AUDIT_BINARY")
	if binary == "" {
		binary = filepath.Join(t.TempDir(), "birakd")
		build := exec.Command("go", "build", "-race", "-o", binary, "./cmd/birakd")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build daemon: %v\n%s", err, output)
		}
	}
	addresses := make([]string, 2)
	for i := range addresses {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addresses[i] = listener.Addr().String()
		listener.Close()
	}
	roots := []string{t.TempDir(), t.TempDir()}
	processes := make([]*exec.Cmd, 2)
	for i := range roots {
		if err := os.Mkdir(filepath.Join(roots[i], "sync"), 0o755); err != nil {
			t.Fatal(err)
		}
		config := fmt.Sprintf(`node_id: "audit-%d"
sync_dir: %q
meta_dir: %q
listen_addr: %q
peers: [%q]
cluster_secret: "audit-only-secret"
sync:
  poll_interval: 30ms
  batch_limit: 17
  max_concurrent_downloads: 3
  scan_interval: 100ms
  debounce_window: 5ms
  repair_interval: 50ms
  reconcile_interval: 200ms
`, i, filepath.Join(roots[i], "sync"), filepath.Join(roots[i], "meta"), addresses[i], "http://"+addresses[1-i])
		if err := os.WriteFile(filepath.Join(roots[i], "config.yaml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stop := func(i int) {
		if processes[i] != nil {
			_ = processes[i].Process.Kill()
			_ = processes[i].Wait()
			processes[i] = nil
		}
	}
	t.Cleanup(func() {
		for i := range roots {
			stop(i)
		}
		for _, root := range roots {
			data, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
			if bytes.Contains(data, []byte("WARNING: DATA RACE")) {
				t.Error("child daemon reported a data race")
			}
		}
		if t.Failed() {
			for i, root := range roots {
				log, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
				if len(log) > 8000 {
					log = log[len(log)-8000:]
				}
				t.Logf("node %d last log bytes:\n%s", i, log)
			}
		}
	})
	start := func(i int) {
		log, err := os.OpenFile(filepath.Join(roots[i], "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-config", filepath.Join(roots[i], "config.yaml"))
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "BIRAK_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		log.Close()
		processes[i] = cmd
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	get := func(i int, path string, out any) bool {
		req, err := http.NewRequest(http.MethodGet, "http://"+addresses[i]+path, nil)
		if err != nil {
			return false
		}
		req.Header.Set(server.HeaderSecret, "audit-only-secret")
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(out) == nil
	}
	start(0)
	start(1)
	waitFor(t, 10*time.Second, "both daemon processes to start", func() bool {
		var status server.StatusResponse
		return get(0, "/status", &status) && get(1, "/status", &status)
	})
	expected := make(map[string][]byte)
	write := func(i int, generation string) {
		name := fmt.Sprintf("file-%03d", i)
		body := bytes.Repeat([]byte(fmt.Sprintf("%s-%03d\n", generation, i)), 8192)
		expected[name] = body
		tmp := filepath.Join(roots[0], "sync", ".birak-tmp-audit")
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(roots[0], "sync", name)); err != nil {
			t.Fatal(err)
		}
	}
	matches := func(i int) bool {
		count := 0
		err := filepath.WalkDir(filepath.Join(roots[i], "sync"), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if strings.HasPrefix(entry.Name(), ".birak") {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			want, exists := expected[entry.Name()]
			if !exists || !bytes.Equal(body, want) {
				return fmt.Errorf("unexpected bytes: %s", path)
			}
			count++
			return nil
		})
		return err == nil && count == len(expected)
	}
	for i := range 128 {
		write(i, "initial")
	}
	waitFor(t, 30*time.Second, "initial 128 files with exact bytes", func() bool { return matches(0) && matches(1) })
	stop(1) // Abrupt process termination, no graceful flushing.
	for i := range 32 {
		write(i, "modified")
	}
	for i := 32; i < 64; i++ {
		name := fmt.Sprintf("file-%03d", i)
		delete(expected, name)
		if err := os.Remove(filepath.Join(roots[0], "sync", name)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 128; i < 192; i++ {
		write(i, "offline-created")
	}
	waitFor(t, 10*time.Second, "all offline deletions indexed", func() bool {
		for i := 32; i < 64; i++ {
			var meta store.FileMeta
			if !get(0, fmt.Sprintf("/meta/file-%03d", i), &meta) || !meta.Deleted {
				return false
			}
		}
		return true
	})
	start(1)
	waitFor(t, 30*time.Second, "restart converges to all 160 expected files and deletions", func() bool { return matches(0) && matches(1) })
	waitFor(t, 10*time.Second, "replication health and repair backlog settle", func() bool {
		for i := range roots {
			var status server.StatusResponse
			if !get(i, "/status", &status) || status.Repairs.Total != 0 || len(status.Peers) != 1 || !status.Peers[0].Healthy || status.Peers[0].Lag != 0 {
				return false
			}
		}
		return true
	})
	t.Log("two real daemons converged after SIGKILL: 128 initial files, 32 updates, 32 deletes, 64 offline creates; 160 final files, exact bytes, no repairs")
}
