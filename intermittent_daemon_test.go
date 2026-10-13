//go:build unix

package birak_test

// Nodes that restart, drop off the network, or sit behind slow links, against
// real daemon processes. Each scenario here is one a production review found
// expensive or stuck:
//
//   - restarting a peer replayed its whole stream through the repair queue;
//   - an interrupted 500 MB transfer started again from zero;
//   - one case-colliding file made a node unready, and after a restart kept
//     replication from starting;
//   - SIGTERM waited for a bulk import of tens of thousands of files.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/birak/birak/internal/server"
)

// peerProxy sits between a node and its peer. It counts requests, records the
// Range of every file request, and can cut or throttle file transfers.
type peerProxy struct {
	*httptest.Server
	cutAt     atomic.Int64 // abort the next file body after this many bytes
	rate      atomic.Int64 // bytes per second for file bodies; 0 = unthrottled
	forwarded atomic.Int64 // file body bytes passed on

	mu     sync.Mutex
	counts map[string]int
	ranges []string
}

func newPeerProxy(t *testing.T, target string) *peerProxy {
	t.Helper()
	p := &peerProxy{counts: map[string]int{}}
	upstream, err := url.Parse("http://" + target)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(upstream)
	rp.ErrorLog = log.New(io.Discard, "", 0)
	rp.FlushInterval = -1
	rp.ModifyResponse = func(resp *http.Response) error {
		if strings.HasPrefix(resp.Request.URL.Path, "/files/") {
			resp.Body = &proxyBody{ReadCloser: resp.Body, p: p, cut: p.cutAt.Swap(0)}
		}
		return nil
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		p.mu.Lock()
		p.counts[kind]++
		if kind == "files" {
			p.ranges = append(p.ranges, r.Header.Get("Range"))
		}
		p.mu.Unlock()
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *peerProxy) count(kind string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[kind]
}

func (p *peerProxy) fileRanges() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ranges...)
}

type proxyBody struct {
	io.ReadCloser
	p    *peerProxy
	cut  int64
	sent int64
}

var errCutByTest = errors.New("connection cut by the test")

func (b *proxyBody) Read(buf []byte) (int, error) {
	if b.cut > 0 {
		if b.sent >= b.cut {
			return 0, errCutByTest
		}
		buf = buf[:min(int64(len(buf)), b.cut-b.sent)]
	}
	rate := b.p.rate.Load()
	if rate > 0 {
		buf = buf[:min(len(buf), 64<<10)]
	}
	n, err := b.ReadCloser.Read(buf)
	b.sent += int64(n)
	b.p.forwarded.Add(int64(n))
	if rate > 0 && n > 0 {
		time.Sleep(time.Duration(int64(n) * int64(time.Second) / rate))
	}
	return n, err
}

// pairWithURL points the node at a peer address of the test's choosing.
func (n *crashNode) pairWithURL(t *testing.T, peerURL string) {
	t.Helper()
	data, err := os.ReadFile(n.configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(string(data),
		"cluster_secret:", fmt.Sprintf("peers: [%q]\ncluster_secret:", peerURL), 1)
	if err := os.WriteFile(n.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

// terminate stops the daemon the way an orchestrator does and reports how
// long it took to exit.
func (n *crashNode) terminate(t *testing.T) time.Duration {
	t.Helper()
	started := time.Now()
	if err := n.process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- n.process.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		n.process.Process.Kill()
		<-done
		n.process = nil
		t.Fatal("daemon did not exit within 60s of SIGTERM")
	}
	n.process = nil
	return time.Since(started)
}

func (n *crashNode) get(t *testing.T, path string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+n.peerAddr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(server.HeaderSecret, crashSecret)
	resp, err := n.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, resp.StatusCode, body)
	}
	return body
}

func (n *crashNode) status(t *testing.T) server.StatusResponse {
	t.Helper()
	var status server.StatusResponse
	if err := json.Unmarshal(n.get(t, "/status"), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

// metric reads one sample, matched by its full name and labels.
func (n *crashNode) metric(t *testing.T, series string) float64 {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(string(n.get(t, "/metrics"))))
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), " ")
		if ok && name == series {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("metric %s not exposed", series)
	return 0
}

func (n *crashNode) ready() bool {
	resp, err := n.client.Get("http://" + n.peerAddr + "/readyz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func fileHash(t *testing.T, path string) (string, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(sum.Sum(nil)), true
}

// Restarting a peer resets every other node's cursor on it, so they all replay
// its whole stream. On converged names that replay must not touch the repair
// queue or ask the peer anything beyond the pages themselves.
func TestDaemonPeerRestartReplaysWithoutRepairWork(t *testing.T) {
	const names = 1000
	binary := daemonBinary(t)
	origin := newCrashNode(t, binary)
	replica := newCrashNode(t, binary)
	for i := range names {
		path := filepath.Join(origin.syncDir, fmt.Sprintf("d%02d", i%20), fmt.Sprintf("img-%04d.png", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf("image %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proxy := newPeerProxy(t, origin.peerAddr)
	replica.pairWithURL(t, proxy.URL)
	origin.pairWith(t, replica)
	origin.start(t)
	replica.start(t)
	origin.awaitReady(t)
	replica.awaitReady(t)

	waitFor(t, 3*time.Minute, "the replica to converge", func() bool {
		status := replica.status(t)
		return status.FileCount == names && status.Repairs.Total == 0
	})
	// The origin reads the replica's stream too: every file it just served comes
	// back to it there, and it already holds all of them.
	waitFor(t, time.Minute, "the origin to read the replica's stream", func() bool {
		peers := origin.status(t).Peers
		return len(peers) == 1 && peers[0].Cursor > 0 && peers[0].Cursor >= peers[0].PeerMaxVersion
	})
	if echoed := origin.metric(t, "birak_repairs_enqueued_total"); echoed != 0 {
		t.Fatalf("the origin queued %v of its own files back from the replica", echoed)
	}

	enqueued := replica.metric(t, "birak_repairs_enqueued_total")
	metas, files := proxy.count("meta"), proxy.count("files")
	before := replica.status(t).Peers[0]

	if took := origin.terminate(t); took > 10*time.Second {
		t.Fatalf("origin took %v to stop", took)
	}
	origin.start(t)
	origin.awaitReady(t)
	incarnation := origin.status(t).Epoch
	waitFor(t, time.Minute, "the replica to replay the restarted origin", func() bool {
		peers := replica.status(t).Peers
		return len(peers) == 1 && peers[0].Epoch == incarnation &&
			peers[0].Cursor >= names && peers[0].Cursor >= peers[0].PeerMaxVersion
	})
	if before.Epoch == incarnation {
		t.Fatal("the origin kept its incarnation across a restart; nothing was replayed")
	}

	if got := replica.metric(t, "birak_repairs_enqueued_total") - enqueued; got != 0 {
		t.Fatalf("replaying %d converged names wrote %v repair rows", names, got)
	}
	if got := proxy.count("meta") - metas; got != 0 {
		t.Fatalf("replaying %d converged names made %d /meta requests", names, got)
	}
	if got := proxy.count("files") - files; got != 0 {
		t.Fatalf("replaying %d converged names downloaded %d files", names, got)
	}
	t.Logf("replayed %d names after a peer restart: 0 queued rows, 0 /meta, 0 downloads; %d change pages",
		names, proxy.count("changes"))
}

func writeRandomFile(t *testing.T, path string, size int) string {
	t.Helper()
	body := make([]byte, size)
	rand.New(rand.NewSource(int64(size))).Read(body)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func resumedFrom(t *testing.T, ranges []string) int64 {
	t.Helper()
	if len(ranges) != 2 || ranges[0] != "" || !strings.HasPrefix(ranges[1], "bytes=") || !strings.HasSuffix(ranges[1], "-") {
		t.Fatalf("file requests carried ranges %q, want a full request then one resume", ranges)
	}
	offset, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(ranges[1], "bytes="), "-"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return offset
}

// A transfer cut half-way by the network continues from where it stopped.
func TestDaemonResumesDownloadAfterNetworkCut(t *testing.T) {
	const size = 48 << 20
	binary := daemonBinary(t)
	origin := newCrashNode(t, binary)
	replica := newCrashNode(t, binary)
	hash := writeRandomFile(t, filepath.Join(origin.syncDir, "apps", "big.apk"), size)
	proxy := newPeerProxy(t, origin.peerAddr)
	proxy.cutAt.Store(size / 2)
	replica.pairWithURL(t, proxy.URL)
	origin.start(t)
	origin.awaitReady(t)
	replica.start(t)

	target := filepath.Join(replica.syncDir, "apps", "big.apk")
	waitFor(t, 2*time.Minute, "the replica to hold the file", func() bool {
		got, ok := fileHash(t, target)
		return ok && got == hash
	})
	offset := resumedFrom(t, proxy.fileRanges())
	if offset < size/2-(256<<10) || offset > size/2 {
		t.Fatalf("resumed from byte %d after a cut at %d", offset, size/2)
	}
	if moved := proxy.forwarded.Load(); moved > size+(1<<20) {
		t.Fatalf("%d bytes crossed the link for a %d-byte file", moved, size)
	}
	if resumed := replica.metric(t, "birak_downloads_resumed_total"); resumed != 1 {
		t.Fatalf("resumed downloads=%v", resumed)
	}
	t.Logf("cut at %d, resumed from %d; %d bytes crossed the link", size/2, offset, proxy.forwarded.Load())
}

// A transfer interrupted by a restart continues after it: a graceful stop
// records the exact position, a crash the last checkpoint.
func TestDaemonResumesDownloadAfterRestart(t *testing.T) {
	const size = 48 << 20
	for _, graceful := range []bool{true, false} {
		t.Run(map[bool]string{true: "SIGTERM", false: "SIGKILL"}[graceful], func(t *testing.T) {
			binary := daemonBinary(t)
			origin := newCrashNode(t, binary)
			replica := newCrashNode(t, binary)
			hash := writeRandomFile(t, filepath.Join(origin.syncDir, "apps", "big.apk"), size)
			proxy := newPeerProxy(t, origin.peerAddr)
			proxy.rate.Store(16 << 20)
			replica.pairWithURL(t, proxy.URL)
			origin.start(t)
			origin.awaitReady(t)
			replica.start(t)

			waitFor(t, time.Minute, "half of the file to cross the link", func() bool {
				return proxy.forwarded.Load() >= size/2
			})
			stoppedAt := proxy.forwarded.Load()
			if graceful {
				if took := replica.terminate(t); took > 10*time.Second {
					t.Fatalf("replica took %v to stop mid-transfer", took)
				}
			} else {
				replica.kill()
			}
			proxy.rate.Store(0)
			replica.start(t)

			target := filepath.Join(replica.syncDir, "apps", "big.apk")
			waitFor(t, 2*time.Minute, "the replica to hold the file", func() bool {
				got, ok := fileHash(t, target)
				return ok && got == hash
			})
			offset := resumedFrom(t, proxy.fileRanges())
			// What was forwarded may still sit in socket buffers at SIGTERM.
			floor := int64(size/2 - (4 << 20))
			if !graceful {
				// A crash keeps what the last checkpoint made durable.
				floor = 16 << 20
			}
			if offset < floor || offset > stoppedAt+(4<<20) {
				t.Fatalf("resumed from byte %d; the transfer had passed byte %d", offset, stoppedAt)
			}
			t.Logf("stopped near byte %d, resumed from %d", stoppedAt, offset)
		})
	}
}

// One stray spelling that collides with a reserved one must leave the node
// ready and replicating, and keep it so across a restart.
func TestDaemonCaseCollisionKeepsNodeReadyAndReplicating(t *testing.T) {
	binary := daemonBinary(t)
	node := newCrashNode(t, binary)
	peer := newCrashNode(t, binary)
	node.pairWith(t, peer)
	peer.pairWith(t, node)
	node.start(t)
	peer.start(t)
	node.awaitReady(t)
	peer.awaitReady(t)

	// 0.PNG next to an indexed 0.png, where the filesystem can hold both; a
	// spelling reserved by deletion history collides everywhere.
	stray := []string{"Photo.png"}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(node.syncDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("0.png", "indexed")
	write("photo.png", "deleted later")
	waitFor(t, time.Minute, "both names to reach the peer", func() bool {
		_, a := fileHash(t, filepath.Join(peer.syncDir, "0.png"))
		_, b := fileHash(t, filepath.Join(peer.syncDir, "photo.png"))
		return a && b
	})
	if err := os.Remove(filepath.Join(node.syncDir, "photo.png")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Minute, "the deletion to reach the peer", func() bool {
		_, ok := fileHash(t, filepath.Join(peer.syncDir, "photo.png"))
		return !ok
	})
	write("Photo.png", "stray")
	write("0.PNG", "stray")
	if data, _ := os.ReadFile(filepath.Join(node.syncDir, "0.png")); string(data) == "indexed" {
		stray = append(stray, "0.PNG")
	} else {
		t.Log("case-insensitive filesystem: 0.png and 0.PNG are one file here")
		write("0.png", "indexed")
	}

	check := func(when string) {
		t.Helper()
		waitFor(t, time.Minute, "the node to count the stray names "+when, func() bool {
			return node.metric(t, `birak_scan_skipped_files{reason="name_collision"}`) == float64(len(stray))
		})
		if !node.ready() {
			t.Fatalf("node unready %s: %+v", when, node.status(t).Local)
		}
		// Replication runs both ways.
		name := "after-" + strings.ReplaceAll(when, " ", "-")
		if err := os.WriteFile(filepath.Join(peer.syncDir, name+"-in"), []byte(when), 0o644); err != nil {
			t.Fatal(err)
		}
		write(name+"-out", when)
		waitFor(t, time.Minute, "replication "+when, func() bool {
			_, in := fileHash(t, filepath.Join(node.syncDir, name+"-in"))
			_, out := fileHash(t, filepath.Join(peer.syncDir, name+"-out"))
			return in && out
		})
	}
	check("with stray names")
	node.terminate(t)
	node.start(t)
	node.awaitReady(t)
	check("after a restart")
	for _, name := range stray {
		if _, ok := fileHash(t, filepath.Join(peer.syncDir, name)); ok {
			t.Fatalf("stray spelling %s replicated", name)
		}
	}
}

// SIGTERM in the middle of indexing a bulk import of tens of thousands of
// files must not wait for the rest of the import.
func TestDaemonStopsPromptlyDuringBulkIndexing(t *testing.T) {
	const files = 50_000
	node := newCrashNode(t, daemonBinary(t))
	node.start(t)
	node.awaitReady(t)
	for i := range files {
		path := filepath.Join(node.syncDir, fmt.Sprintf("bulk%03d", i/100), fmt.Sprintf("f%05d.bin", i))
		if i%100 == 0 {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, time.Minute, "indexing to start", func() bool {
		return node.status(t).FileCount > 0
	})
	if count := node.status(t).FileCount; count >= files {
		t.Fatalf("all %d files indexed before the test could interrupt", count)
	}
	took := node.terminate(t)
	if took > 5*time.Second {
		t.Fatalf("SIGTERM during bulk indexing took %v", took)
	}
	t.Logf("stopped in %v while indexing %d files", took, files)
}
