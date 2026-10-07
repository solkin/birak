package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/generation"
	"github.com/birak/birak/internal/multipart"
	"github.com/birak/birak/internal/quorum"
	"github.com/hashicorp/raft"
)

type soloPeers struct{}

func (soloPeers) Identity(context.Context, raft.ServerID) (quorum.Identity, error) {
	return quorum.Identity{}, errors.New("no peer")
}
func (soloPeers) Receive(context.Context, raft.ServerID, generation.Ref, io.Reader) error {
	return errors.New("no peer")
}
func (soloPeers) Open(context.Context, raft.ServerID, generation.Ref) (io.ReadCloser, error) {
	return nil, errors.New("no peer")
}
func quorumGateway(t *testing.T) *Gateway {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("strict NTFS durability not validated")
	}
	_, transport := raft.NewInmemTransport("solo")
	t.Cleanup(func() { transport.Close() })
	cfg := raft.DefaultConfig()
	cfg.HeartbeatTimeout = 100 * time.Millisecond
	cfg.ElectionTimeout = 100 * time.Millisecond
	cfg.LeaderLeaseTimeout = 50 * time.Millisecond
	cfg.CommitTimeout = time.Millisecond
	cfg.LogOutput = io.Discard
	n, err := quorum.Open(quorum.Options{Dir: filepath.Join(t.TempDir(), "node"), Identity: quorum.Identity{Cluster: "s3", Node: "solo", Format: quorum.Format}, Bootstrap: true, Transport: transport, Peers: soloPeers{}, RaftConfig: cfg, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for n.Ready(context.Background()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("no leader")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return New("", nil, Config{AccessKey: "access", SecretKey: "secret", MaxUploadBytes: 128, Quorum: n, QuorumLimits: multipart.Limits{MinPartBytes: 3, MaxPartBytes: 64, MaxParts: 3, MaxActiveUploads: 2, MaxConcurrentParts: 2}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
func qRequest(t *testing.T, g *Gateway, method, path, body string, headers map[string]string, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://s3.test"+path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	signV4Header(r, "access", "secret")
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w
}
func TestQuorumS3ObjectsListingsAndLimits(t *testing.T) {
	g := quorumGateway(t)
	qRequest(t, g, "PUT", "/bucket", "", nil, 200)
	qRequest(t, g, "GET", "/", "", nil, 200)
	qRequest(t, g, "PUT", "/bucket/A.apk", "APK", map[string]string{"Content-Type": "application/vnd.android.package-archive"}, 200)
	qRequest(t, g, "PUT", "/bucket/a.apk", "lower", nil, 200)
	qRequest(t, g, "PUT", "/bucket/dir/icon", "icon", nil, 200)
	head := qRequest(t, g, "HEAD", "/bucket/A.apk", "", nil, 200)
	if head.Header().Get("Content-Type") != "application/vnd.android.package-archive" {
		t.Fatal(head.Header())
	}
	qRequest(t, g, "GET", "/bucket/A.apk", "", map[string]string{"If-None-Match": head.Header().Get("ETag")}, 304)
	qRequest(t, g, "PUT", "/bucket/A.apk", "replaced", map[string]string{"If-Match": "\"wrong\""}, 412)
	qRequest(t, g, "PUT", "/bucket/copy.apk", "", map[string]string{"X-Amz-Copy-Source": "/bucket/A.apk"}, 200)
	if w := qRequest(t, g, "GET", "/bucket/copy.apk", "", nil, 200); w.Body.String() != "APK" {
		t.Fatal(w.Body.String())
	}
	w := qRequest(t, g, "GET", "/bucket?list-type=2&max-keys=1", "", nil, 200)
	var page ListBucketResultV2
	if err := xml.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Contents) != 1 || !page.IsTruncated || page.NextContinuationToken == "" {
		t.Fatal(page)
	}
	qRequest(t, g, "GET", "/bucket?delimiter=/", "", nil, 200)
	qRequest(t, g, "GET", "/bucket?list-type=2&continuation-token="+page.NextContinuationToken, "", nil, 200)
	qRequest(t, g, "DELETE", "/bucket", "", nil, 409)
	qRequest(t, g, "PUT", "/bucket/bad", "bad", map[string]string{"Content-MD5": "incorrect"}, 400)
	qRequest(t, g, "PUT", "/bucket/huge", strings.Repeat("x", 129), nil, 413)
	r := httptest.NewRequest("PUT", "http://s3.test/bucket/stream", strings.NewReader(strings.Repeat("x", 129)))
	r.ContentLength = -1
	signV4Header(r, "access", "secret")
	out := httptest.NewRecorder()
	g.Handler().ServeHTTP(out, r)
	if out.Code != 413 {
		t.Fatal("unbounded stream", out.Code)
	}
	qRequest(t, g, "GET", "/bucket/stream", "", nil, 404)
	for _, header := range []string{"X-Amz-Meta-Color", "X-Amz-Checksum-Crc32", "X-Amz-Object-Lock-Mode", "Content-Encoding"} {
		qRequest(t, g, "PUT", "/bucket/unsupported", "body", map[string]string{header: "unsupported"}, 501)
	}
	qRequest(t, g, "GET", "/bucket?encoding-type=url", "", nil, 501)
	qRequest(t, g, "GET", "/bucket/A.apk?versionId=123", "", nil, 501)
	qRequest(t, g, "DELETE", "/bucket?lifecycle", "", nil, 501)
	qRequest(t, g, "POST", "/bucket?delete", "<Delete><Object><Key>A.apk</Key><VersionId>old</VersionId></Object></Delete>", nil, 200)
	qRequest(t, g, "GET", "/bucket/A.apk", "", nil, 200)
	qRequest(t, g, "POST", "/bucket?delete", "<Delete><Object><Key>A.apk</Key></Object><Object><Key>copy.apk</Key></Object></Delete>", nil, 200)
	qRequest(t, g, "GET", "/bucket/A.apk", "", nil, 404)
	qRequest(t, g, "GET", "/bucket/a.apk", "", nil, 200)
	qRequest(t, g, "PUT", "/newbucket", "<CreateBucketConfiguration><LocationConstraint>other</LocationConstraint></CreateBucketConfiguration>", nil, 400)
}
func TestQuorumS3MultipartGuardsAndCleanup(t *testing.T) {
	g := quorumGateway(t)
	qRequest(t, g, "PUT", "/bucket", "", nil, 200)
	start := func(key string) string {
		w := qRequest(t, g, "POST", "/bucket/"+key+"?uploads", "", nil, 200)
		var u InitiateMultipartUploadResult
		if err := xml.Unmarshal(w.Body.Bytes(), &u); err != nil {
			t.Fatal(err)
		}
		return u.UploadID
	}
	id := start("file")
	other := start("other")
	qRequest(t, g, "POST", "/bucket/limit?uploads", "", nil, 409)
	qRequest(t, g, "GET", "/bucket?uploads", "", nil, 200)
	qRequest(t, g, "DELETE", "/bucket", "", nil, 409)
	qRequest(t, g, "DELETE", "/bucket/other?uploadId="+other, "", nil, 204)
	qRequest(t, g, "PUT", "/bucket/other?uploadId="+other+"&partNumber=1", "gone", nil, 404)
	partPath := "/bucket/file?uploadId=" + id
	qRequest(t, g, "PUT", partPath+"&partNumber=4", "x", nil, 400)
	qRequest(t, g, "PUT", partPath+"&partNumber=1", strings.Repeat("x", 65), nil, 413)
	one := qRequest(t, g, "PUT", partPath+"&partNumber=1", "abc", nil, 200).Header().Get("ETag")
	two := qRequest(t, g, "PUT", partPath+"&partNumber=2", "d", nil, 200).Header().Get("ETag")
	qRequest(t, g, "GET", partPath+"&max-parts=1", "", nil, 200)
	qRequest(t, g, "POST", partPath, "bad XML", nil, 400)
	complete := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>" + one + "</ETag></Part><Part><PartNumber>2</PartNumber><ETag>" + two + "</ETag></Part></CompleteMultipartUpload>"
	qRequest(t, g, "POST", partPath, strings.Replace(complete, one, "wrong", 1), nil, 400)
	qRequest(t, g, "POST", partPath, complete, nil, 200)
	if w := qRequest(t, g, "GET", "/bucket/file", "", nil, 200); !bytes.Equal(w.Body.Bytes(), []byte("abcd")) {
		t.Fatal(w.Body.String())
	}
	qRequest(t, g, "POST", partPath, strings.Replace(complete, one, "different", 1), nil, 409)
	qRequest(t, g, "DELETE", "/bucket/file", "", nil, 204)
	qRequest(t, g, "POST", partPath, complete, nil, 200)
	qRequest(t, g, "GET", "/bucket/file", "", nil, 404)
	qRequest(t, g, "DELETE", "/bucket", "", nil, 204)
	// Completion receipt belongs to the old bucket generation.
	qRequest(t, g, "PUT", "/bucket", "", nil, 200)
	qRequest(t, g, "POST", partPath, complete, nil, 404)
}
