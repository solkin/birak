package httpui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUploadRejectsSymlinkEscapeInRelativePath(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	body, ct := createMultipartUpload("", "escape/evil.txt", "bad")
	req := httptest.NewRequest(http.MethodPost, "/_api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for symlink escape upload, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("upload must not create a file outside root through a symlink")
	}
}

func TestUploadRejectsSymlinkEscapeInTargetPath(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	body, ct := createMultipartUpload("escape", "evil.txt", "bad")
	req := httptest.NewRequest(http.MethodPost, "/_api/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for symlink escape target path, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("upload must not create a file outside root through target path symlink")
	}
}
