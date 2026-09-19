package webdav

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

func TestCopyMoveRechecksOverwriteBeforeCommit(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		t.Run(method, func(t *testing.T) {
			root := t.TempDir()
			_, _, logger := reviewState(t, root)
			reviewWrite(t, root, "src", "source", time.Now())
			// Model another request creating the destination after this request's
			// early precondition check, before its final namespace commit.
			fileops.SetHooks(root, fileops.Hooks{Begin: func([]string) error {
				return os.WriteFile(filepath.Join(root, "dst"), []byte("concurrent winner"), 0o600)
			}})
			g := New(root, nil, Config{}, logger)
			req := httptest.NewRequest(method, "http://node/src", nil)
			req.Header.Set("Destination", "http://node/dst")
			req.Header.Set("Overwrite", "F")
			out := httptest.NewRecorder()
			g.server.Handler.ServeHTTP(out, req)
			if out.Code != http.StatusPreconditionFailed {
				t.Fatalf("late destination overwritten: status=%d body=%s", out.Code, out.Body)
			}
			for name, want := range map[string]string{"src": "source", "dst": "concurrent winner"} {
				if body, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(body) != want {
					t.Fatalf("%s lost: %q %v", name, body, err)
				}
			}
		})
	}
}

func TestCopyMoveRechecksDirectoryAliasBeforeCommit(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		t.Run(method, func(t *testing.T) {
			root := t.TempDir()
			_, _, logger := reviewState(t, root)
			for _, dir := range []string{"src", "other"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			reviewWrite(t, root, "src/file", "keep", time.Now())
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(filepath.Join(root, "other"), alias); err != nil {
				t.Fatal(err)
			}
			fileops.SetHooks(root, fileops.Hooks{Begin: func([]string) error {
				if err := os.Remove(alias); err != nil {
					return err
				}
				return os.Symlink(filepath.Join(root, "src"), alias)
			}})
			g := New(root, nil, Config{}, logger)
			req := httptest.NewRequest(method, "http://node/src", nil)
			req.Header.Set("Destination", "http://node/alias/child")
			out := httptest.NewRecorder()
			g.server.Handler.ServeHTTP(out, req)
			if out.Code != http.StatusForbidden {
				t.Fatalf("rebound alias bypassed guard: %d %s", out.Code, out.Body)
			}
			if entries, err := os.ReadDir(filepath.Join(root, "src")); err != nil || len(entries) != 1 || entries[0].Name() != "file" {
				t.Fatalf("source changed: %v %v", entries, err)
			}
		})
	}
}
