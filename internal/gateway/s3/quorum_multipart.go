package s3

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/birak/birak/internal/generation"
	"github.com/birak/birak/internal/multipart"
	"github.com/birak/birak/internal/quorum"
)

func qUploads(bucket string) string       { return "u/" + bucket + "/" }
func qUpload(bucket, id string) string    { return qUploads(bucket) + id }
func qParts(id string) string             { return "p/" + id + "/" }
func qPart(id string, number int) string  { return qParts(id) + fmt.Sprintf("%05d", number) }
func qCompleted(bucket, id string) string { return "c/" + bucket + "/" + id }

type uploadRecord struct {
	Key         string
	BucketIndex uint64
	ContentType string
}
type completionRecord struct {
	Key          string
	BucketIndex  uint64
	Digest, ETag string
}

func recordJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func (g *Gateway) qLimits() multipart.Limits {
	l := g.config.QuorumLimits
	if l.MinPartBytes == 0 {
		l.MinPartBytes = multipart.DefaultMinPartBytes
	}
	if l.MaxPartBytes == 0 {
		l.MaxPartBytes = multipart.DefaultMaxPartBytes
	}
	if l.MaxParts == 0 {
		l.MaxParts = multipart.DefaultMaxParts
	}
	return l
}

var errXML = errors.New("invalid XML request")

func readXMLRequest(r *http.Request, v any) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	if err != nil {
		return "", err
	}
	if len(b) > 2<<20 {
		return "", errXML
	}
	md := md5.Sum(b)
	sha := sha256.Sum256(b)
	if want := r.Header.Get("Content-MD5"); want != "" && want != base64.StdEncoding.EncodeToString(md[:]) {
		return "", errPayloadMD5
	}
	if want := r.Header.Get("X-Amz-Content-Sha256"); want != "" && want != "UNSIGNED-PAYLOAD" && !strings.EqualFold(want, hex.EncodeToString(sha[:])) {
		return "", errPayloadSHA
	}
	if err := xml.Unmarshal(b, v); err != nil {
		return "", errors.Join(errXML, err)
	}
	return hex.EncodeToString(sha[:]), nil
}

func (g *Gateway) qMultipart(w http.ResponseWriter, r *http.Request, bucket, key string, be quorum.Entry) {
	q := r.URL.Query()
	id := q.Get("uploadId")
	l := g.qLimits()
	if _, start := q["uploads"]; start && r.Method == "POST" && id == "" {
		if err := g.config.Quorum.Ready(r.Context()); err != nil {
			g.qError(w, err)
			return
		}
		// Every active upload is a replicated record. Resource limits are checked
		// again by the FSM, together with bucket existence.
		id = operationID()
		up := uploadRecord{Key: key, BucketIndex: be.Index, ContentType: r.Header.Get("Content-Type")}
		conditions := []quorum.Condition{{Key: qBucket(bucket), Index: be.Index}, {Key: qUpload(bucket, id)}}
		if l.MaxActiveUploads > 0 {
			conditions = append(conditions, quorum.Condition{Prefix: "u/", MaxCount: l.MaxActiveUploads})
		}
		_, err := g.config.Quorum.Transact(r.Context(), "init:"+id, []quorum.Change{{Key: qUpload(bucket, id), MetaOnly: true, Attributes: quorum.Attributes{Value: recordJSON(up)}}}, conditions)
		if err != nil {
			g.qError(w, err)
			return
		}
		writeXML(w, 200, InitiateMultipartUploadResult{Xmlns: s3Xmlns, Bucket: bucket, Key: key, UploadID: id})
		return
	}
	if len(id) != 32 {
		writeS3Error(w, 404, "NoSuchUpload", "The specified upload does not exist")
		return
	}
	if _, err := hex.DecodeString(id); err != nil {
		writeS3Error(w, 404, "NoSuchUpload", "The specified upload does not exist")
		return
	}
	ue, exists, err := g.config.Quorum.Lookup(r.Context(), qUpload(bucket, id))
	if err != nil {
		g.qError(w, err)
		return
	}
	if !exists {
		if r.Method == "POST" {
			g.qRepeatComplete(w, r, bucket, key, id, be)
			return
		}
		writeS3Error(w, 404, "NoSuchUpload", "The specified upload does not exist")
		return
	}
	var up uploadRecord
	if json.Unmarshal([]byte(ue.Attributes.Value), &up) != nil || up.Key != key || up.BucketIndex != be.Index {
		writeS3Error(w, 404, "NoSuchUpload", "The specified upload does not exist")
		return
	}
	conditions := []quorum.Condition{{Key: qBucket(bucket), Index: be.Index}, {Key: qUpload(bucket, id), Index: ue.Index}}
	switch r.Method {
	case "PUT":
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			writeS3Error(w, 501, "NotImplemented", "UploadPartCopy is not supported")
			return
		}
		number, err := strconv.Atoi(q.Get("partNumber"))
		if err != nil || number < 1 || number > l.MaxParts {
			g.writeMultipartError(w, multipart.ErrInvalidPartNumber, "part", bucket, key)
			return
		}
		limit := min(g.config.MaxUploadBytes, l.MaxPartBytes)
		if r.ContentLength > limit {
			g.writeMultipartError(w, multipart.ErrPartTooLarge, "part", bucket, key)
			return
		}
		ref, tag, err := g.qStage(r.Context(), r, limit)
		if err != nil {
			g.qPayloadError(w, err)
			return
		}
		// Updating the upload's index fences Complete/Abort against every part
		// overwrite. Concurrent part uploads retry just the metadata transaction.
		err = retryPartCommit(func() error {
			_, e := g.config.Quorum.Transact(r.Context(), operationID(), []quorum.Change{{Key: qPart(id, number), Ref: ref, Attributes: quorum.Attributes{ETag: tag}}, {Key: qUpload(bucket, id), MetaOnly: true, Attributes: ue.Attributes}}, conditions)
			return e
		}, func() error {
			var e error
			ue, exists, e = g.config.Quorum.Lookup(r.Context(), qUpload(bucket, id))
			if e != nil {
				return e
			}
			if !exists {
				return quorum.ErrCondition
			}
			conditions[1].Index = ue.Index
			return nil
		})
		if !exists {
			writeS3Error(w, 404, "NoSuchUpload", "Upload was closed")
			return
		}
		if err != nil {
			g.qError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+tag+`"`)
		w.WriteHeader(200)
	case "POST":
		g.qComplete(w, r, bucket, key, id, be, ue, up)
	case "DELETE":
		_, err := g.config.Quorum.Transact(r.Context(), operationID(), []quorum.Change{{Key: qUpload(bucket, id), Delete: true}}, conditions)
		if err != nil {
			g.qError(w, err)
			return
		}
		w.WriteHeader(204)
	case "GET":
		g.qListParts(w, r, bucket, key, id, ue)
	default:
		writeS3Error(w, 405, "MethodNotAllowed", "Method not allowed")
	}
}

// One descriptor at a time, even with 10,000 parts. Each part is independently
// SHA-256 verified before it contributes bytes to the assembled generation.
type generationSequence struct {
	ctx     context.Context
	node    *quorum.Node
	refs    []generation.Ref
	current io.ReadCloser
}

func (s *generationSequence) Read(p []byte) (int, error) {
	for {
		if s.current == nil {
			if len(s.refs) == 0 {
				return 0, io.EOF
			}
			f, err := s.node.OpenBlob(s.ctx, s.refs[0])
			if err != nil {
				return 0, err
			}
			s.current = f
			s.refs = s.refs[1:]
		}
		n, err := s.current.Read(p)
		if err == io.EOF {
			s.current.Close()
			s.current = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}
func (s *generationSequence) Close() error {
	if s.current != nil {
		return s.current.Close()
	}
	return nil
}

func (g *Gateway) qComplete(w http.ResponseWriter, r *http.Request, bucket, key, id string, be, ue quorum.Entry, up uploadRecord) {
	var request CompleteMultipartUploadRequest
	_, err := readXMLRequest(r, &request)
	if err != nil {
		g.qPayloadError(w, err)
		return
	}
	if len(request.Parts) == 0 {
		g.writeMultipartError(w, multipart.ErrEmptyPartList, "complete", bucket, key)
		return
	}
	l := g.qLimits()
	if len(request.Parts) > l.MaxParts {
		g.writeMultipartError(w, multipart.ErrInvalidPart, "complete", bucket, key)
		return
	}
	records, err := g.config.Quorum.View(r.Context(), qParts(id))
	if err != nil {
		g.qError(w, err)
		return
	}
	parts := map[string]quorum.Entry{}
	for _, rec := range records {
		parts[rec.Key] = rec.Entry
	}
	var refs []generation.Ref
	var sum int64
	last := 0
	etagHash := md5.New()
	for i, part := range request.Parts {
		if part.PartNumber <= last {
			g.writeMultipartError(w, multipart.ErrInvalidPartOrder, "complete", bucket, key)
			return
		}
		last = part.PartNumber
		p, ok := parts[qPart(id, part.PartNumber)]
		tag := strings.Trim(part.ETag, `"`)
		if !ok || p.Attributes.ETag != tag {
			g.writeMultipartError(w, multipart.ErrInvalidPart, "complete", bucket, key)
			return
		}
		if i < len(request.Parts)-1 && p.Ref.Size < l.MinPartBytes {
			g.writeMultipartError(w, multipart.ErrPartTooSmall, "complete", bucket, key)
			return
		}
		if p.Ref.Size > g.config.MaxUploadBytes-sum {
			g.writeMultipartError(w, multipart.ErrObjectTooLarge, "complete", bucket, key)
			return
		}
		sum += p.Ref.Size
		digest, err := hex.DecodeString(tag)
		if err != nil || len(digest) != md5.Size {
			g.qError(w, errors.New("invalid stored part checksum"))
			return
		}
		etagHash.Write(digest)
		refs = append(refs, p.Ref)
	}
	sequence := &generationSequence{ctx: r.Context(), node: g.config.Quorum, refs: refs}
	defer sequence.Close()
	ref, err := g.config.Quorum.Stage(r.Context(), sequence, g.config.MaxUploadBytes)
	if err != nil {
		g.qError(w, err)
		return
	}
	if ref.Size != sum {
		g.qError(w, errors.New("assembled size mismatch"))
		return
	}
	tag := hex.EncodeToString(etagHash.Sum(nil)) + "-" + strconv.Itoa(len(refs))
	receipt := completionRecord{Key: key, BucketIndex: be.Index, Digest: completionDigest(request), ETag: tag}
	_, err = g.config.Quorum.Transact(r.Context(), "complete:"+id, []quorum.Change{
		{Key: qObject(bucket, key), Ref: ref, Attributes: quorum.Attributes{ETag: tag, ContentType: up.ContentType}},
		{Key: qUpload(bucket, id), Delete: true},
		{Key: qCompleted(bucket, id), MetaOnly: true, Attributes: quorum.Attributes{Value: recordJSON(receipt)}},
	}, []quorum.Condition{{Key: qBucket(bucket), Index: be.Index}, {Key: qUpload(bucket, id), Index: ue.Index}})
	if err != nil {
		g.qError(w, err)
		return
	}
	g.qCompleteResult(w, bucket, key, tag)
}
func (g *Gateway) qRepeatComplete(w http.ResponseWriter, r *http.Request, bucket, key, id string, be quorum.Entry) {
	var request CompleteMultipartUploadRequest
	_, err := readXMLRequest(r, &request)
	if err != nil {
		g.qPayloadError(w, err)
		return
	}
	e, exists, err := g.config.Quorum.Lookup(r.Context(), qCompleted(bucket, id))
	if err != nil {
		g.qError(w, err)
		return
	}
	var receipt completionRecord
	if !exists || json.Unmarshal([]byte(e.Attributes.Value), &receipt) != nil || receipt.Key != key || receipt.BucketIndex != be.Index {
		writeS3Error(w, 404, "NoSuchUpload", "The specified upload does not exist")
		return
	}
	if receipt.Digest != completionDigest(request) {
		writeS3Error(w, 409, "InvalidRequest", "Completion was already committed with another part list")
		return
	}
	g.qCompleteResult(w, bucket, key, receipt.ETag)
}

// The receipt identifies the ordered part list, independently of XML whitespace.
func completionDigest(request CompleteMultipartUploadRequest) string {
	for i := range request.Parts {
		request.Parts[i].ETag = strings.Trim(request.Parts[i].ETag, `"`)
	}
	b, _ := json.Marshal(request.Parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (g *Gateway) qCompleteResult(w http.ResponseWriter, bucket, key, tag string) {
	writeXML(w, 200, CompleteMultipartUploadResult{Xmlns: s3Xmlns, Bucket: bucket, Key: key, ETag: `"` + tag + `"`, Location: "/" + bucket + "/" + key})
}

func (g *Gateway) qListParts(w http.ResponseWriter, r *http.Request, bucket, key, id string, ue quorum.Entry) {
	records, err := g.config.Quorum.View(r.Context(), qParts(id))
	if err != nil {
		g.qError(w, err)
		return
	}
	marker, _ := strconv.Atoi(r.URL.Query().Get("part-number-marker"))
	limit := 1000
	if s := r.URL.Query().Get("max-parts"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 {
			writeS3Error(w, 400, "InvalidArgument", "Invalid max-parts")
			return
		}
		limit = min(n, 1000)
	}
	result := ListPartsResult{Xmlns: s3Xmlns, Bucket: bucket, Key: key, UploadID: id, Owner: birakOwner(), Initiator: birakOwner(), StorageClass: "STANDARD", PartNumberMarker: marker, MaxParts: limit}
	for _, rec := range records {
		number, _ := strconv.Atoi(strings.TrimPrefix(rec.Key, qParts(id)))
		if number <= marker {
			continue
		}
		if len(result.Parts) == limit {
			result.IsTruncated = true
			break
		}
		result.Parts = append(result.Parts, PartInfo{PartNumber: number, LastModified: qTime(rec.Entry).Format(s3TimeFormat), ETag: qETag(rec.Entry), Size: rec.Entry.Ref.Size})
		result.NextPartNumberMarker = number
	}
	writeXML(w, 200, result)
}
func (g *Gateway) qListUploads(w http.ResponseWriter, r *http.Request, bucket string) {
	if r.URL.Query().Get("delimiter") != "" {
		writeS3Error(w, 501, "NotImplemented", "Upload listing delimiter is not supported")
		return
	}
	records, err := g.config.Quorum.View(r.Context(), qUploads(bucket))
	if err != nil {
		g.qError(w, err)
		return
	}
	q := r.URL.Query()
	limit := 1000
	if s := q.Get("max-uploads"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 {
			writeS3Error(w, 400, "InvalidArgument", "Invalid max-uploads")
			return
		}
		limit = min(n, 1000)
	}
	result := ListMultipartUploadsResult{Xmlns: s3Xmlns, Bucket: bucket, KeyMarker: q.Get("key-marker"), UploadIDMarker: q.Get("upload-id-marker"), Prefix: q.Get("prefix"), MaxUploads: limit}
	var uploads []UploadInfo
	for _, rec := range records {
		var up uploadRecord
		if json.Unmarshal([]byte(rec.Entry.Attributes.Value), &up) != nil {
			g.qError(w, errors.New("invalid upload record"))
			return
		}
		id := strings.TrimPrefix(rec.Key, qUploads(bucket))
		if !strings.HasPrefix(up.Key, q.Get("prefix")) {
			continue
		}
		uploads = append(uploads, UploadInfo{Key: up.Key, UploadID: id, Initiator: birakOwner(), Owner: birakOwner(), StorageClass: "STANDARD", Initiated: qTime(rec.Entry).Format(s3TimeFormat)})
	}
	sort.Slice(uploads, func(i, j int) bool {
		if uploads[i].Key == uploads[j].Key {
			return uploads[i].UploadID < uploads[j].UploadID
		}
		return uploads[i].Key < uploads[j].Key
	})
	for _, up := range uploads {
		if up.Key < result.KeyMarker || (up.Key == result.KeyMarker && (result.UploadIDMarker == "" || up.UploadID <= result.UploadIDMarker)) {
			continue
		}
		if len(result.Uploads) == limit {
			result.IsTruncated = true
			break
		}
		result.Uploads = append(result.Uploads, up)
		result.NextKeyMarker = up.Key
		result.NextUploadIDMarker = up.UploadID
	}
	writeXML(w, 200, result)
}

// Refresh succeeding is not a committed write. Exhausted CAS retries must
// preserve the conflict instead of acknowledging an uncommitted part.
func retryPartCommit(commit, refresh func() error) error {
	for attempt := 0; attempt < 16; attempt++ {
		if err := commit(); !errors.Is(err, quorum.ErrCondition) {
			return err
		}
		if err := refresh(); err != nil {
			return err
		}
	}
	return quorum.ErrCondition
}
