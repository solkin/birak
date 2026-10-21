package s3

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/multipart"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// --- a copy in flight ---

// copyGate makes every copy source opened during the test wait for release
// before its first read. The first read of all runs onFirstRead and then
// reports on started, so a test can act while a copy is under way. It also
// counts the source bytes read.
func copyGate(t *testing.T, onFirstRead func()) (started, release chan struct{}, read *atomic.Int64) {
	t.Helper()
	started, release, read = make(chan struct{}), make(chan struct{}), &atomic.Int64{}
	gate := &copyGateState{started: started, release: release, onFirstRead: onFirstRead, read: read}
	original := openCopySource
	openCopySource = func(root, path string) (copySource, error) {
		f, err := original(root, path)
		if err != nil {
			return nil, err
		}
		return &gatedSource{copySource: f, gate: gate}, nil
	}
	t.Cleanup(func() { openCopySource = original })
	return started, release, read
}

type copyGateState struct {
	first       sync.Once
	started     chan struct{}
	release     chan struct{}
	onFirstRead func()
	read        *atomic.Int64
}

// gatedSource is a copy source behind a copyGate.
type gatedSource struct {
	copySource
	gate *copyGateState
	once sync.Once
}

func (s *gatedSource) ReadAt(p []byte, off int64) (int, error) {
	s.once.Do(func() {
		s.gate.first.Do(func() {
			if s.gate.onFirstRead != nil {
				s.gate.onFirstRead()
			}
			close(s.gate.started)
		})
		<-s.gate.release
	})
	n, err := s.copySource.ReadAt(p, off)
	s.gate.read.Add(int64(n))
	return n, err
}

func waitFor(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestCopyObject_DoesNotHoldCommitLock copies a large object while another
// client writes to the same volume. The copy is staged outside the commit
// lock, as a PUT body is, so the write is not held up for the length of the
// copy; it used to wait for a whole hash-copy-hash pass under the lock.
func TestCopyObject_DoesNotHoldCommitLock(t *testing.T) {
	g, root := copyGateway(t, Config{})
	large := bytes.Repeat([]byte("apk-bytes-"), 400_000) // 4 MB
	if err := os.WriteFile(filepath.Join(root, "bucket", "large.apk"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	started, release, _ := copyGate(t, nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- copyRequest(g, "/bucket/copy.apk", "bucket/large.apk", nil) }()
	waitFor(t, started, "the copy to start reading")

	unlock, free := fileops.TryLock(root)
	if free {
		unlock()
	}
	put := make(chan int, 1)
	go func() {
		put <- serveRequest(g, http.MethodPut, "/bucket/small.png", strings.NewReader("icon"), nil).Code
	}()
	var putCode int
	select {
	case putCode = <-put:
	case <-time.After(5 * time.Second):
	}
	close(release)
	w := <-done
	if !free || putCode != http.StatusOK {
		t.Fatalf("a copy in progress held the commit lock (lock free: %v, concurrent PUT: %d)", free, putCode)
	}
	if w.Code != http.StatusOK || readObject(t, root, "bucket/copy.apk") != string(large) {
		t.Fatalf("copy: %d %s", w.Code, w.Body.String())
	}
}

// TestCopy_SourceChangedMidCopyIsRefused replaces the source while a copy
// reads it: the bytes are of no single generation and must not be published,
// for CopyObject or for a part.
func TestCopy_SourceChangedMidCopyIsRefused(t *testing.T) {
	g, root, _ := multipartGateway(t, Config{}, multipart.Limits{})
	src := filepath.Join(root, "bucket", "src")
	if err := os.WriteFile(src, bytes.Repeat([]byte("a"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bucket", "dst"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := initiateUpload(t, g, "bucket", "big")
	rewrite := func() {
		later := time.Now().Add(time.Minute)
		if err := os.WriteFile(src, bytes.Repeat([]byte("b"), 1<<20), 0o600); err != nil {
			t.Error(err)
		}
		os.Chtimes(src, later, later)
	}

	for name, request := range map[string]func() *httptest.ResponseRecorder{
		"CopyObject": func() *httptest.ResponseRecorder { return copyRequest(g, "/bucket/dst", "bucket/src", nil) },
		"UploadPartCopy": func() *httptest.ResponseRecorder {
			return copyRequest(g, "/bucket/big?partNumber=1&uploadId="+id, "bucket/src", nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, release, _ := copyGate(t, rewrite)
			close(release)
			w := request()
			if w.Code != http.StatusServiceUnavailable || errorCode(t, w) != "SlowDown" {
				t.Fatalf("source changed mid-copy: %d %s", w.Code, w.Body.String())
			}
		})
	}
	if readObject(t, root, "bucket/dst") != "keep me" {
		t.Fatal("destination replaced by a copy of a changing source")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "bucket")); len(entries) != 2 {
		t.Fatalf("scratch left behind: %v", entries)
	}
	var parts ListPartsResult
	xml.Unmarshal(serveRequest(g, http.MethodGet, "/bucket/big?uploadId="+id, nil, nil).Body.Bytes(), &parts)
	if len(parts.Parts) != 0 {
		t.Fatalf("part of a changing source staged: %+v", parts.Parts)
	}
}

// --- UploadPartCopy ---

// partCopy sends an UploadPartCopy and returns the response.
func partCopy(g *Gateway, key, id string, part int, source, rng string, headers map[string]string) *httptest.ResponseRecorder {
	all := map[string]string{"X-Amz-Copy-Source": source}
	if rng != "" {
		all["X-Amz-Copy-Source-Range"] = rng
	}
	for k, v := range headers {
		all[k] = v
	}
	return serveRequest(g, http.MethodPut, fmt.Sprintf("/bucket/%s?partNumber=%d&uploadId=%s", key, part, id), nil, all)
}

// TestUploadPartCopy_AssemblesObject copies an object part by part, the way the
// AWS CLI copies anything above its 8 MiB multipart threshold, mixed with an
// uploaded part, and completes it: each part is known by the same ETag in its
// CopyPartResult, ListParts and the completion.
func TestUploadPartCopy_AssemblesObject(t *testing.T) {
	g, root, _ := multipartGateway(t, Config{}, multipart.Limits{})
	source := []byte("0123456789abcdefghij")
	if err := os.WriteFile(filepath.Join(root, "bucket", "src.apk"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	id := initiateUpload(t, g, "bucket", "copy.apk")

	var entries []CompletePartEntry
	for i, rng := range []string{"bytes=0-7", "bytes=8-15", "bytes=16-19"} {
		w := partCopy(g, "copy.apk", id, i+1, "/bucket/src.apk", rng, nil)
		var result CopyPartResult
		if w.Code != http.StatusOK || xml.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("part %d: %d %s", i+1, w.Code, w.Body.String())
		}
		first, last := 8*i, min(8*i+8, len(source))
		if want := `"` + md5HexOf(source[first:last]) + `"`; result.ETag != want {
			t.Fatalf("part %d ETag %s, want %s", i+1, result.ETag, want)
		}
		if _, err := time.Parse(s3TimeFormat, result.LastModified); err != nil {
			t.Fatalf("part %d LastModified %q", i+1, result.LastModified)
		}
		entries = append(entries, CompletePartEntry{PartNumber: i + 1, ETag: result.ETag})
	}
	entries = append(entries, CompletePartEntry{PartNumber: 4, ETag: uploadPart(t, g, "bucket", "copy.apk", id, 4, []byte("XYZ"))})

	var parts ListPartsResult
	xml.Unmarshal(serveRequest(g, http.MethodGet, "/bucket/copy.apk?uploadId="+id, nil, nil).Body.Bytes(), &parts)
	if len(parts.Parts) != 4 {
		t.Fatalf("parts: %+v", parts.Parts)
	}
	for i, p := range parts.Parts {
		if p.ETag != entries[i].ETag {
			t.Fatalf("ListParts ETag of part %d is %s, copy reported %s", p.PartNumber, p.ETag, entries[i].ETag)
		}
	}
	if w := completeUpload(g, "bucket", "copy.apk", id, entries); w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	if got := readObject(t, root, "bucket/copy.apk"); got != string(source)+"XYZ" {
		t.Fatalf("assembled %q", got)
	}

	// Without a range, the part is the whole source.
	id = initiateUpload(t, g, "bucket", "whole.apk")
	w := partCopy(g, "whole.apk", id, 1, "bucket/src.apk", "", nil)
	var result CopyPartResult
	if w.Code != http.StatusOK || xml.Unmarshal(w.Body.Bytes(), &result) != nil || result.ETag != `"`+md5HexOf(source)+`"` {
		t.Fatalf("whole-source part: %d %s", w.Code, w.Body.String())
	}
}

// TestUploadPartCopy_Refusals refuses every malformed or unsatisfiable part
// copy, leaving the staged part as it was.
func TestUploadPartCopy_Refusals(t *testing.T) {
	g, root, _ := multipartGateway(t, Config{MaxUploadBytes: 12}, multipart.Limits{MaxPartBytes: 16})
	if err := os.WriteFile(filepath.Join(root, "bucket", "src.apk"), []byte("0123456789abcdefghij"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "bucket", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := os.Stat(filepath.Join(root, "bucket", "src.apk"))
	id := initiateUpload(t, g, "bucket", "big")
	staged := uploadPart(t, g, "bucket", "big", id, 1, []byte("staged part"))
	other := strings.Repeat("0", 32)

	for _, tc := range []struct {
		name, id, part, source, rng string
		headers                     map[string]string
		body                        string
		status                      int
		code                        string
	}{
		{"missing source", id, "1", "bucket/missing", "", nil, "", http.StatusNotFound, "NoSuchKey"},
		{"missing source bucket", id, "1", "nobucket/src.apk", "", nil, "", http.StatusNotFound, "NoSuchBucket"},
		{"version", id, "1", "bucket/src.apk?versionId=1", "", nil, "", http.StatusNotImplemented, "NotImplemented"},
		{"folder", id, "1", "bucket/dir/", "", nil, "", http.StatusBadRequest, "InvalidRequest"},
		{"range without end", id, "1", "bucket/src.apk", "bytes=5", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"open range", id, "1", "bucket/src.apk", "bytes=5-", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"suffix range", id, "1", "bucket/src.apk", "bytes=-5", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"reversed range", id, "1", "bucket/src.apk", "bytes=5-2", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"other unit", id, "1", "bucket/src.apk", "items=0-1", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"range past the end", id, "1", "bucket/src.apk", "bytes=10-20", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"part over the part limit", id, "1", "bucket/src.apk", "bytes=0-16", nil, "", http.StatusRequestEntityTooLarge, "EntityTooLarge"},
		{"part over max_upload_bytes", id, "1", "bucket/src.apk", "bytes=0-12", nil, "", http.StatusRequestEntityTooLarge, "EntityTooLarge"},
		{"whole source too large", id, "1", "bucket/src.apk", "", nil, "", http.StatusRequestEntityTooLarge, "EntityTooLarge"},
		{"if-match", id, "1", "bucket/src.apk", "bytes=0-3", map[string]string{"X-Amz-Copy-Source-If-Match": `"stale"`}, "", http.StatusPreconditionFailed, "PreconditionFailed"},
		{"if-none-match", id, "1", "bucket/src.apk", "bytes=0-3", map[string]string{"X-Amz-Copy-Source-If-None-Match": etagFor(src)}, "", http.StatusPreconditionFailed, "PreconditionFailed"},
		{"body", id, "1", "bucket/src.apk", "bytes=0-3", nil, "data", http.StatusBadRequest, "InvalidRequest"},
		{"no such upload", other, "1", "bucket/src.apk", "bytes=0-3", nil, "", http.StatusNotFound, "NoSuchUpload"},
		{"part number", id, "x", "bucket/src.apk", "bytes=0-3", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"part number zero", id, "0", "bucket/src.apk", "bytes=0-3", nil, "", http.StatusBadRequest, "InvalidArgument"},
	} {
		headers := map[string]string{"X-Amz-Copy-Source": tc.source}
		if tc.rng != "" {
			headers["X-Amz-Copy-Source-Range"] = tc.rng
		}
		for k, v := range tc.headers {
			headers[k] = v
		}
		w := serveRequest(g, http.MethodPut, "/bucket/big?partNumber="+tc.part+"&uploadId="+tc.id, strings.NewReader(tc.body), headers)
		if w.Code != tc.status || errorCode(t, w) != tc.code {
			t.Fatalf("%s: %d %s, want %d %s", tc.name, w.Code, w.Body.String(), tc.status, tc.code)
		}
	}
	var parts ListPartsResult
	xml.Unmarshal(serveRequest(g, http.MethodGet, "/bucket/big?uploadId="+id, nil, nil).Body.Bytes(), &parts)
	if len(parts.Parts) != 1 || parts.Parts[0].ETag != staged {
		t.Fatalf("staged part changed: %+v", parts.Parts)
	}
}

// --- damaged sources ---

// indexedGateway is a gateway over a volume indexed by a watcher, with the
// store wired in as the daemon wires it, a multipart store, and a bucket.
func indexedGateway(t *testing.T) (*Gateway, string, *watcher.Watcher) {
	t.Helper()
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	staging, err := multipart.New(root, multipart.Limits{MinPartBytes: 4, MaxPartBytes: 1 << 20}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bucket"), 0o700); err != nil {
		t.Fatal(err)
	}
	return New(root, nil, Config{Catalog: st, Multipart: staging}, logger), root, w
}

// writeIndexed writes a file and indexes it, as a write through Birak would.
func writeIndexed(t *testing.T, root string, w *watcher.Watcher, name, content string) os.FileInfo {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh(name); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// rot replaces a file's bytes under its old size and timestamp, as bit rot
// does: nothing but the checksum can tell.
func rot(t *testing.T, path string, info os.FileInfo, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// TestCopy_DamagedSourceIsNotCopied keeps a source whose bytes no longer match
// their recorded checksum from being re-published as a trusted new write under
// another name, which would replicate the damage as if it were data: not by
// CopyObject, and not part by part. Once the damage is known, the read fence
// refuses the source outright.
func TestCopy_DamagedSourceIsNotCopied(t *testing.T) {
	g, root, w := indexedGateway(t)
	src := filepath.Join(root, "bucket", "src")
	info := writeIndexed(t, root, w, "bucket/src", "original")
	writeIndexed(t, root, w, "bucket/dst", "keep me")
	id := initiateUpload(t, g, "bucket", "big")
	rot(t, src, info, "CORRUPT!")

	check := func(stage string) {
		t.Helper()
		for name, out := range map[string]*httptest.ResponseRecorder{
			"CopyObject over an object": copyRequest(g, "/bucket/dst", "bucket/src", nil),
			"CopyObject to a new key":   copyRequest(g, "/bucket/fresh", "bucket/src", nil),
			"UploadPartCopy":            partCopy(g, "big", id, 1, "bucket/src", "bytes=0-3", nil),
		} {
			if out.Code < 400 {
				t.Fatalf("%s: %s copied: %d %s", stage, name, out.Code, out.Body.String())
			}
		}
		if readObject(t, root, "bucket/dst") != "keep me" {
			t.Fatalf("%s: destination lost", stage)
		}
		if _, err := os.Stat(filepath.Join(root, "bucket", "fresh")); !os.IsNotExist(err) {
			t.Fatalf("%s: a copy of damaged bytes was published: %v", stage, err)
		}
		var parts ListPartsResult
		xml.Unmarshal(serveRequest(g, http.MethodGet, "/bucket/big?uploadId="+id, nil, nil).Body.Bytes(), &parts)
		if len(parts.Parts) != 0 {
			t.Fatalf("%s: part of damaged bytes staged", stage)
		}
	}
	check("undetected damage")

	if err := w.Refresh("bucket/src"); !errors.Is(err, watcher.ErrIntegrity) {
		t.Fatalf("scrub did not find the damage: %v", err)
	}
	check("quarantined source")
}

// TestCopy_QuarantinedSourceIsRefusedByTheReadFence copies from a source the
// watcher has quarantined, through a gateway without an index to check the
// bytes against: the read fence alone must refuse it.
func TestCopy_QuarantinedSourceIsRefusedByTheReadFence(t *testing.T) {
	indexed, root, w := indexedGateway(t)
	info := writeIndexed(t, root, w, "bucket/src", "original")
	rot(t, filepath.Join(root, "bucket", "src"), info, "CORRUPT!")
	if err := w.Refresh("bucket/src"); !errors.Is(err, watcher.ErrIntegrity) {
		t.Fatalf("scrub did not find the damage: %v", err)
	}
	g := New(root, nil, Config{Multipart: indexed.multipart}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id := initiateUpload(t, g, "bucket", "big")
	for name, out := range map[string]*httptest.ResponseRecorder{
		"CopyObject":     copyRequest(g, "/bucket/fresh", "bucket/src", nil),
		"UploadPartCopy": partCopy(g, "big", id, 1, "bucket/src", "bytes=0-3", nil),
	} {
		if out.Code < 400 {
			t.Fatalf("%s of a quarantined source: %d %s", name, out.Code, out.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "bucket", "fresh")); !os.IsNotExist(err) {
		t.Fatalf("a copy of a quarantined source was published: %v", err)
	}
}

// TestUploadPartCopy_VerifiesSourceOncePerGeneration reads an indexed source
// in full once for a whole multipart copy, not once per part, and again only
// when the source is a new generation.
func TestUploadPartCopy_VerifiesSourceOncePerGeneration(t *testing.T) {
	g, root, w := indexedGateway(t)
	content := strings.Repeat("0123456789", 10)
	writeIndexed(t, root, w, "bucket/src", content)
	_, release, read := copyGate(t, nil)
	close(release)

	id := initiateUpload(t, g, "bucket", "copy")
	var entries []CompletePartEntry
	for i := 0; i < 4; i++ {
		out := partCopy(g, "copy", id, i+1, "bucket/src", fmt.Sprintf("bytes=%d-%d", 25*i, 25*i+24), nil)
		var result CopyPartResult
		if out.Code != http.StatusOK || xml.Unmarshal(out.Body.Bytes(), &result) != nil {
			t.Fatalf("part %d: %d %s", i+1, out.Code, out.Body.String())
		}
		entries = append(entries, CompletePartEntry{PartNumber: i + 1, ETag: result.ETag})
	}
	if got := read.Load(); got != int64(2*len(content)) {
		t.Fatalf("read %d source bytes for a %d-byte copy, want one verification and one copy", got, len(content))
	}
	if out := completeUpload(g, "bucket", "copy", id, entries); out.Code != http.StatusOK || readObject(t, root, "bucket/copy") != content {
		t.Fatalf("complete: %d %s", out.Code, out.Body.String())
	}

	// A new generation is verified anew.
	writeIndexed(t, root, w, "bucket/src", strings.Repeat("9876543210", 10))
	read.Store(0)
	id = initiateUpload(t, g, "bucket", "again")
	if out := partCopy(g, "again", id, 1, "bucket/src", "bytes=0-9", nil); out.Code != http.StatusOK {
		t.Fatalf("part of new generation: %d %s", out.Code, out.Body.String())
	}
	if got := read.Load(); got != int64(len(content)+10) {
		t.Fatalf("read %d bytes for a part of a new generation", got)
	}
}

// A refused copy into a new prefix takes back the directories it created, so
// it does not leave a folder nobody asked for.
func TestCopy_RefusedCopyLeavesNoNewFolders(t *testing.T) {
	g, root, _ := multipartGateway(t, Config{}, multipart.Limits{})
	src := filepath.Join(root, "bucket", "src")
	if err := os.WriteFile(src, bytes.Repeat([]byte("a"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	_, release, _ := copyGate(t, func() {
		later := time.Now().Add(time.Minute)
		if err := os.WriteFile(src, bytes.Repeat([]byte("b"), 1<<20), 0o600); err != nil {
			t.Error(err)
		}
		os.Chtimes(src, later, later)
	})
	close(release)
	w := copyRequest(g, "/bucket/deep/new/dst", "bucket/src", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("source changed mid-copy: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "bucket", "deep")); !os.IsNotExist(err) {
		t.Fatalf("a refused copy left its destination folders: %v", err)
	}
}
