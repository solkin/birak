package s3

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/birak/birak/internal/generation"
	"github.com/birak/birak/internal/quorum"
)

func qBucket(bucket string) string      { return "b/" + bucket }
func qObjects(bucket string) string     { return "o/" + bucket + "/" }
func qObject(bucket, key string) string { return qObjects(bucket) + key }
func operationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func qTime(e quorum.Entry) time.Time { return time.Unix(0, e.Modified).UTC() }
func qETag(e quorum.Entry) string {
	tag := e.Attributes.ETag
	if tag == "" {
		tag = e.Ref.Hash
	}
	return `"` + tag + `"`
}

func (g *Gateway) qError(w http.ResponseWriter, err error) {
	if errors.Is(err, quorum.ErrCondition) {
		writeS3Error(w, 409, "OperationAborted", "A concurrent operation changed the object or bucket; retry")
		return
	}
	if errors.Is(err, quorum.ErrOperationID) {
		writeS3Error(w, 409, "InvalidRequest", "Operation ID was reused with different content")
		return
	}
	g.logger.Warn("quorum request failed", "error", err)
	w.Header().Set("Retry-After", "1")
	writeS3Error(w, 503, "SlowDown", "The operation could not be confirmed; a timeout may have committed")
}

func (g *Gateway) routeQuorum(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "PUT" && (r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != "") {
		writeS3Error(w, 501, "NotImplemented", "Conditional operation is not supported")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.config.TransferTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	if r.Method == "PUT" || r.Method == "POST" {
		select {
		case g.uploads <- struct{}{}:
			defer func() { <-g.uploads }()
		default:
			writeS3Error(w, 503, "SlowDown", "Upload capacity exceeded")
			return
		}
	}
	// Never acknowledge request features whose semantics are not implemented.
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") {
			switch lower {
			case "x-amz-date", "x-amz-content-sha256", "x-amz-copy-source", "x-amz-user-agent":
			default:
				writeS3Error(w, 501, "NotImplemented", "Unsupported object metadata or storage feature")
				return
			}
		}
		if r.Method == "PUT" || r.Method == "POST" {
			switch lower {
			case "content-encoding", "content-disposition", "cache-control", "expires":
				writeS3Error(w, 501, "NotImplemented", "Unsupported stored HTTP metadata")
				return
			}
		}

	}
	if len(r.Header.Get("Content-Type")) > 1024 {
		writeS3Error(w, 400, "InvalidArgument", "Content-Type is too large")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if v := g.extractBucketFromHost(r.Host); v != "" {
		bucket = v
		key = path
	}
	if bucket == "" {
		if r.Method != "GET" {
			writeS3Error(w, 405, "MethodNotAllowed", "Method not allowed")
			return
		}
		records, err := g.config.Quorum.View(r.Context(), "b/")
		if err != nil {
			g.qError(w, err)
			return
		}
		result := ListAllMyBucketsResult{Xmlns: s3Xmlns, Owner: birakOwner()}
		for _, rec := range records {
			result.Buckets.Bucket = append(result.Buckets.Bucket, BucketInfo{Name: strings.TrimPrefix(rec.Key, "b/"), CreationDate: qTime(rec.Entry).Format(s3TimeFormat)})
		}
		writeXML(w, 200, result)
		return
	}
	if !validateBucketName(bucket) || len(bucket) > 63 || !utf8.ValidString(bucket) || strings.ContainsAny(bucket, "\x00\r\n") {
		writeS3Error(w, 400, "InvalidBucketName", "Invalid bucket name")
		return
	}
	if key != "" && (len(key) > 1024 || !utf8.ValidString(key) || strings.ContainsRune(key, 0)) {
		writeS3Error(w, 400, "InvalidArgument", "Invalid key")
		return
	}
	q := r.URL.Query()
	for name := range q {
		if strings.HasPrefix(name, "X-Amz-") {
			delete(q, name)
		}
	}
	if _, ok := q["versionId"]; ok {
		writeS3Error(w, 501, "NotImplemented", "Public version IDs are not supported")
		return
	}
	if key == "" && r.Method == "PUT" && len(q) == 0 {
		if r.ContentLength != 0 {
			var request struct {
				XMLName  xml.Name `xml:"CreateBucketConfiguration"`
				Location string   `xml:"LocationConstraint"`
			}
			if _, err := readXMLRequest(r, &request); err != nil {
				g.qPayloadError(w, err)
				return
			}
			if request.Location != "" && request.Location != "us-east-1" {
				writeS3Error(w, 400, "InvalidLocationConstraint", "Only us-east-1 is supported")
				return
			}
		}
		_, ok, err := g.config.Quorum.Lookup(r.Context(), qBucket(bucket))
		if err != nil {
			g.qError(w, err)
			return
		}
		if !ok {
			_, err = g.config.Quorum.Transact(r.Context(), operationID(), []quorum.Change{{Key: qBucket(bucket), MetaOnly: true}}, []quorum.Condition{{Key: qBucket(bucket)}})
			if err != nil {
				g.qError(w, err)
				return
			}
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(200)
		return
	}
	be, exists, err := g.config.Quorum.Lookup(r.Context(), qBucket(bucket))
	if err != nil {
		g.qError(w, err)
		return
	}
	if !exists {
		writeS3Error(w, 404, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	if key == "" {
		if (r.Method == "DELETE" || r.Method == "HEAD") && len(q) != 0 {
			writeS3Error(w, 501, "NotImplemented", "Unsupported bucket resource")
			return
		}
		if _, ok := q["uploads"]; ok && r.Method == "GET" {
			g.qListUploads(w, r, bucket)
			return
		}
		if _, ok := q["delete"]; ok && r.Method == "POST" {
			g.qDeleteObjects(w, r, bucket, be)
			return
		}
		if _, ok := q["location"]; ok && r.Method == "GET" {
			writeXML(w, 200, LocationConstraint{Xmlns: s3Xmlns})
			return
		}
		if _, ok := q["versioning"]; ok && r.Method == "GET" {
			writeXML(w, 200, VersioningConfiguration{Xmlns: s3Xmlns})
			return
		}
		switch r.Method {
		case "HEAD":
			w.Header().Set("x-amz-bucket-region", "us-east-1")
			w.WriteHeader(200)
		case "GET":
			if !qListingParameters(q) {
				writeS3Error(w, 501, "NotImplemented", "Unsupported bucket resource")
				return
			}
			g.qListObjects(w, r, bucket)
		case "DELETE":
			_, err := g.config.Quorum.Transact(r.Context(), operationID(), []quorum.Change{{Key: qBucket(bucket), Delete: true}}, []quorum.Condition{{Key: qBucket(bucket), Index: be.Index}, {EmptyPrefix: qObjects(bucket)}, {EmptyPrefix: qUploads(bucket)}})
			if errors.Is(err, quorum.ErrCondition) {
				writeS3Error(w, 409, "BucketNotEmpty", "The bucket has objects or active uploads")
				return
			}
			if err != nil {
				g.qError(w, err)
				return
			}
			w.WriteHeader(204)
		default:
			writeS3Error(w, 501, "NotImplemented", "Unsupported bucket operation")
		}
		return
	}
	if _, ok := q["uploads"]; ok || q.Get("uploadId") != "" || q.Get("partNumber") != "" {
		if r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != "" {
			writeS3Error(w, 501, "NotImplemented", "Conditional multipart operation is not supported")
			return
		}
		for name := range q {
			switch name {
			case "uploads", "uploadId", "partNumber", "max-parts", "part-number-marker":
			default:
				writeS3Error(w, 501, "NotImplemented", "Unsupported multipart parameter")
				return
			}
		}
		g.qMultipart(w, r, bucket, key, be)
		return
	}
	if len(q) > 0 {
		// Authentication query parameters do not select an S3 sub-resource.
		for name := range q {
			if !strings.HasPrefix(name, "X-Amz-") {
				writeS3Error(w, 501, "NotImplemented", "Unsupported object resource")
				return
			}
		}
	}
	switch r.Method {
	case "GET", "HEAD":
		g.qGet(w, r, bucket, key)
	case "PUT":
		g.qPut(w, r, bucket, key, be)
	case "DELETE":
		_, err := g.config.Quorum.Transact(r.Context(), operationID(), []quorum.Change{{Key: qObject(bucket, key), Delete: true}}, []quorum.Condition{{Key: qBucket(bucket), Index: be.Index}})
		if err != nil {
			g.qError(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		writeS3Error(w, 405, "MethodNotAllowed", "Method not allowed")
	}
}

func (g *Gateway) qGet(w http.ResponseWriter, r *http.Request, bucket, key string) {
	e, ok, err := g.config.Quorum.Lookup(r.Context(), qObject(bucket, key))
	if err != nil {
		g.qError(w, err)
		return
	}
	if !ok {
		writeS3Error(w, 404, "NoSuchKey", "The specified key does not exist")
		return
	}
	f, err := g.config.Quorum.OpenBlob(r.Context(), e.Ref)
	if err != nil {
		g.qError(w, err)
		return
	}
	defer f.Close()
	seeker, ok := f.(io.ReadSeeker)
	if !ok {
		g.qError(w, errors.New("generation reader is not seekable"))
		return
	}
	w.Header().Set("ETag", qETag(e))
	w.Header().Set("Content-Type", e.Attributes.ContentType)
	if e.Attributes.ContentType == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, key, qTime(e), seeker)
}

var errPayloadMD5 = errors.New("content MD5 mismatch")
var errPayloadSHA = errors.New("content SHA256 mismatch")

func (g *Gateway) qStage(ctx context.Context, r *http.Request, limit int64) (generation.Ref, string, error) {
	md := md5.New()
	sha := sha256.New()
	ref, err := g.config.Quorum.Stage(ctx, io.TeeReader(r.Body, io.MultiWriter(md, sha)), limit)
	if err != nil {
		return ref, "", err
	}
	if r.ContentLength >= 0 && ref.Size != r.ContentLength {
		return ref, "", io.ErrUnexpectedEOF
	}
	if want := r.Header.Get("Content-MD5"); want != "" && want != base64.StdEncoding.EncodeToString(md.Sum(nil)) {
		return ref, "", errPayloadMD5
	}
	if want := r.Header.Get("X-Amz-Content-Sha256"); want != "" && want != "UNSIGNED-PAYLOAD" {
		if !isHexSHA256(want) || !strings.EqualFold(want, hex.EncodeToString(sha.Sum(nil))) {
			return ref, "", errPayloadSHA
		}
	}
	return ref, hex.EncodeToString(md.Sum(nil)), nil
}
func (g *Gateway) qPayloadError(w http.ResponseWriter, err error) {
	if errors.Is(err, generation.ErrTooLarge) {
		writeS3Error(w, 413, "EntityTooLarge", "Object exceeds configured limit")
		return
	}
	if errors.Is(err, errXML) {
		writeS3Error(w, 400, "MalformedXML", "Invalid XML request")
		return
	}

	if errors.Is(err, errPayloadMD5) {
		writeS3Error(w, 400, "BadDigest", "Content-MD5 mismatch")
		return
	}
	if errors.Is(err, errPayloadSHA) {
		writeS3Error(w, 400, "XAmzContentSHA256Mismatch", "Payload SHA256 mismatch")
		return
	}
	g.qError(w, err)
}
func (g *Gateway) qPut(w http.ResponseWriter, r *http.Request, bucket, key string, be quorum.Entry) {
	var ref generation.Ref
	var tag string
	var err error
	attrs := quorum.Attributes{ContentType: r.Header.Get("Content-Type")}
	if attrs.ContentType == "" {
		attrs.ContentType = "application/octet-stream"
	}
	conditions := []quorum.Condition{{Key: qBucket(bucket), Index: be.Index}}
	if r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != "" {
		target, exists, e := g.config.Quorum.Lookup(r.Context(), qObject(bucket, key))
		if e != nil {
			g.qError(w, e)
			return
		}
		match, none := r.Header.Get("If-Match"), r.Header.Get("If-None-Match")
		if none != "" && none != "*" {
			writeS3Error(w, 400, "InvalidArgument", "If-None-Match must be *")
			return
		}
		if (none == "*" && exists) || (match != "" && (!exists || (match != "*" && match != qETag(target)))) {
			writeS3Error(w, 412, "PreconditionFailed", "Object precondition failed")
			return
		}
		index := uint64(0)
		if exists {
			index = target.Index
		}
		conditions = append(conditions, quorum.Condition{Key: qObject(bucket, key), Index: index})
	}
	copySource := r.Header.Get("X-Amz-Copy-Source")
	if copySource != "" {
		source, e := url.PathUnescape(strings.TrimPrefix(copySource, "/"))
		if e != nil || strings.Contains(source, "?") {
			writeS3Error(w, 400, "InvalidArgument", "Invalid copy source")
			return
		}
		sb, sk, _ := strings.Cut(source, "/")
		entry, ok, e := g.config.Quorum.Lookup(r.Context(), qObject(sb, sk))
		if e != nil {
			g.qError(w, e)
			return
		}
		if !ok {
			writeS3Error(w, 404, "NoSuchKey", "Copy source missing")
			return
		}
		ref = entry.Ref
		attrs = entry.Attributes
		tag = attrs.ETag
		conditions = append(conditions, quorum.Condition{Key: qObject(sb, sk), Index: entry.Index})
	} else {
		if r.ContentLength > g.config.MaxUploadBytes {
			writeS3Error(w, 413, "EntityTooLarge", "Object exceeds configured limit")
			return
		}
		ref, tag, err = g.qStage(r.Context(), r, g.config.MaxUploadBytes)
		if err != nil {
			g.qPayloadError(w, err)
			return
		}
	}
	attrs.ETag = tag
	e, err := g.config.Quorum.Transact(r.Context(), operationID(), []quorum.Change{{Key: qObject(bucket, key), Ref: ref, Attributes: attrs}}, conditions)
	if err != nil {
		g.qError(w, err)
		return
	}
	w.Header().Set("ETag", qETag(e))
	if copySource != "" {
		writeXML(w, 200, struct {
			XMLName      xml.Name `xml:"CopyObjectResult"`
			ETag         string
			LastModified string
		}{ETag: qETag(e), LastModified: qTime(e).Format(s3TimeFormat)})
		return
	}
	w.WriteHeader(200)
}

func (g *Gateway) qListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	maxKeys, ok := parseMaxKeys(r)
	if !ok {
		writeS3Error(w, 400, "InvalidArgument", "Invalid max-keys")
		return
	}
	q := r.URL.Query()
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	records, err := g.config.Quorum.View(r.Context(), qObjects(bucket)+prefix)
	if err != nil {
		g.qError(w, err)
		return
	}
	var objects []ObjectInfo
	prefixes := map[string]bool{}
	for _, rec := range records {
		key := strings.TrimPrefix(rec.Key, qObjects(bucket))
		if delimiter != "" {
			if i := strings.Index(strings.TrimPrefix(key, prefix), delimiter); i >= 0 {
				prefixes[key[:len(prefix)+i+len(delimiter)]] = true
				continue
			}
		}
		objects = append(objects, ObjectInfo{Key: key, Size: rec.Entry.Ref.Size, ETag: qETag(rec.Entry), LastModified: qTime(rec.Entry).Format(s3TimeFormat), StorageClass: "STANDARD"})
	}
	var cp []string
	for p := range prefixes {
		cp = append(cp, p)
	}
	sort.Strings(cp)
	start := q.Get("marker")
	if q.Get("list-type") == "2" {
		start = q.Get("start-after")
		if q.Get("continuation-token") != "" {
			start = q.Get("continuation-token")
		}
	}
	objects, ps, truncated, next := paginate(objects, cp, start, maxKeys)
	if q.Get("list-type") == "2" {
		writeXML(w, 200, ListBucketResultV2{Xmlns: s3Xmlns, Name: bucket, Prefix: prefix, Delimiter: delimiter, MaxKeys: maxKeys, KeyCount: len(objects) + len(ps), Contents: objects, CommonPrefixes: ps, IsTruncated: truncated, NextContinuationToken: next, ContinuationToken: q.Get("continuation-token"), StartAfter: q.Get("start-after")})
	} else {
		writeXML(w, 200, ListBucketResultV1{Xmlns: s3Xmlns, Name: bucket, Prefix: prefix, Delimiter: delimiter, MaxKeys: maxKeys, Marker: start, NextMarker: next, Contents: objects, CommonPrefixes: ps, IsTruncated: truncated})
	}
}

func (g *Gateway) qDeleteObjects(w http.ResponseWriter, r *http.Request, bucket string, be quorum.Entry) {
	var request struct {
		XMLName xml.Name `xml:"Delete"`
		Quiet   bool     `xml:"Quiet"`
		Objects []struct {
			Key       string  `xml:"Key"`
			VersionID *string `xml:"VersionId"`
		} `xml:"Object"`
	}
	if _, err := readXMLRequest(r, &request); err != nil {
		g.qPayloadError(w, err)
		return
	}
	if len(request.Objects) > 1000 {
		writeS3Error(w, 400, "MalformedXML", "Invalid delete request")
		return
	}
	result := DeleteObjectsResult{}
	for _, object := range request.Objects {
		if object.VersionID != nil {
			result.Errors = append(result.Errors, DeleteObjectError{Key: object.Key, Code: "NotImplemented", Message: "Version IDs are not supported"})
			continue
		}
		if object.Key == "" || len(object.Key) > 1024 || !utf8.ValidString(object.Key) || strings.ContainsRune(object.Key, 0) {
			result.Errors = append(result.Errors, DeleteObjectError{Key: object.Key, Code: "InvalidArgument", Message: "Invalid key"})
			continue
		}
		_, err := g.config.Quorum.Transact(r.Context(), operationID(), []quorum.Change{{Key: qObject(bucket, object.Key), Delete: true}}, []quorum.Condition{{Key: qBucket(bucket), Index: be.Index}})
		if err != nil {
			result.Errors = append(result.Errors, DeleteObjectError{Key: object.Key, Code: "SlowDown", Message: "Deletion not confirmed"})
		} else if !request.Quiet {
			result.Deleted = append(result.Deleted, DeletedObject{Key: object.Key})
		}
	}
	writeXML(w, 200, result)
}

func qListingParameters(q url.Values) bool {
	for name := range q {
		switch name {
		case "list-type", "prefix", "delimiter", "marker", "max-keys", "start-after", "continuation-token":
		default:
			return false
		}
	}
	return q.Get("list-type") == "" || q.Get("list-type") == "2"
}
