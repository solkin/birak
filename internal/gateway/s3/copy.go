package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/multipart"
	"github.com/birak/birak/internal/store"
)

// CopyObjectResult is the response for CopyObject.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// CopyPartResult is the response for UploadPartCopy.
type CopyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

var (
	errCopyBody = &objectFailure{http.StatusBadRequest, "InvalidRequest",
		"A copy request carries no body: the object's bytes come from x-amz-copy-source."}
	errCopySource = &objectFailure{http.StatusBadRequest, "InvalidArgument",
		"Copy Source must mention the source bucket and key: sourcebucket/sourcekey"}
	errCopyTooLarge = &objectFailure{http.StatusRequestEntityTooLarge, "EntityTooLarge",
		"The copy source exceeds the maximum allowed object size"}
	errFolderCopy = &objectFailure{http.StatusBadRequest, "InvalidRequest",
		"A folder (a key ending in /) can only be copied to another folder."}
	errCopyRange = &objectFailure{http.StatusBadRequest, "InvalidArgument",
		"The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy"}
	// errSourceChanged is a source replaced while it was being read: the bytes
	// read are no one generation of it. It is worth retrying, which SDKs do
	// for SlowDown on their own.
	errSourceChanged = &objectFailure{http.StatusServiceUnavailable, "SlowDown",
		"The copy source changed while it was being copied. Please retry."}
	// errSourceDamaged is a source whose bytes are not the ones its checksum was
	// recorded for: bit rot the scrub has not caught yet. Copying it would
	// publish the damage as a trusted new write, which replicates everywhere.
	errSourceDamaged = &objectFailure{http.StatusInternalServerError, "InternalError",
		"The copy source failed its integrity check and was not copied."}
)

// copyBufferSize is the read size of a server-side copy.
const copyBufferSize = 256 << 10

// parseCopySource splits x-amz-copy-source ("bucket/key" or "/bucket/key",
// URL-encoded, optionally followed by "?versionId=") into a bucket and key.
// Buckets are not versioned, so a version cannot be named.
func parseCopySource(raw string) (string, string, *objectFailure) {
	source, query, _ := strings.Cut(raw, "?")
	if query != "" {
		values, err := url.ParseQuery(query)
		if err != nil {
			return "", "", errCopySource
		}
		if _, ok := values["versionId"]; ok {
			return "", "", &objectFailure{http.StatusNotImplemented, "NotImplemented",
				"Buckets are not versioned: a copy source cannot name a version."}
		}
	}
	decoded, err := url.PathUnescape(source)
	if err != nil {
		return "", "", errCopySource
	}
	bucket, key, ok := strings.Cut(strings.TrimPrefix(decoded, "/"), "/")
	if !ok || bucket == "" || key == "" {
		return "", "", errCopySource
	}
	return bucket, key, nil
}

// copyRequestSource reads and checks the source a copy request names: the
// copy source header, the namespace rules every key of a URL is held to, and an
// empty body, since the bytes come from the source.
func (g *Gateway) copyRequestSource(r *http.Request) (string, string, *objectFailure) {
	srcBucket, srcKey, failure := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if failure == nil {
		failure = g.bucketFailure(srcBucket)
	}
	if failure == nil {
		failure = g.keyFailure(srcBucket, srcKey)
	}
	if failure == nil && (!validateBucketName(srcBucket) || !validateKey(srcKey)) {
		failure = &objectFailure{http.StatusBadRequest, "InvalidArgument", "Invalid argument"}
	}
	if failure == nil {
		_, failure = g.requireEmptyBody(r, errCopyBody)
	}
	if failure == nil && !g.bucketExists(srcBucket) {
		failure = errNoSuchBucket
	}
	return srcBucket, srcKey, failure
}

// --- the source of a copy ---

// copySource is an open source object: fileops.Reader, behind the read fence.
type copySource interface {
	io.ReaderAt
	io.Closer
	Stat() (os.FileInfo, error)
}

// openCopySource opens a copy source through the read fence, so a quarantined
// or known-damaged generation is refused and a reader is revoked if its
// generation is fenced while it reads. Tests replace it to observe a copy in
// flight.
var openCopySource = func(root, path string) (copySource, error) {
	f, err := fileops.OpenReader(root, path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// sourceObject is one generation of a copy source, open for reading.
type sourceObject struct {
	name string // root-relative, as the index names it
	path string
	file copySource
	info os.FileInfo
}

// openSource opens the source of a copy and holds it to the request's
// x-amz-copy-source-if-* conditions. The caller closes it.
func (g *Gateway) openSource(r *http.Request, bucket, key string) (*sourceObject, error) {
	path, ok := g.objectPath(bucket, key)
	if !ok {
		return nil, &objectFailure{http.StatusBadRequest, "InvalidArgument", "Invalid key"}
	}
	// A folder, or a key below an object, is no object to copy.
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		if err == nil || readMissing(err) {
			return nil, errNoSuchKey
		}
		return nil, err
	}
	f, err := openCopySource(g.syncDir, path)
	if err != nil {
		if readMissing(err) {
			return nil, errNoSuchKey
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if evaluateConditions(r, copyConditions, info) != conditionMet {
		f.Close()
		return nil, errPreconditionFailed
	}
	name, err := filepath.Rel(g.syncDir, path)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &sourceObject{name: filepath.ToSlash(name), path: path, file: f, info: info}, nil
}

// unchanged checks that the source is still the generation that was opened,
// so the bytes read are all of one generation. A source replaced mid-copy
// must never be published as a mix of two.
func (s *sourceObject) unchanged() error {
	current, err := os.Stat(s.path)
	if err != nil || !fileops.SameGeneration(s.info, current) {
		return errSourceChanged
	}
	return nil
}

// sourceIndex is the part of the daemon's index that records each file's
// checksum. The store the daemon passes as Catalog has it; a gateway without
// an index copies what the read fence lets through.
type sourceIndex interface {
	GetFile(name string) (*store.FileMeta, error)
}

// indexedHash returns the SHA-256 the index recorded for the source
// generation. ok is false when the index cannot vouch for it: there is no
// index, the name is untracked, or the file is a generation the watcher has
// not indexed yet — a write it will itself take as new data.
func (g *Gateway) indexedHash(src *sourceObject) (string, bool, error) {
	index, ok := g.config.Catalog.(sourceIndex)
	if !ok {
		return "", false, nil
	}
	meta, err := index.GetFile(src.name)
	if err != nil {
		return "", false, err
	}
	if meta == nil || meta.Deleted || meta.Size != src.info.Size() || meta.ModTime != src.info.ModTime().UnixNano() {
		return "", false, nil
	}
	return meta.Hash, true, nil
}

// checkSourceHash compares the SHA-256 of a source's bytes, as a copy read
// them, with the one recorded for that generation.
func (g *Gateway) checkSourceHash(src *sourceObject, sum hash.Hash) error {
	want, ok, err := g.indexedHash(src)
	if err != nil || !ok {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		g.logger.Error("copy source does not match its recorded checksum; not copied",
			"source", src.name, "recorded", want, "read", got)
		return errSourceDamaged
	}
	return nil
}

// sourceCheckTTL is how long a source generation verified in full stays
// trusted for the parts of a multipart copy. A copy of a large object takes
// minutes; bytes that rot later are the scrub's to find.
const sourceCheckTTL = time.Hour

// sourceChecks remembers source generations verified against the index, so a
// multipart copy reads its source in full once rather than once per part, and
// parts copied in parallel wait for one verification instead of each running
// their own.
type sourceChecks struct {
	mu     sync.Mutex
	checks map[string]*sourceCheck
}

type sourceCheck struct {
	done chan struct{}
	err  error
	at   time.Time
}

// verify runs check once for the generation key and returns its outcome to
// every caller asking within the TTL. A failure is not remembered: the next
// part checks again.
func (c *sourceChecks) verify(key string, check func() error) error {
	now := time.Now()
	c.mu.Lock()
	if c.checks == nil {
		c.checks = make(map[string]*sourceCheck)
	}
	for k, old := range c.checks {
		select {
		case <-old.done:
			if now.Sub(old.at) > sourceCheckTTL {
				delete(c.checks, k)
			}
		default:
		}
	}
	if existing, ok := c.checks[key]; ok {
		c.mu.Unlock()
		<-existing.done
		return existing.err
	}
	pending := &sourceCheck{done: make(chan struct{}), at: now}
	c.checks[key] = pending
	c.mu.Unlock()

	pending.err = check()
	if pending.err != nil {
		c.mu.Lock()
		delete(c.checks, key)
		c.mu.Unlock()
	}
	close(pending.done)
	return pending.err
}

// verifySource checks a whole source generation against its recorded
// checksum, once per generation (see sourceChecks). A part copies only a range,
// so this is what keeps a multipart copy from re-publishing damaged bytes.
func (g *Gateway) verifySource(src *sourceObject) error {
	if _, ok, err := g.indexedHash(src); err != nil || !ok {
		return err
	}
	key, err := fileops.GenerationKey(src.path, src.info)
	if err != nil {
		return err
	}
	return g.sourceChecks.verify(key, func() error {
		sum := sha256.New()
		buf := make([]byte, copyBufferSize)
		if _, err := io.CopyBuffer(sum, io.NewSectionReader(src.file, 0, src.info.Size()), buf); err != nil {
			return err
		}
		if err := src.unchanged(); err != nil {
			return err
		}
		return g.checkSourceHash(src, sum)
	})
}

// copyFailure answers a failed copy, logging what is this server's fault.
func (g *Gateway) copyFailure(w http.ResponseWriter, err error, srcBucket, srcKey, bucket, key string) {
	if failure := writeFailure(err); failure != nil {
		writeObjectFailure(w, failure)
		return
	}
	g.logger.Error("copy failed", "from", srcBucket+"/"+srcKey, "to", bucket+"/"+key, "error", err)
	writeS3Error(w, http.StatusInternalServerError, "InternalError", "Failed to copy object")
}

// writeCopyResult answers a CopyObject with the ETag and timestamp of the
// destination as the copy left it, which is what a HEAD of it reports.
func writeCopyResult(w http.ResponseWriter, etag string, info os.FileInfo) {
	writeXML(w, http.StatusOK, CopyObjectResult{
		Xmlns:        s3Xmlns,
		ETag:         etag,
		LastModified: info.ModTime().UTC().Format(s3TimeFormat),
	})
}

// --- CopyObject ---

// handleCopyObject copies an object within or across buckets
// (PUT /{bucket}/{key} with x-amz-copy-source).
//
// The copy is staged the way a PutObject body is, with no commit lock held:
// the source is read through the read fence into a scratch file beside the
// destination, hashed as it streams and flushed, and then published with the
// same short rename a PUT ends with. Before it is published, the bytes must be
// one generation of the source and, where the index recorded a checksum for
// that generation, match it: a source replaced or rotted underneath the copy is
// never re-published as a trusted new write. A crash mid-copy leaves only the
// scratch file, which the janitor removes.
//
// Metadata is not stored, so a metadata directive changes nothing, and copying
// an object onto itself — what clients do to "replace" metadata — leaves it
// exactly as it is.
func (g *Gateway) handleCopyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	srcBucket, srcKey, failure := g.copyRequestSource(r)
	if failure == nil && (!validateBucketName(bucket) || !validateKey(key)) {
		failure = &objectFailure{http.StatusBadRequest, "InvalidArgument", "Invalid argument"}
	}
	if failure == nil {
		switch directive := strings.ToUpper(strings.TrimSpace(r.Header.Get("X-Amz-Metadata-Directive"))); directive {
		case "", "COPY", "REPLACE":
		default:
			failure = &objectFailure{http.StatusBadRequest, "InvalidArgument", "Unknown metadata directive."}
		}
	}
	if failure == nil && !g.bucketExists(bucket) {
		failure = errNoSuchBucket
	}
	if failure != nil {
		writeObjectFailure(w, failure)
		return
	}
	if isFolderKey(srcKey) || isFolderKey(key) {
		g.copyFolder(w, r, srcBucket, srcKey, bucket, key)
		return
	}

	dstPath, ok := g.objectPath(bucket, key)
	if !ok {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Invalid key")
		return
	}
	src, err := g.openSource(r, srcBucket, srcKey)
	if err != nil {
		g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
		return
	}
	defer src.file.Close()

	dstInfo, dstErr := os.Stat(dstPath)
	if (srcBucket == bucket && srcKey == key) || (dstErr == nil && os.SameFile(src.info, dstInfo)) {
		g.logger.Debug("object copied onto itself", "bucket", bucket, "key", key)
		writeCopyResult(w, etagFor(src.info), src.info)
		return
	}
	if dstErr == nil && dstInfo.IsDir() {
		writeObjectFailure(w, errObjectIsDirectory)
		return
	}
	if g.config.MaxUploadBytes > 0 && src.info.Size() > g.config.MaxUploadBytes {
		writeObjectFailure(w, errCopyTooLarge)
		return
	}

	dir := filepath.Dir(dstPath)
	// A copy that fails takes back the directories it created for the key,
	// so a refused copy does not leave a folder behind.
	created := firstMissingDir(dir)
	if err := fileops.Mkdir(g.syncDir, dir, 0o755, true); err != nil {
		g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
		return
	}
	tmp, err := fileops.CreateTemp(dir, ".birak-tmp-*")
	if err != nil {
		g.removeCreatedDirs(dir, created)
		g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
		return
	}
	defer fileops.ReleaseTemp(tmp)
	tmpPath := tmp.Name()
	if err := g.stageCopy(src, tmp); err != nil {
		os.Remove(tmpPath)
		g.removeCreatedDirs(dir, created)
		g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
		return
	}

	info, err := fileops.PublishIf(g.syncDir, tmpPath, dstPath, nil)
	if err != nil {
		os.Remove(tmpPath)
		g.removeCreatedDirs(dir, created)
		if failure := publishFailure(err, dstPath); failure != nil {
			writeObjectFailure(w, failure)
			return
		}
		g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
		return
	}

	g.logger.Info("object copied", "from", srcBucket+"/"+srcKey, "to", bucket+"/"+key, "size", info.Size())
	writeCopyResult(w, etagFor(info), info)
}

// firstMissingDir returns the outermost ancestor of dir (or dir itself) that
// does not exist yet, or "" when dir exists.
func firstMissingDir(dir string) string {
	missing := ""
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			return missing
		}
		missing = d
		if d == filepath.Dir(d) {
			return missing
		}
	}
}

// removeCreatedDirs removes the directories from dir up to created that are
// still empty, under the commit lock like any other namespace change.
func (g *Gateway) removeCreatedDirs(dir, created string) {
	if created == "" {
		return
	}
	_ = fileops.Do(g.syncDir, func() error {
		for d := dir; ; d = filepath.Dir(d) {
			if os.Remove(d) != nil || d == created || d == filepath.Dir(d) {
				return nil
			}
		}
	})
}

// stageCopy fills tmp with the bytes of src and flushes it, checking that they
// are all of the generation that was opened and match its recorded checksum.
// It closes tmp.
func (g *Gateway) stageCopy(src *sourceObject, tmp *os.File) error {
	sum := sha256.New()
	buf := make([]byte, copyBufferSize)
	// One byte past the expected size, so a source that grows underneath the
	// copy is noticed rather than copied without bound.
	n, err := io.CopyBuffer(io.MultiWriter(tmp, sum), io.NewSectionReader(src.file, 0, src.info.Size()+1), buf)
	if err == nil && n != src.info.Size() {
		err = errSourceChanged
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	if err := src.unchanged(); err != nil {
		return err
	}
	return g.checkSourceHash(src, sum)
}

// copyFolder copies a folder marker to another folder marker, which makes the
// destination folder. A marker holds no data, so a folder cannot become an
// object or the other way round.
func (g *Gateway) copyFolder(w http.ResponseWriter, r *http.Request, srcBucket, srcKey, bucket, key string) {
	if !isFolderKey(srcKey) || !isFolderKey(key) {
		writeObjectFailure(w, errFolderCopy)
		return
	}
	srcDir, failure := g.folderPath(srcBucket, srcKey)
	if failure == nil {
		var dir string
		dir, failure = g.folderPath(bucket, key)
		if failure == nil {
			info, err := os.Stat(srcDir)
			switch {
			case err == nil && info.IsDir():
				if err := g.makeFolder(dir); err != nil {
					g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
					return
				}
				if info, err = os.Stat(dir); err != nil {
					g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
					return
				}
				writeCopyResult(w, emptyETag, info)
				return
			case err == nil || readMissing(err):
				failure = errNoSuchKey
			default:
				g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
				return
			}
		}
	}
	writeObjectFailure(w, failure)
}

// --- UploadPartCopy ---

// parseCopyRange reads x-amz-copy-source-range ("bytes=first-last", both
// offsets inclusive) against a source of size bytes and returns the offset and
// length to copy; without the header, the part is the whole source. A range
// that is malformed or does not lie within the source is refused, as S3
// refuses it.
func parseCopyRange(raw string, size int64) (int64, int64, *objectFailure) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, size, nil
	}
	spec, ok := strings.CutPrefix(raw, "bytes=")
	if !ok {
		return 0, 0, errCopyRange
	}
	firstRaw, lastRaw, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, errCopyRange
	}
	first, err1 := strconv.ParseInt(firstRaw, 10, 64)
	last, err2 := strconv.ParseInt(lastRaw, 10, 64)
	if err1 != nil || err2 != nil || first < 0 || last < first {
		return 0, 0, errCopyRange
	}
	if last >= size {
		return 0, 0, &objectFailure{http.StatusBadRequest, "InvalidArgument",
			fmt.Sprintf("Range specified is not valid for source object of size: %d", size)}
	}
	return first, last - first + 1, nil
}

// rangeReader reads a range of a source and, once it is through, checks that
// the source is still the generation it started from; a failed check is
// returned in place of io.EOF, so the part is discarded rather than staged.
type rangeReader struct {
	r   io.Reader
	src *sourceObject
}

func (r *rangeReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err == io.EOF {
		if changed := r.src.unchanged(); changed != nil {
			return n, changed
		}
	}
	return n, err
}

// handleUploadPartCopy stages a part from a range of an existing object
// (PUT /{bucket}/{key}?partNumber=N&uploadId=ID with x-amz-copy-source).
//
// It is what the AWS CLI and SDK transfer managers use to copy an object above
// their multipart threshold (8 MiB by default), so every APK copy goes this
// way. The source is held to the same rules as CopyObject's, the range is
// streamed into the part with no commit lock held, and the part is staged
// exactly as an uploaded one is, so ListParts and the completion know it by
// the same ETag.
func (g *Gateway) handleUploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if _, ok := g.multipartReady(w, bucket, key); !ok {
		return
	}
	partNumber, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("partNumber")))
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument",
			"Part number must be an integer between 1 and 10000, inclusive.")
		return
	}
	if _, ok := g.ownedUpload(w, uploadID, bucket, key); !ok {
		return
	}
	srcBucket, srcKey, failure := g.copyRequestSource(r)
	if failure == nil && isFolderKey(srcKey) {
		failure = errFolderCopy
	}
	if failure != nil {
		writeObjectFailure(w, failure)
		return
	}

	src, err := g.openSource(r, srcBucket, srcKey)
	if err != nil {
		g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
		return
	}
	defer src.file.Close()

	offset, length, failure := parseCopyRange(r.Header.Get("X-Amz-Copy-Source-Range"), src.info.Size())
	if failure == nil {
		partLimit := g.multipart.Limits().MaxPartBytes
		if g.config.MaxUploadBytes > 0 && g.config.MaxUploadBytes < partLimit {
			partLimit = g.config.MaxUploadBytes
		}
		if length > partLimit {
			failure = &objectFailure{http.StatusRequestEntityTooLarge, "EntityTooLarge",
				"Your proposed upload exceeds the maximum allowed size"}
		}
	}
	if failure != nil {
		writeObjectFailure(w, failure)
		return
	}
	if err := g.verifySource(src); err != nil {
		g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
		return
	}

	body := &rangeReader{r: io.NewSectionReader(src.file, offset, length), src: src}
	part, err := g.multipart.WritePart(uploadID, partNumber, body, multipart.Checksums{})
	if err != nil {
		var failure *objectFailure
		if errors.As(err, &failure) || writeFailure(err) != nil {
			g.copyFailure(w, err, srcBucket, srcKey, bucket, key)
			return
		}
		g.writeMultipartError(w, err, "upload-part-copy", bucket, key)
		return
	}

	g.logger.Debug("part copied", "bucket", bucket, "key", key, "upload_id", uploadID,
		"part", partNumber, "from", srcBucket+"/"+srcKey, "offset", offset, "size", part.Size)
	writeXML(w, http.StatusOK, CopyPartResult{
		Xmlns:        s3Xmlns,
		ETag:         `"` + part.ETag + `"`,
		LastModified: part.LastModified.UTC().Format(s3TimeFormat),
	})
}
