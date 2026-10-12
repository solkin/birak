package syncer

// A load measurement for the per-volume commit lock, not a correctness test.
// It answers two questions for a read-heavy node: does a GET wait while a large
// replica is applied or the scratch janitor walks the tree, and how long does a
// replica apply hold the lock compared with publishing the same bytes locally.
//
// It is opt-in, writes several large files, and creates a large tree:
//
//	BIRAK_LOCK_BENCH=1 go test -run TestCommitLockContention -v -timeout 30m ./internal/syncer
//
// Knobs, all optional:
//
//	BIRAK_LOCK_BENCH_MB      size of the published and replicated file (default 500)
//	BIRAK_LOCK_BENCH_FILES   files in the swept tree                    (default 200000)
//	BIRAK_LOCK_BENCH_READERS concurrent GET loops                       (default 8)
//
// Set TMPDIR to measure on the storage you intend to ship on.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/store"
)

func lockBenchInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("%s: %q is not a positive count", key, raw)
	}
	return n
}

// patternReader streams size bytes by repeating one random block, so a large
// replica needs neither a large buffer nor a disk to come from.
type patternReader struct {
	block     []byte
	remaining int64
	offset    int
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}
	n := copy(b[:min(int64(len(b)), p.remaining)], p.block[p.offset:])
	p.offset = (p.offset + n) % len(p.block)
	p.remaining -= int64(n)
	return n, nil
}

func patternHash(block []byte, size int64) string {
	h := sha256.New()
	io.Copy(h, &patternReader{block: block, remaining: size})
	return hex.EncodeToString(h.Sum(nil))
}

func writePattern(t *testing.T, path string, block []byte, size int64, stamp time.Time) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, &patternReader{block: block, remaining: size}); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// readLoop is a GET load: open, read and close one small object as fast as the
// node allows, recording how long each open took.
type readLoop struct {
	stop   chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	opens  []time.Duration
	cycles atomic.Int64
	errors atomic.Int64
	began  time.Time
}

func startReadLoop(dir, path string, workers int) *readLoop {
	l := &readLoop{stop: make(chan struct{}), began: time.Now()}
	for i := 0; i < workers; i++ {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			buf := make([]byte, 4096)
			var local []time.Duration
			defer func() {
				l.mu.Lock()
				l.opens = append(l.opens, local...)
				l.mu.Unlock()
			}()
			for {
				select {
				case <-l.stop:
					return
				default:
				}
				began := time.Now()
				r, err := fileops.OpenReader(dir, path)
				local = append(local, time.Since(began))
				if err != nil {
					l.errors.Add(1)
					continue
				}
				r.Read(buf)
				r.Close()
				l.cycles.Add(1)
			}
		}()
	}
	return l
}

type readResult struct {
	opens   []time.Duration
	cycles  int64
	errors  int64
	elapsed time.Duration
}

func (l *readLoop) halt() readResult {
	close(l.stop)
	l.wg.Wait()
	slices.Sort(l.opens)
	return readResult{opens: l.opens, cycles: l.cycles.Load(), errors: l.errors.Load(), elapsed: time.Since(l.began)}
}

func (r readResult) String() string {
	if len(r.opens) == 0 {
		return "no samples"
	}
	at := func(q float64) time.Duration {
		return r.opens[min(len(r.opens)-1, int(q*float64(len(r.opens))))].Round(time.Microsecond)
	}
	return fmt.Sprintf("open p50 %v p99 %v p99.9 %v max %v; %.0f open/read/close per second; %d errors",
		at(0.50), at(0.99), at(0.999), r.opens[len(r.opens)-1].Round(time.Microsecond),
		float64(r.cycles)/r.elapsed.Seconds(), r.errors)
}

// lockDelta describes the commit lock across one operation. The worst hold is
// a high-water mark set by binding the volume, so if the operation did not
// move it, it is bounded instead by everything held during the operation.
func lockDelta(before, after fileops.LockLoad) string {
	held := after.Held - before.Held
	worst := fmt.Sprintf("worst hold %v", after.Worst.Round(time.Microsecond))
	if after.Worst == before.Worst {
		worst = fmt.Sprintf("worst hold <= %v", min(before.Worst, held).Round(time.Microsecond))
	}
	return fmt.Sprintf("%s; held %v over %d acquisitions", worst, held.Round(time.Microsecond), after.Acquisitions-before.Acquisitions)
}

func TestCommitLockContention(t *testing.T) {
	if os.Getenv("BIRAK_LOCK_BENCH") == "" {
		t.Skip("opt-in load measurement: set BIRAK_LOCK_BENCH=1")
	}
	size := int64(lockBenchInt(t, "BIRAK_LOCK_BENCH_MB", 500)) << 20
	files := lockBenchInt(t, "BIRAK_LOCK_BENCH_FILES", 200000)
	readers := lockBenchInt(t, "BIRAK_LOCK_BENCH_READERS", 8)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	block := make([]byte, 1<<20)
	rand.Read(block)
	oldBlock := make([]byte, 1<<20)
	rand.Read(oldBlock)
	hash := patternHash(block, size)
	var report []string
	line := func(format string, args ...any) {
		report = append(report, fmt.Sprintf(format, args...))
	}

	// prepare returns a syncer on a fresh, bound volume holding one small object
	// for the GET load.
	prepare := func() (*Syncer, string) {
		s, _ := auditSyncer(t)
		icon := filepath.Join(s.syncDir, "icon.png")
		if err := os.WriteFile(icon, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.watcher.CheckStorage(); err != nil {
			t.Fatal(err)
		}
		return s, icon
	}
	during := func(s *Syncer, icon string, op func()) (readResult, fileops.LockLoad, fileops.LockLoad, time.Duration) {
		before := fileops.LockStats(s.syncDir)
		load := startReadLoop(s.syncDir, icon, readers)
		time.Sleep(50 * time.Millisecond)
		began := time.Now()
		op()
		elapsed := time.Since(began)
		time.Sleep(50 * time.Millisecond)
		return load.halt(), before, fileops.LockStats(s.syncDir), elapsed
	}
	mb := size >> 20

	{
		s, icon := prepare()
		load := startReadLoop(s.syncDir, icon, readers)
		time.Sleep(2 * time.Second)
		line("idle node                      %s", load.halt())
	}

	{
		// The control: the same bytes written and flushed with no Birak lock
		// involved. GET latency here is what the filesystem itself costs.
		s, icon := prepare()
		reads, before, after, elapsed := during(s, icon, func() {
			writePattern(t, filepath.Join(s.syncDir, ".birak-tmp-control"), block, size, time.Now())
		})
		line("write+fsync only, %4d MiB     took %v; %s", mb, elapsed.Round(time.Millisecond), lockDelta(before, after))
		line("  GETs meanwhile               %s", reads)
	}

	// publish runs a gateway's commit of size bytes, optionally over an indexed
	// file of the same size.
	publish := func(over bool) {
		s, icon := prepare()
		dest := filepath.Join(s.syncDir, "app.apk")
		if over {
			writePattern(t, dest, oldBlock, size, time.Unix(0, 100))
			if _, err := s.store.PutFile("app.apk", 100, size, patternHash(oldBlock, size), false); err != nil {
				t.Fatal(err)
			}
		}
		scratch, err := fileops.CreateTemp(s.syncDir, ".birak-tmp-bench-*")
		if err != nil {
			t.Fatal(err)
		}
		scratch.Close()
		fileops.ReleaseTemp(scratch)
		writePattern(t, scratch.Name(), block, size, time.Unix(0, 200))
		reads, before, after, elapsed := during(s, icon, func() {
			if err := fileops.Publish(s.syncDir, scratch.Name(), dest); err != nil {
				t.Fatal(err)
			}
		})
		kind := "new, "
		if over {
			kind = "over,"
		}
		line("source publish, %s %4d MiB  took %v; %s", kind, mb, elapsed.Round(time.Millisecond), lockDelta(before, after))
		line("  GETs meanwhile               %s", reads)
	}
	publish(false)
	publish(true)

	serve := func(s *Syncer) {
		s.downloadClient.Transport = auditTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(&patternReader{block: block, remaining: size})}, nil
		})
	}
	meta := store.FileMeta{Name: "app.apk", Hash: hash, Size: size, ModTime: 200, Version: 1}

	{
		s, icon := prepare()
		serve(s)
		reads, before, after, elapsed := during(s, icon, func() {
			if err := s.applyChange(context.Background(), "http://peer.invalid", meta); err != nil {
				t.Fatal(err)
			}
		})
		line("replica apply, new,  %4d MiB  took %v; %s", mb, elapsed.Round(time.Millisecond), lockDelta(before, after))
		line("  GETs meanwhile               %s", reads)
	}

	{
		s, icon := prepare()
		serve(s)
		path := filepath.Join(s.syncDir, "app.apk")
		writePattern(t, path, oldBlock, size, time.Unix(0, 100))
		if _, err := s.store.PutFile("app.apk", 100, size, patternHash(oldBlock, size), false); err != nil {
			t.Fatal(err)
		}
		reads, before, after, elapsed := during(s, icon, func() {
			if err := s.applyChange(context.Background(), "http://peer.invalid", meta); err != nil {
				t.Fatal(err)
			}
		})
		line("replica apply, over, %4d MiB  took %v; %s", mb, elapsed.Round(time.Millisecond), lockDelta(before, after))
		line("  GETs meanwhile               %s", reads)
	}

	{
		s, icon := prepare()
		const perDir = 200
		for i := 0; i < files; i++ {
			dir := filepath.Join(s.syncDir, fmt.Sprintf("tree/%04d", i/perDir))
			if i%perDir == 0 {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			name := fmt.Sprintf("f%03d", i%perDir)
			if i%(files/10+1) == 0 {
				name = ".birak-tmp-orphan-" + name
			}
			f, err := os.Create(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			if strings.HasPrefix(name, ".birak-tmp-") {
				old := time.Now().Add(-2 * time.Hour)
				os.Chtimes(f.Name(), old, old)
			}
		}
		reads, before, after, elapsed := during(s, icon, func() {
			gateway.SweepTempFiles(s.syncDir, time.Hour, logger)
		})
		line("scratch sweep, %d files    took %v; %s", files, elapsed.Round(time.Millisecond), lockDelta(before, after))
		line("  GETs meanwhile               %s", reads)
	}

	t.Logf("commit lock contention (%d GET loops)\n  %s", readers, strings.Join(report, "\n  "))
}
