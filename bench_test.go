package birak_test

// A load stand, not a correctness test. Every other test here answers "is this
// right"; this one answers the questions that gate production and that no
// amount of functional testing can settle: what do writes cost while the node
// is verifying itself, how long does a peer take to catch up after it dies, and
// does the node keep reporting itself ready the whole time.
//
// It is opt-in and it writes real data at a real rate, so it never runs in CI
// by accident. Point it at the storage you intend to ship on — the numbers from
// a laptop's page cache mean nothing for a network volume.
//
//	BIRAK_BENCH_DIR=/data/bench go test -run TestBenchSyncUnderLoad -timeout 30m .
//
// Knobs, all optional:
//
//	BIRAK_BENCH_DIR        where to put both nodes (default: a temp dir)
//	BIRAK_BENCH_FILES      seed files per node          (default 2000)
//	BIRAK_BENCH_FILE_BYTES seed file size               (default 65536)
//	BIRAK_BENCH_LARGE      number of large files        (default 4)
//	BIRAK_BENCH_LARGE_BYTES size of a large file        (default 134217728)
//	BIRAK_BENCH_DURATION   load phase                   (default 60s)
//	BIRAK_BENCH_WRITERS    concurrent writers           (default 4)
//	BIRAK_BENCH_SCRUB      scrub budget, bytes/second   (default 8388608)

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/syncer"
	"github.com/birak/birak/internal/watcher"
)

type benchOptions struct {
	dir        string
	files      int
	fileBytes  int
	large      int
	largeBytes int
	duration   time.Duration
	writers    int
	scrubRate  int64
}

func benchOptionsFromEnv(t *testing.T) benchOptions {
	t.Helper()
	opt := benchOptions{
		dir:        os.Getenv("BIRAK_BENCH_DIR"),
		files:      benchInt(t, "BIRAK_BENCH_FILES", 2000),
		fileBytes:  benchInt(t, "BIRAK_BENCH_FILE_BYTES", 64<<10),
		large:      benchInt(t, "BIRAK_BENCH_LARGE", 4),
		largeBytes: benchInt(t, "BIRAK_BENCH_LARGE_BYTES", 128<<20),
		duration:   benchDuration(t, "BIRAK_BENCH_DURATION", time.Minute),
		writers:    benchInt(t, "BIRAK_BENCH_WRITERS", 4),
		scrubRate:  int64(benchInt(t, "BIRAK_BENCH_SCRUB", 8<<20)),
	}
	if opt.dir == "" {
		opt.dir = t.TempDir()
	}
	return opt
}

func benchInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		t.Fatalf("%s: %q is not a count", key, raw)
	}
	return n
}

func benchDuration(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		t.Fatalf("%s: %q is not a duration", key, raw)
	}
	return d
}

// benchNode is one daemon's worth of components, restartable on the same
// volume so a peer can be killed and brought back.
type benchNode struct {
	id      string
	addr    string
	syncDir string
	metaDir string
	peers   []string
	scrub   int64

	store  *store.Store
	sync   *syncer.Syncer
	server *http.Server
	cancel context.CancelFunc
	done   sync.WaitGroup
}

func (n *benchNode) start(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(n.syncDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(n.metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.New(filepath.Join(n.metaDir, "birak.db"), logger)
	if err != nil {
		t.Fatalf("open store for %s: %v", n.id, err)
	}
	w := watcher.New(n.syncDir, st, logger, 300*time.Millisecond, 5*time.Minute, nil)
	w.SetScrubRate(n.scrub)
	syn := syncer.New(st, w, n.syncDir, n.id, n.peers, nil, logger, syncer.Options{
		PollInterval:           time.Second,
		BatchLimit:             1000,
		MaxConcurrentDownloads: 5,
		RepairInterval:         5 * time.Second,
		ReconcileInterval:      time.Minute,
	})
	srv := server.New(st, n.syncDir, n.id, nil, server.Config{Stats: syn}, logger)

	ln, err := net.Listen("tcp", n.addr)
	if err != nil {
		t.Fatalf("listen for %s: %v", n.id, err)
	}
	httpServer := &http.Server{Handler: srv.Handler()}
	ctx, cancel := context.WithCancel(context.Background())

	n.store, n.sync, n.server, n.cancel = st, syn, httpServer, cancel
	n.done.Add(3)
	go func() { defer n.done.Done(); httpServer.Serve(ln) }()
	go func() { defer n.done.Done(); w.Run(ctx) }()
	go func() { defer n.done.Done(); syn.Run(ctx) }()
}

func (n *benchNode) stop() {
	n.cancel()
	n.server.Close()
	n.done.Wait()
	n.store.Close()
}

// ready reports what a load balancer would see right now.
func (n *benchNode) ready() bool {
	resp, err := http.Get("http://" + n.addr + "/readyz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func TestBenchSyncUnderLoad(t *testing.T) {
	if os.Getenv("BIRAK_BENCH_DIR") == "" && os.Getenv("BIRAK_BENCH") == "" {
		t.Skip("load stand: set BIRAK_BENCH_DIR (preferred) or BIRAK_BENCH=1")
	}
	opt := benchOptionsFromEnv(t)
	report := &benchReport{options: opt}
	t.Cleanup(func() { t.Log("\n" + report.String()) })

	addrA, addrB := benchFreeAddr(t), benchFreeAddr(t)
	nodeA := &benchNode{
		id: "bench-a", addr: addrA, scrub: opt.scrubRate,
		syncDir: filepath.Join(opt.dir, "a", "sync"),
		metaDir: filepath.Join(opt.dir, "a", "meta"),
		peers:   []string{"http://" + addrB},
	}
	nodeB := &benchNode{
		id: "bench-b", addr: addrB, scrub: opt.scrubRate,
		syncDir: filepath.Join(opt.dir, "b", "sync"),
		metaDir: filepath.Join(opt.dir, "b", "meta"),
		peers:   []string{"http://" + addrA},
	}

	// --- Phase 0: what this filesystem charges for a safe publish ----------
	// Without it the write latency below is uninterpretable: an operator cannot
	// tell Birak's cost from the cost of durably renaming a file on this disk.
	report.floor = benchPublishFloor(t, filepath.Join(opt.dir, "calibration"), opt.fileBytes)

	// --- Phase 1: seed and index -------------------------------------------
	// What an operator feels as "how long until a fresh node is usable".
	seedStart := time.Now()
	bytes := benchSeed(t, nodeA.syncDir, opt)
	report.seedBytes = bytes
	report.seedWrite = time.Since(seedStart)

	indexStart := time.Now()
	nodeA.start(t)
	t.Cleanup(nodeA.stop)
	benchAwait(t, 30*time.Minute, "node A to finish its first sweep", func() bool {
		return nodeA.ready()
	})
	report.indexTime = time.Since(indexStart)
	report.indexedFiles = benchFileCount(t, nodeA)

	// --- Phase 1b: what one write costs with nothing else happening -------
	// Without this the load numbers below are read as per-write cost, which is
	// how the first round of this stand reached the wrong conclusion. The gap
	// between the publish floor and this line is Birak's own overhead; the gap
	// between this line and the loaded figure is queueing.
	quiet := newBenchLoad(nodeA, benchOptions{
		dir: opt.dir, files: opt.files, fileBytes: opt.fileBytes, writers: 1,
	}, "quiet")
	quiet.run(t, 5*time.Second)
	report.quietWrite = quiet.writeLatency()

	nodeB.start(t)
	t.Cleanup(nodeB.stop)

	// --- Phase 2: sustained writes while the node verifies itself ----------
	// The commit lock is shared by gateway writes, indexing and replication;
	// the scrub reads every byte the node holds. This is where the two meet.
	load := newBenchLoad(nodeA, opt, "steady")
	load.run(t, opt.duration)
	report.write = load.writeLatency()
	report.largeWrite = load.largeWrite()
	report.lock = load.lockWait()
	report.writes = load.writes.Load()
	report.readyFailures = load.notReady.Load()
	report.scrubbed = benchScrubProgress(t, nodeA)
	report.lockLoad = fileops.LockStats(nodeA.syncDir)

	// --- Phase 3: a peer dies, falls behind, and catches up ----------------
	benchAwait(t, 30*time.Minute, "node B to catch up before the outage", func() bool {
		return benchDigest(t, nodeB) == benchDigest(t, nodeA)
	})
	nodeB.stop()

	outage := newBenchLoad(nodeA, opt, "outage")
	outage.run(t, opt.duration/2)
	report.writesDuringOutage = outage.writes.Load()

	// Agreement means identical state for every name, not an equal file count:
	// an outage that only overwrites existing names leaves the count untouched.
	target := benchDigest(t, nodeA)
	report.convergedFiles = benchFileCount(t, nodeA)
	recoverStart := time.Now()
	nodeB.start(t)
	benchAwait(t, 60*time.Minute, "node B to converge after the outage", func() bool {
		return benchDigest(t, nodeB) == target
	})
	report.convergence = time.Since(recoverStart)

	if report.readyFailures > 0 {
		t.Errorf("node reported not ready %d times while serving writes", report.readyFailures)
	}
}

// --- load generator --------------------------------------------------------

// benchLoad overwrites existing names and creates new ones, which is the mix
// that actually exercises the commit path: an overwrite has to observe the old
// generation before it can publish a new one.
type benchLoad struct {
	node *benchNode
	opt  benchOptions
	// generation keeps each phase's writes distinct. Without it a later phase
	// rewrites the same names with the same bytes and diverges from nothing.
	generation string

	mu        sync.Mutex
	latencies []time.Duration
	largeOnes []time.Duration
	lockWaits []time.Duration

	writes   atomic.Int64
	notReady atomic.Int64
}

func newBenchLoad(node *benchNode, opt benchOptions, generation string) *benchLoad {
	return &benchLoad{node: node, opt: opt, generation: generation}
}

func (l *benchLoad) run(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	payload := make([]byte, l.opt.fileBytes)
	rand.Read(payload)
	copy(payload, l.generation)

	stop := make(chan struct{})
	var helpers sync.WaitGroup

	// A sampler for the lock every gateway write has to take.
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			start := time.Now()
			unlock := fileops.Lock(l.node.syncDir)
			waited := time.Since(start)
			unlock()
			l.mu.Lock()
			l.lockWaits = append(l.lockWaits, waited)
			l.mu.Unlock()
		}
	}()

	// Readiness as a load balancer would poll it.
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			if !l.node.ready() {
				l.notReady.Add(1)
			}
		}
	}()

	// One writer keeps overwriting a large file. Publishing it has to observe
	// the generation it replaces, so this is where the commit path's reads show
	// up — and where holding the shared lock across them is felt.
	if l.opt.large > 0 && l.opt.largeBytes > 0 {
		helpers.Add(1)
		go func() {
			defer helpers.Done()
			blob := make([]byte, l.opt.largeBytes)
			rand.Read(blob)
			copy(blob, l.generation)
			for i := 0; time.Now().Before(deadline); i++ {
				name := fmt.Sprintf("large/blob-%02d.bin", i%l.opt.large)
				elapsed, err := benchWrite(l.node.syncDir, name, blob)
				if err != nil {
					continue
				}
				l.mu.Lock()
				l.largeOnes = append(l.largeOnes, elapsed)
				l.mu.Unlock()
			}
		}()
	}

	var writers sync.WaitGroup
	for worker := 0; worker < max(1, l.opt.writers); worker++ {
		writers.Add(1)
		go func(worker int) {
			defer writers.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				// Two thirds overwrite a seeded name, one third adds a new one.
				var name string
				if i%3 == 2 {
					name = fmt.Sprintf("load/%s/w%d-%06d.bin", l.generation, worker, i)
				} else {
					name = benchSeedName((worker*7919 + i) % max(1, l.opt.files))
				}
				elapsed, err := benchWrite(l.node.syncDir, name, payload)
				if err != nil {
					// A busy name is ordinary contention, not a failure.
					continue
				}
				l.writes.Add(1)
				l.mu.Lock()
				l.latencies = append(l.latencies, elapsed)
				l.mu.Unlock()
			}
		}(worker)
	}
	writers.Wait()
	close(stop)
	helpers.Wait()
}

func (l *benchLoad) writeLatency() benchStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return benchSummarize(l.latencies)
}

func (l *benchLoad) largeWrite() benchStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return benchSummarize(l.largeOnes)
}

func (l *benchLoad) lockWait() benchStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return benchSummarize(l.lockWaits)
}

// benchWrite publishes one file the way a gateway does: staged, then committed
// under the shared lock and indexed before the write is acknowledged.
func benchWrite(root, name string, payload []byte) (time.Duration, error) {
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	start := time.Now()
	f, commit, err := fileops.OpenWriter(root, path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(payload); err != nil {
		fileops.AbortWriter(root, f)
		return 0, err
	}
	if err := commit(); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// --- seeding and probes ----------------------------------------------------

func benchSeedName(i int) string {
	return fmt.Sprintf("seed/%03d/file-%06d.bin", i%256, i)
}

func benchSeed(t *testing.T, root string, opt benchOptions) int64 {
	t.Helper()
	payload := make([]byte, opt.fileBytes)
	rand.Read(payload)
	var total int64
	for i := 0; i < opt.files; i++ {
		path := filepath.Join(root, filepath.FromSlash(benchSeedName(i)))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		total += int64(len(payload))
	}
	// A few large files: they are what used to hold the commit lock for the
	// whole of their own read, and what makes a transfer slow enough to matter.
	large := make([]byte, 1<<20)
	rand.Read(large)
	for i := 0; i < opt.large; i++ {
		path := filepath.Join(root, fmt.Sprintf("large/blob-%02d.bin", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		for written := 0; written < opt.largeBytes; written += len(large) {
			if _, err := f.Write(large); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		total += int64(opt.largeBytes)
	}
	return total
}

func benchFileCount(t *testing.T, n *benchNode) int64 {
	t.Helper()
	count, err := n.store.FileCount()
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// benchPublishFloor times the cheapest durable publish this filesystem allows:
// write a scratch file, fsync it, rename it into place, fsync the directory.
// Birak does that and more, so this is the floor its write latency sits on.
func benchPublishFloor(t *testing.T, dir string, size int) time.Duration {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	payload := make([]byte, size)
	const samples = 50

	start := time.Now()
	for i := 0; i < samples; i++ {
		scratch := filepath.Join(dir, fmt.Sprintf("scratch-%03d", i))
		final := filepath.Join(dir, fmt.Sprintf("published-%03d", i))
		f, err := os.Create(scratch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := errorsJoin(f.Sync(), f.Close()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(scratch, final); err != nil {
			t.Fatal(err)
		}
		if err := fileops.SyncDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	return time.Since(start) / samples
}

func errorsJoin(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// benchDigest fingerprints everything a node claims to hold: every name with
// its content hash and its place in the conflict order, tombstones included.
// Two nodes agree exactly when these match.
func benchDigest(t *testing.T, n *benchNode) string {
	t.Helper()
	sum := sha256.New()
	after := ""
	for {
		page, err := n.store.ListManifest(after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			return hex.EncodeToString(sum.Sum(nil))
		}
		for _, meta := range page {
			fmt.Fprintf(sum, "%s|%s|%t|%d|%s|%d\n", meta.Name, meta.Hash, meta.Deleted, meta.StateClock(), meta.BigClock, meta.Size)
			after = meta.Name
		}
	}
}

func benchScrubProgress(t *testing.T, n *benchNode) string {
	t.Helper()
	status := n.sync.ScanStatus()
	if status.LastScrubAgoMS < 0 {
		return "no full cycle yet"
	}
	return fmt.Sprintf("last full cycle %s ago", time.Duration(status.LastScrubAgoMS)*time.Millisecond)
}

func benchAwait(t *testing.T, limit time.Duration, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func benchFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// --- statistics and report -------------------------------------------------

type benchStats struct {
	count            int
	p50, p95, p99    time.Duration
	worst            time.Duration
	totalMeasurement time.Duration
}

func benchSummarize(samples []time.Duration) benchStats {
	if len(samples) == 0 {
		return benchStats{}
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(q float64) time.Duration {
		index := int(q * float64(len(sorted)-1))
		return sorted[index]
	}
	var total time.Duration
	for _, s := range sorted {
		total += s
	}
	return benchStats{
		count: len(sorted), p50: pick(0.50), p95: pick(0.95), p99: pick(0.99),
		worst: sorted[len(sorted)-1], totalMeasurement: total,
	}
}

func (s benchStats) String() string {
	if s.count == 0 {
		return "no samples"
	}
	return fmt.Sprintf("n=%d p50=%s p95=%s p99=%s worst=%s",
		s.count, s.p50.Round(time.Microsecond), s.p95.Round(time.Microsecond),
		s.p99.Round(time.Microsecond), s.worst.Round(time.Microsecond))
}

type benchReport struct {
	options            benchOptions
	floor              time.Duration
	quietWrite         benchStats
	lockLoad           fileops.LockLoad
	largeWrite         benchStats
	seedBytes          int64
	seedWrite          time.Duration
	indexTime          time.Duration
	indexedFiles       int64
	writes             int64
	write              benchStats
	lock               benchStats
	readyFailures      int64
	scrubbed           string
	writesDuringOutage int64
	convergence        time.Duration
	convergedFiles     int64
}

func benchPerAcquisition(load fileops.LockLoad) time.Duration {
	if load.Acquisitions == 0 {
		return 0
	}
	return (load.Held / time.Duration(load.Acquisitions)).Round(time.Microsecond)
}

func (r *benchReport) String() string {
	mib := func(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
	rate := func(n int64, d time.Duration) string {
		if d <= 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.0f/s", float64(n)/d.Seconds())
	}
	return fmt.Sprintf(`birak load stand
  data root            %s
  seeded               %d files of %s plus %d large files of %s (%s total)
  scrub budget         %s/s

  publish floor        %s per write on this filesystem
  first index          %s for %d files (%s)
  write, node idle     %s
  write, under load    %s
  large-file overwrite %s
  commit-lock wait     %s
  commit-lock held     %s over %d acquisitions (avg %s, worst %s)
  writes accepted      %d in %s (%s)
  readiness failures   %d
  scrub                %s

  outage writes        %d
  convergence          %s for %d files after the peer returned

  Read the three write lines together. Floor to idle is what Birak adds per
  write; idle to loaded is queueing on the one commit lock a volume has. Read
  all of it against the storage this ran on: a page-cached laptop says nothing
  about a network volume, and no number here bounds replication lag on a slow
  link, where a transfer that keeps making progress has no deadline.`,
		r.options.dir,
		r.options.files, mib(int64(r.options.fileBytes)), r.options.large, mib(int64(r.options.largeBytes)), mib(r.seedBytes),
		mib(r.options.scrubRate),
		r.floor.Round(time.Microsecond),
		r.indexTime.Round(time.Millisecond), r.indexedFiles, rate(r.indexedFiles, r.indexTime),
		r.quietWrite, r.write, r.largeWrite, r.lock,
		r.lockLoad.Held.Round(time.Millisecond), r.lockLoad.Acquisitions,
		benchPerAcquisition(r.lockLoad), r.lockLoad.Worst.Round(time.Microsecond),
		r.writes, r.options.duration, rate(r.writes, r.options.duration),
		r.readyFailures,
		r.scrubbed,
		r.writesDuringOutage,
		r.convergence.Round(time.Millisecond), r.convergedFiles,
	)
}
