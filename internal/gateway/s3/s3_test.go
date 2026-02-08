package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testGateway creates a Gateway with a temporary syncDir and returns it with cleanup.
func testGateway(t *testing.T, cfg Config) (*Gateway, string) {
	t.Helper()
	syncDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ignorePatterns := []string{".DS_Store", "Thumbs.db", "desktop.ini", ".birak-tmp-*"}
	g := New(syncDir, ignorePatterns, cfg, logger)
	return g, syncDir
}

// serveRequest is a helper to perform a request directly against the gateway handler.
// If headers contains "Host", it is set as req.Host (Go treats Host specially).
func serveRequest(g *Gateway, method, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	g.route(w, req)
	return w
}

// authHeaders returns headers for authenticated requests (simplified V2 auth).
func authHeaders(accessKey string) map[string]string {
	return map[string]string{
		"Authorization": fmt.Sprintf("AWS %s:signature", accessKey),
	}
}

// sigV4Headers returns headers for authenticated requests using V4 format.
func sigV4Headers(accessKey string) map[string]string {
	return map[string]string{
		"Authorization": fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/20260206/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc123", accessKey),
	}
}

// noAuth returns nil headers for requests without auth.
func noAuth() map[string]string {
	return nil
}

// --- Authentication Tests ---

func TestAuth_NoAuthConfigured(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	// Create a bucket to list.
	os.Mkdir(filepath.Join(syncDir, "test"), 0o755)

	// No auth should succeed.
	w := serveRequest(g, http.MethodGet, "/", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuth_ValidV2(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	w := serveRequest(g, http.MethodGet, "/", nil, authHeaders("admin"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAuth_ValidV4(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	w := serveRequest(g, http.MethodGet, "/", nil, sigV4Headers("admin"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAuth_InvalidAccessKey(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	w := serveRequest(g, http.MethodGet, "/", nil, authHeaders("wrong"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestAuth_MissingAuthHeader(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	w := serveRequest(g, http.MethodGet, "/", nil, noAuth())
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestAuth_PresignedURL_Valid(t *testing.T) {
	g, syncDir := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	// Presigned URL with valid access key in X-Amz-Credential query param.
	url := "/mybucket/file.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=admin%2F20260206%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260206T163218Z&X-Amz-Expires=2999&X-Amz-SignedHeaders=content-type%3Bhost&X-Amz-Signature=abcdef1234567890"
	w := serveRequest(g, http.MethodPut, url, strings.NewReader("presigned data"), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify file was created.
	data, err := os.ReadFile(filepath.Join(syncDir, "mybucket", "file.txt"))
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if string(data) != "presigned data" {
		t.Fatalf("expected 'presigned data', got %q", string(data))
	}
}

func TestAuth_PresignedURL_InvalidKey(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	url := "/?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=wrong%2F20260206%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=abc"
	w := serveRequest(g, http.MethodGet, url, nil, noAuth())
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestAuth_PresignedURL_MissingCredential(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	url := "/?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=abc"
	w := serveRequest(g, http.MethodGet, url, nil, noAuth())
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- Bucket Tests ---

func TestListBuckets_Empty(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodGet, "/", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result ListAllMyBucketsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Buckets.Bucket) != 0 {
		t.Fatalf("expected 0 buckets, got %d", len(result.Buckets.Bucket))
	}
}

func TestCreateBucket(t *testing.T) {
	g, syncDir := testGateway(t, Config{})

	w := serveRequest(g, http.MethodPut, "/mybucket", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify directory exists.
	info, err := os.Stat(filepath.Join(syncDir, "mybucket"))
	if err != nil {
		t.Fatalf("bucket directory should exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("bucket should be a directory")
	}
}

func TestCreateBucket_AlreadyExists(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodPut, "/mybucket", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestCreateBucket_InvalidName(t *testing.T) {
	g, _ := testGateway(t, Config{})

	for _, name := range []string{"..", ".", ""} {
		w := serveRequest(g, http.MethodPut, "/"+name, nil, noAuth())
		// Empty name hits ListBuckets route; .. and . should be rejected.
		if name != "" && w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for bucket name %q, got %d", name, w.Code)
		}
	}
}

func TestHeadBucket_Exists(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodHead, "/mybucket", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHeadBucket_NotExists(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodHead, "/nonexistent", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteBucket_Empty(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodDelete, "/mybucket", nil, noAuth())
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(syncDir, "mybucket")); !os.IsNotExist(err) {
		t.Fatal("bucket directory should be removed")
	}
}

func TestDeleteBucket_NotEmpty(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "file.txt"), []byte("data"), 0o644)

	w := serveRequest(g, http.MethodDelete, "/mybucket", nil, noAuth())
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestDeleteBucket_WithOnlyIgnoredFiles(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, ".DS_Store"), []byte("apple"), 0o644)

	w := serveRequest(g, http.MethodDelete, "/mybucket", nil, noAuth())
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteBucket_NotExists(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodDelete, "/nonexistent", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListBuckets_IgnoresFiles(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	// Create a directory and a file at root level.
	os.Mkdir(filepath.Join(syncDir, "bucket1"), 0o755)
	os.WriteFile(filepath.Join(syncDir, "rootfile.txt"), []byte("hi"), 0o644)

	w := serveRequest(g, http.MethodGet, "/", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result ListAllMyBucketsResult
	xml.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Buckets.Bucket) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(result.Buckets.Bucket))
	}
	if result.Buckets.Bucket[0].Name != "bucket1" {
		t.Fatalf("expected bucket1, got %s", result.Buckets.Bucket[0].Name)
	}
}

func TestListBuckets_IgnoresIgnoredDirs(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "bucket1"), 0o755)
	os.Mkdir(filepath.Join(syncDir, ".DS_Store"), 0o755) // shouldn't appear

	w := serveRequest(g, http.MethodGet, "/", nil, noAuth())
	var result ListAllMyBucketsResult
	xml.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Buckets.Bucket) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(result.Buckets.Bucket))
	}
}

// --- Object Tests ---

func TestPutAndGetObject(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	content := "hello world"
	w := serveRequest(g, http.MethodPut, "/mybucket/test.txt", strings.NewReader(content), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify ETag is present.
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("put: expected ETag header")
	}

	// Get the object.
	w = serveRequest(g, http.MethodGet, "/mybucket/test.txt", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != content {
		t.Fatalf("get: expected %q, got %q", content, w.Body.String())
	}
}

func TestPutObject_NestedKey(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	content := "nested file"
	w := serveRequest(g, http.MethodPut, "/mybucket/path/to/file.txt", strings.NewReader(content), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify file exists on disk.
	data, err := os.ReadFile(filepath.Join(syncDir, "mybucket", "path", "to", "file.txt"))
	if err != nil {
		t.Fatalf("file should exist on disk: %v", err)
	}
	if string(data) != content {
		t.Fatalf("expected %q, got %q", content, string(data))
	}
}

func TestPutObject_NoBucket(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodPut, "/nonexistent/file.txt", strings.NewReader("data"), noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetObject_NotExists(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket/nonexistent.txt", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHeadObject(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	content := "head test"
	os.WriteFile(filepath.Join(bp, "test.txt"), []byte(content), 0o644)

	w := serveRequest(g, http.MethodHead, "/mybucket/test.txt", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("Content-Length") != fmt.Sprintf("%d", len(content)) {
		t.Fatalf("expected Content-Length %d, got %s", len(content), w.Header().Get("Content-Length"))
	}
	if w.Header().Get("ETag") == "" {
		t.Fatal("expected ETag header")
	}
	if w.Header().Get("Last-Modified") == "" {
		t.Fatal("expected Last-Modified header")
	}
}

func TestHeadObject_NotExists(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodHead, "/mybucket/nope.txt", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteObject(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "delete-me.txt"), []byte("bye"), 0o644)

	w := serveRequest(g, http.MethodDelete, "/mybucket/delete-me.txt", nil, noAuth())
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	if _, err := os.Stat(filepath.Join(bp, "delete-me.txt")); !os.IsNotExist(err) {
		t.Fatal("file should have been deleted")
	}
}

func TestDeleteObject_NotExists(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	// S3 returns 204 for deleting non-existent objects.
	w := serveRequest(g, http.MethodDelete, "/mybucket/nonexistent.txt", nil, noAuth())
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestDeleteObject_CleansEmptyParents(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(bp, "a", "b", "file.txt"), []byte("data"), 0o644)

	w := serveRequest(g, http.MethodDelete, "/mybucket/a/b/file.txt", nil, noAuth())
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	// a/b/ and a/ should be cleaned up.
	if _, err := os.Stat(filepath.Join(bp, "a")); !os.IsNotExist(err) {
		t.Fatal("a/ should have been cleaned up")
	}
}

func TestPutObject_IgnoredFile(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodPut, "/mybucket/.DS_Store", strings.NewReader("apple"), noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for ignored file, got %d", w.Code)
	}
}

func TestGetObject_IgnoredFile(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, ".DS_Store"), []byte("apple"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket/.DS_Store", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for ignored file, got %d", w.Code)
	}
}

// --- ListObjects V1 Tests (default, no list-type param) ---

func TestListObjectsV1_Basic(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(bp, "b.txt"), []byte("bbb"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ListBucketResultV1
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal V1: %v", err)
	}
	if len(result.Contents) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(result.Contents))
	}
	if result.Contents[0].Key != "a.txt" {
		t.Fatalf("expected a.txt, got %s", result.Contents[0].Key)
	}
	if result.Contents[1].Key != "b.txt" {
		t.Fatalf("expected b.txt, got %s", result.Contents[1].Key)
	}
	// V1 must have Marker field.
	if result.Marker != "" {
		t.Fatalf("expected empty Marker, got %q", result.Marker)
	}
}

func TestListObjectsV1_HasMarker(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(bp, "b.txt"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(bp, "c.txt"), []byte("c"), 0o644)

	// Use marker to skip past "a.txt".
	w := serveRequest(g, http.MethodGet, "/mybucket?marker=a.txt", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)

	// Should return b.txt and c.txt only.
	if len(result.Contents) != 2 {
		t.Fatalf("expected 2 objects after marker, got %d", len(result.Contents))
	}
	if result.Contents[0].Key != "b.txt" {
		t.Fatalf("expected b.txt, got %s", result.Contents[0].Key)
	}
}

func TestListObjectsV1_WithPrefix(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "photos"), 0o755)
	os.MkdirAll(filepath.Join(bp, "docs"), 0o755)
	os.WriteFile(filepath.Join(bp, "photos", "img1.jpg"), []byte("img"), 0o644)
	os.WriteFile(filepath.Join(bp, "photos", "img2.jpg"), []byte("img"), 0o644)
	os.WriteFile(filepath.Join(bp, "docs", "readme.txt"), []byte("doc"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket?prefix=photos/", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Contents) != 2 {
		t.Fatalf("expected 2 objects with prefix photos/, got %d", len(result.Contents))
	}
	for _, obj := range result.Contents {
		if !strings.HasPrefix(obj.Key, "photos/") {
			t.Fatalf("unexpected key %s", obj.Key)
		}
	}
}

func TestListObjectsV1_WithDelimiter(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "photos", "2024"), 0o755)
	os.MkdirAll(filepath.Join(bp, "docs"), 0o755)
	os.WriteFile(filepath.Join(bp, "root.txt"), []byte("root"), 0o644)
	os.WriteFile(filepath.Join(bp, "photos", "img.jpg"), []byte("img"), 0o644)
	os.WriteFile(filepath.Join(bp, "photos", "2024", "jan.jpg"), []byte("jan"), 0o644)
	os.WriteFile(filepath.Join(bp, "docs", "readme.txt"), []byte("doc"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket?delimiter=/", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)

	if len(result.Contents) != 1 || result.Contents[0].Key != "root.txt" {
		t.Fatalf("expected [root.txt] in contents, got %v", result.Contents)
	}
	if len(result.CommonPrefixes) != 2 {
		t.Fatalf("expected 2 common prefixes, got %d: %v", len(result.CommonPrefixes), result.CommonPrefixes)
	}
	cpMap := make(map[string]bool)
	for _, cp := range result.CommonPrefixes {
		cpMap[cp.Prefix] = true
	}
	if !cpMap["photos/"] || !cpMap["docs/"] {
		t.Fatalf("expected photos/ and docs/ in common prefixes, got %v", result.CommonPrefixes)
	}
}

func TestListObjectsV1_WithPrefixAndDelimiter(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "photos", "2024"), 0o755)
	os.MkdirAll(filepath.Join(bp, "photos", "2025"), 0o755)
	os.WriteFile(filepath.Join(bp, "photos", "avatar.jpg"), []byte("avatar"), 0o644)
	os.WriteFile(filepath.Join(bp, "photos", "2024", "jan.jpg"), []byte("jan"), 0o644)
	os.WriteFile(filepath.Join(bp, "photos", "2025", "feb.jpg"), []byte("feb"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket?prefix=photos/&delimiter=/", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)

	if len(result.Contents) != 1 || result.Contents[0].Key != "photos/avatar.jpg" {
		t.Fatalf("expected [photos/avatar.jpg], got %v", result.Contents)
	}
	if len(result.CommonPrefixes) != 2 {
		t.Fatalf("expected 2 common prefixes, got %d", len(result.CommonPrefixes))
	}
}

func TestListObjectsV1_IgnoredFilesExcluded(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "real.txt"), []byte("data"), 0o644)
	os.WriteFile(filepath.Join(bp, ".DS_Store"), []byte("apple"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Contents) != 1 {
		t.Fatalf("expected 1 object (ignored .DS_Store), got %d", len(result.Contents))
	}
	if result.Contents[0].Key != "real.txt" {
		t.Fatalf("expected real.txt, got %s", result.Contents[0].Key)
	}
}

func TestListObjectsV1_EmptyBucket(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Contents) != 0 {
		t.Fatalf("expected 0 objects, got %d", len(result.Contents))
	}
	if result.Name != "mybucket" {
		t.Fatalf("expected bucket name mybucket, got %s", result.Name)
	}
}

func TestListObjects_NoBucket(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodGet, "/nonexistent", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- ListObjects V2 Tests (list-type=2) ---

func TestListObjectsV2_Basic(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(bp, "b.txt"), []byte("bbb"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket?list-type=2", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ListBucketResultV2
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal V2: %v", err)
	}
	if result.KeyCount != 2 {
		t.Fatalf("expected KeyCount 2, got %d", result.KeyCount)
	}
	if len(result.Contents) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(result.Contents))
	}
	if result.Contents[0].Key != "a.txt" {
		t.Fatalf("expected a.txt, got %s", result.Contents[0].Key)
	}
}

func TestListObjectsV2_WithStartAfter(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(bp, "b.txt"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(bp, "c.txt"), []byte("c"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket?list-type=2&start-after=a.txt", nil, noAuth())
	var result ListBucketResultV2
	xml.Unmarshal(w.Body.Bytes(), &result)

	if len(result.Contents) != 2 {
		t.Fatalf("expected 2 objects after start-after, got %d", len(result.Contents))
	}
	if result.Contents[0].Key != "b.txt" {
		t.Fatalf("expected b.txt, got %s", result.Contents[0].Key)
	}
	if result.StartAfter != "a.txt" {
		t.Fatalf("expected StartAfter=a.txt, got %s", result.StartAfter)
	}
}

func TestListObjectsV2_WithDelimiter(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "photos"), 0o755)
	os.WriteFile(filepath.Join(bp, "root.txt"), []byte("root"), 0o644)
	os.WriteFile(filepath.Join(bp, "photos", "img.jpg"), []byte("img"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket?list-type=2&delimiter=/", nil, noAuth())
	var result ListBucketResultV2
	xml.Unmarshal(w.Body.Bytes(), &result)

	if result.KeyCount != 1 {
		t.Fatalf("expected KeyCount 1, got %d", result.KeyCount)
	}
	if len(result.CommonPrefixes) != 1 || result.CommonPrefixes[0].Prefix != "photos/" {
		t.Fatalf("expected [photos/] in common prefixes, got %v", result.CommonPrefixes)
	}
}

func TestListObjectsV2_EmptyBucket(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket?list-type=2", nil, noAuth())
	var result ListBucketResultV2
	xml.Unmarshal(w.Body.Bytes(), &result)
	if result.KeyCount != 0 {
		t.Fatalf("expected KeyCount 0, got %d", result.KeyCount)
	}
	if result.Name != "mybucket" {
		t.Fatalf("expected bucket name mybucket, got %s", result.Name)
	}
}

// --- Sub-resource Tests ---

func TestGetBucketLocation(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket?location", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result LocationConstraint
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal LocationConstraint: %v", err)
	}
	// Value should be empty (default region).
	if result.Value != "" {
		t.Fatalf("expected empty location, got %q", result.Value)
	}
}

func TestGetBucketLocation_NotExists(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodGet, "/nonexistent?location", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetBucketVersioning(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket?versioning", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result VersioningConfiguration
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal VersioningConfiguration: %v", err)
	}
}

func TestGetBucketVersioning_NotExists(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodGet, "/nonexistent?versioning", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetBucketACL(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket?acl", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "AccessControlPolicy") {
		t.Fatal("expected AccessControlPolicy in response")
	}
	if !strings.Contains(body, "FULL_CONTROL") {
		t.Fatal("expected FULL_CONTROL in response")
	}
}

func TestGetBucketACL_NotExists(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodGet, "/nonexistent?acl", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetBucketTagging(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket?tagging", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (NoSuchTagSet), got %d", w.Code)
	}
}

func TestGetBucketPolicy(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket?policy", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (NoSuchBucketPolicy), got %d", w.Code)
	}
}

// --- Sub-resource does not interfere with normal listing ---

func TestSubResourceDoesNotAffectListObjects(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "file.txt"), []byte("data"), 0o644)

	// Normal list should still work.
	w := serveRequest(g, http.MethodGet, "/mybucket?prefix=&delimiter=/", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Contents) != 1 {
		t.Fatalf("expected 1 object, got %d", len(result.Contents))
	}
}

// --- Path Traversal Tests ---

func TestPathTraversal_BucketName(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodGet, "/..", nil, noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for .. bucket, got %d", w.Code)
	}
}

func TestPathTraversal_Key(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket/../../etc/passwd", nil, noAuth())
	// Should be rejected as invalid.
	if w.Code == http.StatusOK {
		t.Fatal("path traversal should be rejected")
	}
}

// --- Object Overwrite Test ---

func TestPutObject_Overwrite(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	// Write v1.
	serveRequest(g, http.MethodPut, "/mybucket/file.txt", strings.NewReader("version 1"), noAuth())

	// Write v2.
	w := serveRequest(g, http.MethodPut, "/mybucket/file.txt", strings.NewReader("version 2"), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("overwrite: expected 200, got %d", w.Code)
	}

	// Get should return v2.
	w = serveRequest(g, http.MethodGet, "/mybucket/file.txt", nil, noAuth())
	if w.Body.String() != "version 2" {
		t.Fatalf("expected 'version 2', got %q", w.Body.String())
	}
}

// --- Large Object Test ---

func TestPutObject_LargeFile(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	// Create a 1MB file.
	data := bytes.Repeat([]byte("x"), 1024*1024)
	w := serveRequest(g, http.MethodPut, "/mybucket/large.bin", bytes.NewReader(data), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Read it back.
	w = serveRequest(g, http.MethodGet, "/mybucket/large.bin", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}
	if w.Body.Len() != 1024*1024 {
		t.Fatalf("expected 1MB, got %d bytes", w.Body.Len())
	}
}

// --- Method Not Allowed ---

func TestMethodNotAllowed_Root(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodPost, "/", nil, noAuth())
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestMethodNotAllowed_Object(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodPost, "/mybucket/file.txt", nil, noAuth())
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

// --- Empty Object Test ---

func TestPutObject_EmptyFile(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodPut, "/mybucket/empty.txt", strings.NewReader(""), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	info, err := os.Stat(filepath.Join(syncDir, "mybucket", "empty.txt"))
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected empty file, got size %d", info.Size())
	}
}

// --- Virtual-Hosted-Style Tests ---

// vhostHeaders returns headers with Host set to bucket.hostname:port for virtual-hosted-style.
func vhostHeaders(bucket, baseHost string) map[string]string {
	return map[string]string{
		"Host": fmt.Sprintf("%s.%s", bucket, baseHost),
	}
}

func TestVirtualHosted_ListObjects(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(bp, "b.txt"), []byte("bbb"), 0o644)

	// Virtual-hosted-style: Host=mybucket.localhost, path=/
	w := serveRequest(g, http.MethodGet, "/", nil, vhostHeaders("mybucket", "localhost"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ListBucketResultV1
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, w.Body.String())
	}
	if result.Name != "mybucket" {
		t.Fatalf("expected bucket name mybucket, got %s", result.Name)
	}
	if len(result.Contents) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(result.Contents))
	}
}

func TestVirtualHosted_ListObjectsWithDelimiter(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "photos"), 0o755)
	os.WriteFile(filepath.Join(bp, "root.txt"), []byte("root"), 0o644)
	os.WriteFile(filepath.Join(bp, "photos", "img.jpg"), []byte("img"), 0o644)

	// Simulates Cyberduck: GET /?encoding-type=url&max-keys=1000&prefix=&delimiter=%2F
	w := serveRequest(g, http.MethodGet, "/?encoding-type=url&max-keys=1000&prefix=&delimiter=%2F",
		nil, vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ListBucketResultV1
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, w.Body.String())
	}
	if result.Name != "mybucket" {
		t.Fatalf("expected bucket name mybucket, got %s", result.Name)
	}
	if len(result.Contents) != 1 || result.Contents[0].Key != "root.txt" {
		t.Fatalf("expected [root.txt], got %v", result.Contents)
	}
	if len(result.CommonPrefixes) != 1 || result.CommonPrefixes[0].Prefix != "photos/" {
		t.Fatalf("expected [photos/] common prefix, got %v", result.CommonPrefixes)
	}
}

func TestVirtualHosted_GetBucketVersioning(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/?versioning", nil, vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result VersioningConfiguration
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

func TestVirtualHosted_GetBucketLocation(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/?location=", nil, vhostHeaders("mybucket", "localhost"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result LocationConstraint
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

func TestVirtualHosted_HeadBucket(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodHead, "/", nil, vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestVirtualHosted_PutAndGetObject(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	// PUT via virtual-hosted-style.
	content := "vhost content"
	w := serveRequest(g, http.MethodPut, "/test.txt", strings.NewReader(content),
		vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// GET via virtual-hosted-style.
	w = serveRequest(g, http.MethodGet, "/test.txt", nil,
		vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}
	if w.Body.String() != content {
		t.Fatalf("expected %q, got %q", content, w.Body.String())
	}
}

func TestVirtualHosted_DeleteObject(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "del.txt"), []byte("bye"), 0o644)

	w := serveRequest(g, http.MethodDelete, "/del.txt", nil,
		vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	if _, err := os.Stat(filepath.Join(bp, "del.txt")); !os.IsNotExist(err) {
		t.Fatal("file should be deleted")
	}
}

func TestVirtualHosted_HeadObject(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "test.txt"), []byte("data"), 0o644)

	w := serveRequest(g, http.MethodHead, "/test.txt", nil,
		vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("ETag") == "" {
		t.Fatal("expected ETag header")
	}
}

func TestVirtualHosted_NonExistentBucket(t *testing.T) {
	g, _ := testGateway(t, Config{})

	// Bucket doesn't exist — extractBucketFromHost returns "" → falls through to path-style.
	// Path "/" → ListBuckets (no error).
	w := serveRequest(g, http.MethodGet, "/", nil, vhostHeaders("nonexistent", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (list buckets fallback), got %d", w.Code)
	}
}

func TestVirtualHosted_GetBucketACL(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/?acl", nil, vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AccessControlPolicy") {
		t.Fatal("expected AccessControlPolicy in response")
	}
}

func TestVirtualHosted_ListObjectsV2(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "a.txt"), []byte("a"), 0o644)

	w := serveRequest(g, http.MethodGet, "/?list-type=2", nil, vhostHeaders("mybucket", "localhost:9200"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ListBucketResultV2
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal V2: %v", err)
	}
	if result.KeyCount != 1 {
		t.Fatalf("expected KeyCount 1, got %d", result.KeyCount)
	}
}

// --- Edge Case: ListObjects max-keys truncation ---

func TestListObjectsV1_MaxKeysTruncation(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(bp, fmt.Sprintf("file%02d.txt", i)), []byte("x"), 0o644)
	}

	w := serveRequest(g, http.MethodGet, "/mybucket?max-keys=3", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)

	if result.MaxKeys != 3 {
		t.Fatalf("expected MaxKeys=3, got %d", result.MaxKeys)
	}
	if !result.IsTruncated {
		t.Fatal("expected IsTruncated=true")
	}
	if len(result.Contents) != 3 {
		t.Fatalf("expected 3 objects, got %d", len(result.Contents))
	}
	if result.NextMarker == "" {
		t.Fatal("expected non-empty NextMarker when truncated")
	}
}

func TestListObjectsV2_MaxKeysTruncation(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(bp, fmt.Sprintf("file%02d.txt", i)), []byte("x"), 0o644)
	}

	w := serveRequest(g, http.MethodGet, "/mybucket?list-type=2&max-keys=2", nil, noAuth())
	var result ListBucketResultV2
	xml.Unmarshal(w.Body.Bytes(), &result)

	if result.MaxKeys != 2 {
		t.Fatalf("expected MaxKeys=2, got %d", result.MaxKeys)
	}
	if !result.IsTruncated {
		t.Fatal("expected IsTruncated=true")
	}
	if result.KeyCount != 2 {
		t.Fatalf("expected KeyCount=2, got %d", result.KeyCount)
	}
	if result.NextContinuationToken == "" {
		t.Fatal("expected non-empty NextContinuationToken when truncated")
	}
}

// --- Edge Case: ListObjects V2 ContinuationToken ---

func TestListObjectsV2_ContinuationToken(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(bp, "b.txt"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(bp, "c.txt"), []byte("c"), 0o644)

	// Use continuation-token to skip past "a.txt".
	w := serveRequest(g, http.MethodGet, "/mybucket?list-type=2&continuation-token=a.txt", nil, noAuth())
	var result ListBucketResultV2
	xml.Unmarshal(w.Body.Bytes(), &result)

	if len(result.Contents) != 2 {
		t.Fatalf("expected 2 objects after continuation-token, got %d", len(result.Contents))
	}
	if result.Contents[0].Key != "b.txt" {
		t.Fatalf("expected b.txt, got %s", result.Contents[0].Key)
	}
	if result.ContinuationToken != "a.txt" {
		t.Fatalf("expected ContinuationToken=a.txt, got %s", result.ContinuationToken)
	}
}

// --- Edge Case: Unicode / Non-ASCII file names ---

func TestPutGetObject_UnicodeKey(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	content := "PDF content"
	// Russian characters in key (like М-4_АО.pdf from real usage).
	key := "М-4_АО.pdf"
	w := serveRequest(g, http.MethodPut, "/mybucket/"+key, strings.NewReader(content), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("put unicode: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w = serveRequest(g, http.MethodGet, "/mybucket/"+key, nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("get unicode: expected 200, got %d", w.Code)
	}
	if w.Body.String() != content {
		t.Fatalf("expected %q, got %q", content, w.Body.String())
	}
}

func TestListObjects_UnicodeKeys(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "документ.txt"), []byte("doc"), 0o644)
	os.WriteFile(filepath.Join(bp, "фото.jpg"), []byte("img"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)

	if len(result.Contents) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(result.Contents))
	}
}

// --- Edge Case: Presigned URL + Virtual-Hosted-Style ---

func TestVirtualHosted_PresignedURL(t *testing.T) {
	g, syncDir := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)

	// PUT via presigned URL + virtual-hosted-style (Commander One pattern).
	url := "/upload.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=admin%2F20260206%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260206T163218Z&X-Amz-Expires=2999&X-Amz-SignedHeaders=content-type%3Bhost&X-Amz-Signature=abc123"
	headers := map[string]string{
		"Host": "mybucket.localhost:9200",
	}
	w := serveRequest(g, http.MethodPut, url, strings.NewReader("presigned vhost"), headers)
	if w.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify file on disk.
	data, err := os.ReadFile(filepath.Join(bp, "upload.txt"))
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if string(data) != "presigned vhost" {
		t.Fatalf("expected 'presigned vhost', got %q", string(data))
	}
}

// --- Edge Case: Special characters in keys ---

func TestPutGetObject_KeyWithSpaces(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	// Spaces must be URL-encoded in the request URL.
	w := serveRequest(g, http.MethodPut, "/mybucket/my%20file.txt", strings.NewReader("data"), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify file exists on disk with decoded name.
	data, err := os.ReadFile(filepath.Join(syncDir, "mybucket", "my file.txt"))
	if err != nil {
		t.Fatalf("file should exist on disk: %v", err)
	}
	if string(data) != "data" {
		t.Fatalf("expected 'data', got %q", string(data))
	}

	w = serveRequest(g, http.MethodGet, "/mybucket/my%20file.txt", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}
}

func TestPutGetObject_KeyWithPlusSign(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodPut, "/mybucket/a+b.txt", strings.NewReader("plus"), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d", w.Code)
	}

	w = serveRequest(g, http.MethodGet, "/mybucket/a+b.txt", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}
	if w.Body.String() != "plus" {
		t.Fatalf("expected 'plus', got %q", w.Body.String())
	}
}

// --- Edge Case: ETag consistency ---

func TestETagConsistency_PutHeadGet(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	content := "etag test content"
	wPut := serveRequest(g, http.MethodPut, "/mybucket/etag.txt", strings.NewReader(content), noAuth())
	if wPut.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d", wPut.Code)
	}
	putETag := wPut.Header().Get("ETag")
	if putETag == "" {
		t.Fatal("PUT should return ETag")
	}

	wHead := serveRequest(g, http.MethodHead, "/mybucket/etag.txt", nil, noAuth())
	headETag := wHead.Header().Get("ETag")
	if headETag != putETag {
		t.Fatalf("ETag mismatch: PUT=%q HEAD=%q", putETag, headETag)
	}

	wGet := serveRequest(g, http.MethodGet, "/mybucket/etag.txt", nil, noAuth())
	getETag := wGet.Header().Get("ETag")
	if getETag != putETag {
		t.Fatalf("ETag mismatch: PUT=%q GET=%q", putETag, getETag)
	}
}

// --- Edge Case: Deeply nested directories ---

func TestListObjects_DeeplyNested(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "a", "b", "c", "d"), 0o755)
	os.WriteFile(filepath.Join(bp, "a", "b", "c", "d", "deep.txt"), []byte("deep"), 0o644)
	os.WriteFile(filepath.Join(bp, "a", "top.txt"), []byte("top"), 0o644)

	// List all objects (no delimiter).
	w := serveRequest(g, http.MethodGet, "/mybucket", nil, noAuth())
	var r1 ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &r1)

	if len(r1.Contents) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(r1.Contents))
	}
	keys := map[string]bool{}
	for _, obj := range r1.Contents {
		keys[obj.Key] = true
	}
	if !keys["a/b/c/d/deep.txt"] || !keys["a/top.txt"] {
		t.Fatalf("expected a/b/c/d/deep.txt and a/top.txt, got %v", keys)
	}

	// List with delimiter — should show only a/ as common prefix.
	w = serveRequest(g, http.MethodGet, "/mybucket?delimiter=/", nil, noAuth())
	var r2 ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &r2)
	if len(r2.Contents) != 0 {
		t.Fatalf("expected 0 objects at root with delimiter, got %d", len(r2.Contents))
	}
	if len(r2.CommonPrefixes) != 1 || r2.CommonPrefixes[0].Prefix != "a/" {
		t.Fatalf("expected [a/], got %v", r2.CommonPrefixes)
	}

	// Drill into a/ — should show a/top.txt and a/b/ prefix.
	w = serveRequest(g, http.MethodGet, "/mybucket?prefix=a/&delimiter=/", nil, noAuth())
	var r3 ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &r3)
	if len(r3.Contents) != 1 || r3.Contents[0].Key != "a/top.txt" {
		t.Fatalf("expected [a/top.txt], got %v", r3.Contents)
	}
	if len(r3.CommonPrefixes) != 1 || r3.CommonPrefixes[0].Prefix != "a/b/" {
		t.Fatalf("expected [a/b/], got %v", r3.CommonPrefixes)
	}
}

// --- Edge Case: Ignored files in subdirectories during listing ---

func TestListObjects_IgnoredFilesInSubdirs(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "docs"), 0o755)
	os.WriteFile(filepath.Join(bp, "docs", "readme.txt"), []byte("doc"), 0o644)
	os.WriteFile(filepath.Join(bp, "docs", ".DS_Store"), []byte("apple"), 0o644)
	os.WriteFile(filepath.Join(bp, "docs", "Thumbs.db"), []byte("win"), 0o644)

	w := serveRequest(g, http.MethodGet, "/mybucket?prefix=docs/", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)

	if len(result.Contents) != 1 {
		t.Fatalf("expected 1 object (ignored .DS_Store and Thumbs.db), got %d", len(result.Contents))
	}
	if result.Contents[0].Key != "docs/readme.txt" {
		t.Fatalf("expected docs/readme.txt, got %s", result.Contents[0].Key)
	}
}

// --- Edge Case: Concurrent PUT operations ---

func TestPutObject_Concurrent(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	const goroutines = 10
	done := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			content := fmt.Sprintf("content-%d", idx)
			w := serveRequest(g, http.MethodPut, "/mybucket/concurrent.txt", strings.NewReader(content), noAuth())
			if w.Code != http.StatusOK {
				done <- fmt.Errorf("goroutine %d: expected 200, got %d", idx, w.Code)
				return
			}
			done <- nil
		}(i)
	}

	for i := 0; i < goroutines; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	// File should exist and be readable.
	data, err := os.ReadFile(filepath.Join(syncDir, "mybucket", "concurrent.txt"))
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if !strings.HasPrefix(string(data), "content-") {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

// --- Edge Case: Malformed auth headers ---

func TestAuth_MalformedV4Header(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	// V4 prefix but no Credential= part.
	w := serveRequest(g, http.MethodGet, "/", nil, map[string]string{
		"Authorization": "AWS4-HMAC-SHA256 garbage",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestAuth_MalformedV2Header(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	// V2 prefix but no colon separator.
	w := serveRequest(g, http.MethodGet, "/", nil, map[string]string{
		"Authorization": "AWS nocolon",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestAuth_UnknownScheme(t *testing.T) {
	g, _ := testGateway(t, Config{AccessKey: "admin", SecretKey: "secret"})

	w := serveRequest(g, http.MethodGet, "/", nil, map[string]string{
		"Authorization": "Bearer some-token",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// --- Edge Case: GET/HEAD on a key that is actually a directory ---

func TestGetObject_DirectoryAsKey(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "subdir"), 0o755)
	os.WriteFile(filepath.Join(bp, "subdir", "file.txt"), []byte("data"), 0o644)

	// GET /mybucket/subdir — subdir is a directory, not an object.
	w := serveRequest(g, http.MethodGet, "/mybucket/subdir", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for directory key, got %d", w.Code)
	}
}

func TestHeadObject_DirectoryAsKey(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "subdir"), 0o755)

	w := serveRequest(g, http.MethodHead, "/mybucket/subdir", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for directory key, got %d", w.Code)
	}
}

// --- Edge Case: Bucket names with special chars ---

func TestBucket_WithHyphenAndDot(t *testing.T) {
	g, syncDir := testGateway(t, Config{})

	// Create bucket with hyphen.
	w := serveRequest(g, http.MethodPut, "/my-bucket", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("create my-bucket: expected 200, got %d", w.Code)
	}

	// Create bucket with dot.
	w = serveRequest(g, http.MethodPut, "/my.bucket", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("create my.bucket: expected 200, got %d", w.Code)
	}

	// List should show both.
	w = serveRequest(g, http.MethodGet, "/", nil, noAuth())
	var result ListAllMyBucketsResult
	xml.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Buckets.Bucket) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(result.Buckets.Bucket))
	}

	// Put object in hyphenated bucket.
	w = serveRequest(g, http.MethodPut, "/my-bucket/test.txt", strings.NewReader("ok"), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("put to my-bucket: expected 200, got %d", w.Code)
	}

	_, err := os.ReadFile(filepath.Join(syncDir, "my-bucket", "test.txt"))
	if err != nil {
		t.Fatalf("file should exist in my-bucket: %v", err)
	}
}

// --- Edge Case: Multiple buckets ordering ---

func TestListBuckets_Ordering(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		os.Mkdir(filepath.Join(syncDir, name), 0o755)
	}

	w := serveRequest(g, http.MethodGet, "/", nil, noAuth())
	var result ListAllMyBucketsResult
	xml.Unmarshal(w.Body.Bytes(), &result)

	if len(result.Buckets.Bucket) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(result.Buckets.Bucket))
	}
	// os.ReadDir returns entries sorted alphabetically.
	if result.Buckets.Bucket[0].Name != "alpha" {
		t.Fatalf("expected first bucket=alpha, got %s", result.Buckets.Bucket[0].Name)
	}
	if result.Buckets.Bucket[1].Name != "bravo" {
		t.Fatalf("expected second bucket=bravo, got %s", result.Buckets.Bucket[1].Name)
	}
	if result.Buckets.Bucket[2].Name != "charlie" {
		t.Fatalf("expected third bucket=charlie, got %s", result.Buckets.Bucket[2].Name)
	}
}

// --- Edge Case: Delete object with nested key cleans parent but not bucket ---

func TestDeleteObject_CleansParentsButNotBucket(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.MkdirAll(filepath.Join(bp, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(bp, "a", "b", "file.txt"), []byte("data"), 0o644)

	w := serveRequest(g, http.MethodDelete, "/mybucket/a/b/file.txt", nil, noAuth())
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	// a/ and a/b/ should be cleaned up.
	if _, err := os.Stat(filepath.Join(bp, "a")); !os.IsNotExist(err) {
		t.Fatal("a/ should have been cleaned up")
	}

	// But the bucket itself must still exist.
	info, err := os.Stat(bp)
	if err != nil || !info.IsDir() {
		t.Fatal("bucket directory should still exist after cleanup")
	}
}

// --- Edge Case: max-keys=0 ---

func TestListObjectsV1_MaxKeysZero(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	bp := filepath.Join(syncDir, "mybucket")
	os.Mkdir(bp, 0o755)
	os.WriteFile(filepath.Join(bp, "a.txt"), []byte("a"), 0o644)

	// max-keys=0 or negative should default to 1000.
	w := serveRequest(g, http.MethodGet, "/mybucket?max-keys=0", nil, noAuth())
	var result ListBucketResultV1
	xml.Unmarshal(w.Body.Bytes(), &result)
	if result.MaxKeys != 1000 {
		t.Fatalf("expected MaxKeys=1000 (default for 0), got %d", result.MaxKeys)
	}
}

// --- Edge Case: XML response structure ---

func TestListObjectsV1_XMLHasXMLHeader(t *testing.T) {
	g, syncDir := testGateway(t, Config{})
	os.Mkdir(filepath.Join(syncDir, "mybucket"), 0o755)

	w := serveRequest(g, http.MethodGet, "/mybucket", nil, noAuth())
	body := w.Body.String()
	if !strings.HasPrefix(body, "<?xml version=") {
		t.Fatalf("response should start with XML header, got: %s", body[:min(50, len(body))])
	}
	if !strings.Contains(body, `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`) {
		t.Fatal("response should contain S3 namespace")
	}
	if w.Header().Get("Content-Type") != "application/xml" {
		t.Fatalf("expected Content-Type application/xml, got %s", w.Header().Get("Content-Type"))
	}
}

func TestErrorResponse_XMLStructure(t *testing.T) {
	g, _ := testGateway(t, Config{})

	w := serveRequest(g, http.MethodGet, "/nonexistent-bucket", nil, noAuth())
	body := w.Body.String()
	if !strings.HasPrefix(body, "<?xml version=") {
		t.Fatalf("error response should start with XML header, got: %s", body[:min(50, len(body))])
	}
	if !strings.Contains(body, "<Code>NoSuchBucket</Code>") {
		t.Fatal("error response should contain error code")
	}
	if w.Header().Get("Content-Type") != "application/xml" {
		t.Fatalf("expected Content-Type application/xml, got %s", w.Header().Get("Content-Type"))
	}
}

// --- Full Server Test (Start/Stop) ---

func TestGateway_StartStop(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	syncDir := t.TempDir()
	os.Mkdir(filepath.Join(syncDir, "testbucket"), 0o755)
	os.WriteFile(filepath.Join(syncDir, "testbucket", "hello.txt"), []byte("hello"), 0o644)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g := New(syncDir, []string{".DS_Store"}, Config{ListenAddr: addr}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- g.Start(ctx)
	}()

	// Wait for server to be ready.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/", addr))
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// List buckets.
	resp, err := http.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result ListAllMyBucketsResult
	xml.Unmarshal(body, &result)
	if len(result.Buckets.Bucket) != 1 || result.Buckets.Bucket[0].Name != "testbucket" {
		t.Fatalf("expected [testbucket], got %v", result.Buckets.Bucket)
	}

	// Get object.
	resp2, err := http.Get(fmt.Sprintf("http://%s/testbucket/hello.txt", addr))
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	defer resp2.Body.Close()
	objBody, _ := io.ReadAll(resp2.Body)
	if string(objBody) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(objBody))
	}

	// Stop.
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("gateway error: %v", err)
	}
}
