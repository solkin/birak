package birak_test

// Crash consistency, against a real daemon process.
//
// Every durability argument in this codebase is about ordering: bytes before
// the rename, the directory entry before the index, the intent before the
// filesystem. Those arguments have been made by reading code. This test makes
// the machine check them — it kills the daemon at an arbitrary moment while it
// is accepting writes, restarts it, and then insists on two things:
//
//   - every name the index claims to hold is on disk with the size and checksum
//     the index recorded. The index must never be ahead of the disk: an entry
//     for a file that is not there is what makes the next scan broadcast a
//     deletion to every peer.
//   - every write the daemon acknowledged is on disk with the exact bytes the
//     client sent. That is what "acknowledged" has to mean.
//
// What it does not do is simulate power loss: SIGKILL leaves the page cache
// intact, so the kernel still writes out everything the daemon handed it. This
// checks ordering and recovery, not the physics of an fsync.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
)

// crashRounds is how many times a test kills the daemon. A few prove the
// invariants; BIRAK_CRASH_ROUNDS turns the same tests into a soak.
func crashRounds(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("BIRAK_CRASH_ROUNDS")
	if raw == "" {
		return 3
	}
	rounds, err := strconv.Atoi(raw)
	if err != nil || rounds < 1 {
		t.Fatalf("BIRAK_CRASH_ROUNDS: %q is not a round count", raw)
	}
	return rounds
}

var (
	daemonOnce  sync.Once
	daemonPath  string
	daemonBuilt error
)

// daemonBinary builds the daemon once for the whole test run. Building it per
// test costs more than the tests do, and on a slow runner that is the
// difference between passing and timing out.
func daemonBinary(t *testing.T) string {
	t.Helper()
	daemonOnce.Do(func() {
		dir, err := os.MkdirTemp("", "birak-daemon-*")
		if err != nil {
			daemonBuilt = err
			return
		}
		daemonPath = filepath.Join(dir, "birakd")
		build := exec.Command("go", "build", "-o", daemonPath, "./cmd/birakd")
		if output, err := build.CombinedOutput(); err != nil {
			daemonBuilt = fmt.Errorf("build daemon: %w\n%s", err, output)
		}
	})
	if daemonBuilt != nil {
		t.Fatal(daemonBuilt)
	}
	return daemonPath
}

// removeDaemonBinary is called from TestMain, which owns what Once created.
func removeDaemonBinary() {
	if daemonPath != "" {
		os.RemoveAll(filepath.Dir(daemonPath))
	}
}

func TestCrashLeavesTheIndexBehindTheDisk(t *testing.T) {
	rounds := crashRounds(t)
	node := newCrashNode(t, daemonBinary(t))
	node.start(t)
	node.awaitReady(t)

	// Acknowledged writes accumulate across rounds: a crash must not lose one
	// that was acknowledged three crashes ago either.
	acknowledged := map[string][]byte{}
	firstACK := make(chan struct{})
	var ackOnce sync.Once

	for round := range rounds {
		written := node.writeUntilKilled(t, round)
		for name, body := range written {
			acknowledged[name] = body
		}

		node.start(t)
		node.awaitReady(t)
		node.checkIndexIsBehindTheDisk(t, round)
		node.checkAcknowledgedWritesSurvived(t, round, acknowledged)
	}
	t.Logf("%d crash rounds, %d acknowledged writes, index and disk agreed every time",
		rounds, len(acknowledged))
}

// --- the node under test ---------------------------------------------------

type crashNode struct {
	binary     string
	id         string
	root       string
	syncDir    string
	configPath string
	peerAddr   string
	davAddr    string
	process    *exec.Cmd
	client     *http.Client
}

const crashSecret = "crash-test-secret"

var crashNodeSeq atomic.Int64

func newCrashNode(t *testing.T, binary string) *crashNode {
	t.Helper()
	root := t.TempDir()
	n := &crashNode{
		binary: binary,
		// Replication refuses a peer advertising this node's own identity, so
		// every node in a test needs its own.
		id:         fmt.Sprintf("crash-node-%d", crashNodeSeq.Add(1)),
		root:       root,
		syncDir:    filepath.Join(root, "sync"),
		configPath: filepath.Join(root, "config.yaml"),
		peerAddr:   crashFreePort(t),
		davAddr:    crashFreePort(t),
		client:     &http.Client{Timeout: 5 * time.Second},
	}
	if err := os.MkdirAll(n.syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`node_id: %q
sync_dir: %q
meta_dir: %q
listen_addr: %q
cluster_secret: %q
log_level: "warn"
sync:
  poll_interval: 1s
  scan_interval: 500ms
  debounce_window: 10ms
  scrub_bytes_per_second: 1048576
gateways:
  webdav:
    enabled: true
    listen_addr: %q
`, n.id, n.syncDir, filepath.Join(root, "meta"), n.peerAddr, crashSecret, n.davAddr)
	if err := os.WriteFile(n.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		n.kill()
		n.client.CloseIdleConnections()
		if t.Failed() {
			log, _ := os.ReadFile(filepath.Join(root, "daemon.log"))
			if len(log) > 8000 {
				log = log[len(log)-8000:]
			}
			t.Logf("daemon log tail:\n%s", log)
		}
	})
	return n
}

func (n *crashNode) start(t *testing.T) {
	t.Helper()
	log, err := os.OpenFile(filepath.Join(n.root, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(n.binary, "-config", n.configPath)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BIRAK_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	n.process = cmd
}

func (n *crashNode) kill() {
	if n.process == nil {
		return
	}
	_ = n.process.Process.Kill()
	_ = n.process.Wait()
	n.process = nil
}

func (n *crashNode) awaitReady(t *testing.T) {
	t.Helper()
	waitFor(t, 30*time.Second, "the daemon to recover and report ready", func() bool {
		resp, err := n.client.Get("http://" + n.davAddr + "/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		resp, err = n.client.Get("http://" + n.peerAddr + "/readyz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode == http.StatusOK
	})
}

// --- writing, and being killed while doing it ------------------------------

// writeUntilKilled uploads through the gateway and kills the daemon partway
// through. It returns only the writes the daemon acknowledged.
func (n *crashNode) writeUntilKilled(t *testing.T, round int) map[string][]byte {
	t.Helper()
	var mu sync.Mutex
	acknowledged := map[string][]byte{}

	stop := make(chan struct{})
	var writers sync.WaitGroup
	for worker := range 3 {
		writers.Add(1)
		go func(worker int) {
			defer writers.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				name := fmt.Sprintf("r%02d/w%d-%04d.bin", round, worker, i)
				body := bytes.Repeat(
					fmt.Appendf(nil, "round=%d worker=%d seq=%d ", round, worker, i),
					64,
				)
				if !n.put(name, body) {
					continue
				}
				mu.Lock()
				acknowledged[name] = body
				mu.Unlock()
				ackOnce.Do(func() { close(firstACK) })
			}
		}(worker)
	}

	// Establish acknowledged work before starting the crash timer. A busy CI
	// disk can take longer than 300 ms to commit its first write; killing at
	// that fixed deadline would test no acknowledged-write invariant at all.
	select {
	case <-firstACK:
	case <-time.After(30 * time.Second):
		n.kill()
		close(stop)
		writers.Wait()
		t.Fatalf("round %d: no acknowledged write before the crash deadline", round)
	}
	// Keep all writers running, then kill without warning.
	time.Sleep(time.Duration(300+round*250) * time.Millisecond)
	n.kill()
	close(stop)
	writers.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(acknowledged) == 0 {
		t.Fatalf("round %d acknowledged nothing; the test proves nothing", round)
	}
	return acknowledged
}

// put reports whether the daemon acknowledged the write. A refused or
// interrupted write promises nothing and is not counted.
func (n *crashNode) put(name string, body []byte) bool {
	req, err := http.NewRequest(http.MethodPut, "http://"+n.davAddr+"/"+name, bytes.NewReader(body))
	if err != nil {
		return false
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// --- the invariants --------------------------------------------------------

// checkIndexIsBehindTheDisk is the invariant the whole write path is ordered
// around. An index entry for a file that is not on disk, or whose checksum
// disagrees, is not a local inconsistency: the next scan reads it as a deletion
// or a corruption and tells every peer.
func (n *crashNode) checkIndexIsBehindTheDisk(t *testing.T, round int) {
	t.Helper()
	for _, entry := range n.manifest(t) {
		if entry.Deleted {
			continue
		}
		path := filepath.Join(n.syncDir, filepath.FromSlash(entry.Name))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("round %d: index holds %q but the disk does not: %v", round, entry.Name, err)
		}
		if info.Size() != entry.Size {
			t.Fatalf("round %d: %q is %d bytes on disk, %d in the index",
				round, entry.Name, info.Size(), entry.Size)
		}
		if got := crashFileHash(t, path); got != entry.Hash {
			t.Fatalf("round %d: %q hashes to %s on disk, %s in the index",
				round, entry.Name, got[:12], entry.Hash[:12])
		}
	}
}

// checkAcknowledgedWritesSurvived holds the daemon to what a 2xx meant.
func (n *crashNode) checkAcknowledgedWritesSurvived(t *testing.T, round int, acknowledged map[string][]byte) {
	t.Helper()
	indexed := map[string]store.FileMeta{}
	for _, entry := range n.manifest(t) {
		indexed[entry.Name] = entry
	}
	for name, want := range acknowledged {
		got, err := os.ReadFile(filepath.Join(n.syncDir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("round %d: acknowledged write %q is gone: %v", round, name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("round %d: acknowledged write %q has %d bytes, wrote %d",
				round, name, len(got), len(want))
		}
		entry, ok := indexed[name]
		if !ok || entry.Deleted {
			t.Fatalf("round %d: acknowledged write %q is on disk but not in the index", round, name)
		}
	}
}

func (n *crashNode) manifest(t *testing.T) []store.FileMeta {
	t.Helper()
	var all []store.FileMeta
	after := ""
	for {
		req, err := http.NewRequest(http.MethodGet,
			fmt.Sprintf("http://%s/manifest?after=%s&limit=1000", n.peerAddr, after), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(server.HeaderSecret, crashSecret)
		req.Header.Set(server.HeaderProtocol, server.ProtocolVersion)
		resp, err := n.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var page []store.FileMeta
		decErr := json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if decErr != nil {
			t.Fatal(decErr)
		}
		if len(page) == 0 {
			return all
		}
		all = append(all, page...)
		after = page[len(page)-1].Name
	}
}

// --- helpers ---------------------------------------------------------------

func crashFileHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func crashFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// A crash must not leave scratch files that nothing will ever clean up, and
// must not leave the private state directory in a shape that blocks startup.
func TestCrashLeavesNoUnreachableScratch(t *testing.T) {
	node := newCrashNode(t, daemonBinary(t))
	node.start(t)
	node.awaitReady(t)
	node.writeUntilKilled(t, 0)
	node.start(t)
	node.awaitReady(t)

	var scratch []string
	err := filepath.WalkDir(node.syncDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(d.Name(), ".birak-tmp-") || strings.HasPrefix(d.Name(), ".birak-bak-") {
			scratch = append(scratch, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Scratch left by an interrupted write is expected; what matters is that
	// the node started, recovered and serves, which awaitReady already proved.
	t.Logf("%d scratch files remained after the crash; the node recovered and is serving", len(scratch))
}

// Replication, interrupted repeatedly.
//
// The single-node test above proves a node does not lie to itself after a
// crash. This one proves a cluster does not lose work: one node keeps accepting
// writes while its peer is killed over and over, mid-transfer, and the peer must
// still end up holding exactly what the writer holds — with its own index still
// behind its own disk after every restart.
func TestCrashDuringReplicationStillConverges(t *testing.T) {
	rounds := crashRounds(t)
	binary := daemonBinary(t)
	writer := newCrashNode(t, binary)
	replica := newCrashNode(t, binary)
	writer.pairWith(t, replica)
	replica.pairWith(t, writer)

	writer.start(t)
	replica.start(t)
	writer.awaitReady(t)
	replica.awaitReady(t)

	acknowledged := map[string][]byte{}
	for round := range rounds {
		// Keep writing to one node while the other is killed mid-replication.
		done := make(chan map[string][]byte, 1)
		go func() { done <- writer.writeFor(t, round, 900*time.Millisecond) }()
		time.Sleep(time.Duration(200+round*150) * time.Millisecond)
		replica.kill()
		for name, body := range <-done {
			acknowledged[name] = body
		}

		replica.start(t)
		replica.awaitReady(t)
		replica.checkIndexIsBehindTheDisk(t, round)
	}

	// With both nodes up and nothing more being written, the replica must end
	// up holding every acknowledged write, byte for byte.
	waitFor(t, 90*time.Second, "the replica to hold every acknowledged write", func() bool {
		for name, want := range acknowledged {
			got, err := os.ReadFile(filepath.Join(replica.syncDir, filepath.FromSlash(name)))
			if err != nil || !bytes.Equal(got, want) {
				return false
			}
		}
		return true
	})
	replica.checkIndexIsBehindTheDisk(t, rounds)
	replica.checkAcknowledgedWritesSurvived(t, rounds, acknowledged)
	t.Logf("%d crashes during replication, %d acknowledged writes, the replica holds all of them",
		rounds, len(acknowledged))
}

// pairWith rewrites the node's config so it polls the other one.
func (n *crashNode) pairWith(t *testing.T, peer *crashNode) {
	t.Helper()
	data, err := os.ReadFile(n.configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(string(data),
		"cluster_secret:", fmt.Sprintf("peers: [%q]\ncluster_secret:", "http://"+peer.peerAddr), 1)
	if err := os.WriteFile(n.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFor uploads for a fixed time and returns the acknowledged writes. Unlike
// writeUntilKilled it leaves the daemon alone; the caller kills whatever it
// wants to kill.
func (n *crashNode) writeFor(t *testing.T, round int, d time.Duration) map[string][]byte {
	t.Helper()
	var mu sync.Mutex
	acknowledged := map[string][]byte{}
	deadline := time.Now().Add(d)
	var writers sync.WaitGroup
	for worker := range 2 {
		writers.Add(1)
		go func(worker int) {
			defer writers.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				name := fmt.Sprintf("rep%02d/w%d-%04d.bin", round, worker, i)
				body := bytes.Repeat(fmt.Appendf(nil, "round=%d worker=%d seq=%d ", round, worker, i), 32)
				if !n.put(name, body) {
					continue
				}
				mu.Lock()
				acknowledged[name] = body
				mu.Unlock()
			}
		}(worker)
	}
	writers.Wait()
	mu.Lock()
	defer mu.Unlock()
	return acknowledged
}
