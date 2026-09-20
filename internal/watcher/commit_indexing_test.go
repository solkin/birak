package watcher

// A commit knows things an external writer does not: it fsynced the bytes
// before it published them, and it already read them. Indexing takes those
// promises only from a commit, and only while they still describe the file on
// disk — these tests pin both halves.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

func TestCommitReusesTheBytesItAlreadyRead(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "published.bin")
	if err := os.WriteFile(path, []byte("published bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// A sentinel proves reuse: reading the file again would record its real hash.
	unlock := fileops.Lock(w.dir)
	err = w.finishCommitLocked([]string{path}, map[string]fileops.Published{
		path: {Info: info, Hash: "sentinel-hash"},
	})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	meta, err := w.store.GetFile("published.bin")
	if err != nil || meta == nil {
		t.Fatalf("name was not indexed: %+v %v", meta, err)
	}
	if meta.Hash != "sentinel-hash" {
		t.Fatalf("hash = %q; the commit's own read was thrown away", meta.Hash)
	}
}

// The promise is about one generation. If the file changed after the commit
// read it, the hint is worthless and the file must be read again.
func TestCommitHintFromAnotherGenerationIsIgnored(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "replaced.bin")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("second generation"), 0o644); err != nil {
		t.Fatal(err)
	}

	unlock := fileops.Lock(w.dir)
	err = w.finishCommitLocked([]string{path}, map[string]fileops.Published{
		path: {Info: stale, Hash: "sentinel-hash"},
	})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	meta, err := w.store.GetFile("replaced.bin")
	if err != nil || meta == nil {
		t.Fatalf("name was not indexed: %+v %v", meta, err)
	}
	sum := sha256.Sum256([]byte("second generation"))
	if meta.Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %q; a hint from a replaced generation was trusted", meta.Hash)
	}
}

// The whole point of the gateway path: a real write publishes, and what gets
// indexed is what is on disk.
func TestGatewayWriteIndexesWhatItPublished(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "uploaded.bin")
	body := strings.Repeat("payload", 1000)

	f, commit, err := fileops.OpenWriter(w.dir, path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}

	meta, err := w.store.GetFile("uploaded.bin")
	if err != nil || meta == nil {
		t.Fatalf("published file was not indexed: %+v %v", meta, err)
	}
	sum := sha256.Sum256([]byte(body))
	if meta.Hash != hex.EncodeToString(sum[:]) || meta.Size != int64(len(body)) {
		t.Fatalf("indexed state disagrees with the published bytes: %+v", meta)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || string(onDisk) != body {
		t.Fatalf("published bytes are wrong: %v", err)
	}
}

// An overwrite whose size and timestamp still match the index has nothing new
// to observe, but a name the index has never seen must still be read.
func TestCommitStillObservesAnUnindexedGeneration(t *testing.T) {
	w := auditWatcher(t)
	path := filepath.Join(w.dir, "external.bin")
	if err := os.WriteFile(path, []byte("written outside birak"), 0o644); err != nil {
		t.Fatal(err)
	}

	unlock := fileops.Lock(w.dir)
	err := w.beginCommitLocked([]string{path})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	meta, err := w.store.GetFile("external.bin")
	if err != nil || meta == nil {
		t.Fatalf("a generation the index had never seen was not observed: %+v %v", meta, err)
	}
	sum := sha256.Sum256([]byte("written outside birak"))
	if meta.Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("observed the wrong bytes: %+v", meta)
	}
	if err := w.store.EndLocal([]string{"external.bin"}); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
}
