package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/gateway"
)

func TestHandleFileRejectsSymlinkToPrivateOrExternalData(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(nil, root, "node", nil, logger)

	visible := filepath.Join(root, "visible.txt")
	if err := os.WriteFile(visible, []byte("visible"), 0o644); err != nil {
		t.Fatal(err)
	}
	reserved := filepath.Join(root, gateway.ReservedDirName)
	if err := os.Mkdir(reserved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reserved, "upload.json"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(reserved, "upload.json"), filepath.Join(root, "state-alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	request := func(name string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/"+name, nil))
		return w
	}

	if w := request("visible.txt"); w.Code != http.StatusOK || w.Body.String() != "visible" {
		t.Fatalf("visible file: status=%d body=%q", w.Code, w.Body.String())
	}
	for _, alias := range []string{"state-alias", "outside-alias"} {
		if w := request(alias); w.Code == http.StatusOK {
			t.Fatalf("unsafe symlink %q was served: %q", alias, w.Body.String())
		}
	}
}
