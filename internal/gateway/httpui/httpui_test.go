package httpui

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestGateway(t *testing.T, username, password string) (*Gateway, string) {
	t.Helper()
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	g := New(dir, []string{".DS_Store", ".birak-tmp-*"}, Config{
		ListenAddr: ":0",
		Username:   username,
		Password:   password,
	}, logger)
	return g, dir
}

func serve(g *Gateway, method, path string, body io.Reader, auth func(r *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if auth != nil {
		auth(req)
	}
	w := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(w, req)
	return w
}

func basicAuth(user, pass string) func(r *http.Request) {
	return func(r *http.Request) {
		r.SetBasicAuth(user, pass)
	}
}

func noAuth() func(r *http.Request) { return nil }

func jsonBody(v interface{}) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// --- Auth tests ---

func TestAuthRequired(t *testing.T) {
	g, _ := newTestGateway(t, "admin", "secret")
	w := serve(g, http.MethodGet, "/", nil, noAuth())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatal("expected WWW-Authenticate header")
	}
}

func TestAuthValid(t *testing.T) {
	g, _ := newTestGateway(t, "admin", "secret")
	w := serve(g, http.MethodGet, "/", nil, basicAuth("admin", "secret"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAuthInvalid(t *testing.T) {
	g, _ := newTestGateway(t, "admin", "secret")
	w := serve(g, http.MethodGet, "/", nil, basicAuth("admin", "wrong"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthDisabled(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- Page tests ---

func TestServesHTMLPage(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html, got %s", ct)
	}
	if !strings.Contains(w.Body.String(), "Birak") {
		t.Fatal("expected HTML to contain 'Birak'")
	}
}

func TestServesHTMLForAnyPath(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/some/deep/path/", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Fatal("expected HTML content type")
	}
}

func TestFavicon(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/favicon.ico", nil, noAuth())
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

// --- List tests ---

func TestListEmptyDir(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/_api/list?path=", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp listResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Path != "" {
		t.Fatalf("expected empty path, got %q", resp.Path)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(resp.Entries))
	}
}

func TestListWithFiles(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644) // ignored

	w := serve(g, http.MethodGet, "/_api/list?path=", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp listResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Entries) != 2 {
		t.Fatalf("expected 2 entries (ignored .DS_Store), got %d: %+v", len(resp.Entries), resp.Entries)
	}
}

func TestListSubdir(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	sub := filepath.Join(dir, "docs")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "readme.md"), []byte("# hi"), 0o644)

	w := serve(g, http.MethodGet, "/_api/list?path=docs", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp listResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Path != "docs" {
		t.Fatalf("expected path 'docs', got %q", resp.Path)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Name != "readme.md" {
		t.Fatalf("unexpected entries: %+v", resp.Entries)
	}
}

func TestListNotFound(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/_api/list?path=nonexistent", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListPathTraversal(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/_api/list?path=../../../etc", nil, noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- Download tests ---

func TestDownloadFile(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	os.WriteFile(filepath.Join(dir, "data.txt"), []byte("hello world"), 0o644)

	w := serve(g, http.MethodGet, "/_api/dl/data.txt", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "hello world" {
		t.Fatalf("unexpected body: %q", w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "data.txt") {
		t.Fatalf("expected Content-Disposition with filename, got %q", cd)
	}
}

func TestDownloadNestedFile(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "deep.txt"), []byte("nested"), 0o644)

	w := serve(g, http.MethodGet, "/_api/dl/a/b/deep.txt", nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "nested" {
		t.Fatalf("unexpected body: %q", w.Body.String())
	}
}

func TestDownloadNotFound(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/_api/dl/missing.txt", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDownloadDirectory(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	os.Mkdir(filepath.Join(dir, "folder"), 0o755)

	w := serve(g, http.MethodGet, "/_api/dl/folder", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for directory, got %d", w.Code)
	}
}

func TestDownloadPathTraversal(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	// The HTTP framework cleans "/../" from the path, so test with a path
	// that resolves outside syncDir after cleaning (e.g. the path that
	// filepath.Clean still considers traversal).
	w := serve(g, http.MethodGet, "/_api/dl/..%2f..%2fetc/passwd", nil, noAuth())
	// The framework normalises this to /_api/dl/../../etc/passwd → redirect or 400.
	// Either behaviour is acceptable as long as the file is not served.
	if w.Code == http.StatusOK {
		t.Fatal("expected traversal to be blocked, got 200")
	}
}

// --- Upload tests ---

func TestUploadFile(t *testing.T) {
	g, dir := newTestGateway(t, "", "")

	body, ct := createMultipartUpload("", "test.txt", "file content")
	req := httptest.NewRequest(http.MethodPost, "/_api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	data, err := os.ReadFile(filepath.Join(dir, "test.txt"))
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if string(data) != "file content" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

func TestUploadToSubdir(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	os.Mkdir(filepath.Join(dir, "uploads"), 0o755)

	body, ct := createMultipartUpload("uploads", "doc.pdf", "pdf data")
	req := httptest.NewRequest(http.MethodPost, "/_api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "uploads", "doc.pdf")); err != nil {
		t.Fatalf("file not found in subdir: %v", err)
	}
}

func TestUploadIgnoredFile(t *testing.T) {
	g, _ := newTestGateway(t, "", "")

	body, ct := createMultipartUpload("", ".DS_Store", "ignored")
	req := httptest.NewRequest(http.MethodPost, "/_api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for ignored file, got %d", w.Code)
	}
}

// --- Mkdir tests ---

func TestMkdir(t *testing.T) {
	g, dir := newTestGateway(t, "", "")

	w := serve(g, http.MethodPost, "/_api/mkdir", jsonBody(map[string]string{"path": "new-folder"}), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	info, err := os.Stat(filepath.Join(dir, "new-folder"))
	if err != nil || !info.IsDir() {
		t.Fatal("directory not created")
	}
}

func TestMkdirNested(t *testing.T) {
	g, dir := newTestGateway(t, "", "")

	w := serve(g, http.MethodPost, "/_api/mkdir", jsonBody(map[string]string{"path": "a/b/c"}), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	info, err := os.Stat(filepath.Join(dir, "a", "b", "c"))
	if err != nil || !info.IsDir() {
		t.Fatal("nested directory not created")
	}
}

func TestMkdirEmpty(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodPost, "/_api/mkdir", jsonBody(map[string]string{"path": ""}), noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- Rename tests ---

func TestRenameFile(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	os.WriteFile(filepath.Join(dir, "old.txt"), []byte("data"), 0o644)

	w := serve(g, http.MethodPost, "/_api/rename",
		jsonBody(map[string]string{"from": "old.txt", "to": "new.txt"}), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("old file should not exist")
	}
	data, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil || string(data) != "data" {
		t.Fatal("new file should have the content")
	}
}

func TestRenameDir(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	os.Mkdir(filepath.Join(dir, "old-dir"), 0o755)
	os.WriteFile(filepath.Join(dir, "old-dir", "f.txt"), []byte("x"), 0o644)

	w := serve(g, http.MethodPost, "/_api/rename",
		jsonBody(map[string]string{"from": "old-dir", "to": "new-dir"}), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if _, err := os.Stat(filepath.Join(dir, "new-dir", "f.txt")); err != nil {
		t.Fatal("renamed dir should preserve contents")
	}
}

func TestRenameNotFound(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodPost, "/_api/rename",
		jsonBody(map[string]string{"from": "nope.txt", "to": "x.txt"}), noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- Delete tests ---

func TestDeleteFile(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	os.WriteFile(filepath.Join(dir, "bye.txt"), []byte("gone"), 0o644)

	w := serve(g, http.MethodPost, "/_api/delete",
		jsonBody(map[string]string{"path": "bye.txt"}), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "bye.txt")); !os.IsNotExist(err) {
		t.Fatal("file should be deleted")
	}
}

func TestDeleteDir(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	sub := filepath.Join(dir, "rmdir")
	os.MkdirAll(filepath.Join(sub, "nested"), 0o755)
	os.WriteFile(filepath.Join(sub, "nested", "f.txt"), []byte("x"), 0o644)

	w := serve(g, http.MethodPost, "/_api/delete",
		jsonBody(map[string]string{"path": "rmdir"}), noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("directory should be deleted recursively")
	}
}

func TestDeleteRoot(t *testing.T) {
	g, _ := newTestGateway(t, "", "")

	for _, p := range []string{"", "/", "."} {
		w := serve(g, http.MethodPost, "/_api/delete",
			jsonBody(map[string]string{"path": p}), noAuth())
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for path %q, got %d", p, w.Code)
		}
	}
}

func TestDeleteNotFound(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodPost, "/_api/delete",
		jsonBody(map[string]string{"path": "nonexistent.txt"}), noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeletePathTraversal(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodPost, "/_api/delete",
		jsonBody(map[string]string{"path": "../../../tmp"}), noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- API 404 ---

func TestAPINotFound(t *testing.T) {
	g, _ := newTestGateway(t, "", "")
	w := serve(g, http.MethodGet, "/_api/nonexistent", nil, noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- Helper ---

func createMultipartUpload(path, filename, content string) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.WriteField("path", path)
	part, _ := writer.CreateFormFile("files", filename)
	part.Write([]byte(content))
	writer.Close()
	return &buf, writer.FormDataContentType()
}
