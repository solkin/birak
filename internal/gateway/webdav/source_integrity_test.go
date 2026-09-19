package webdav

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyMoveCannotBlessCorruptSource(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		for _, tree := range []bool{false, true} {
			name := method + "/file"
			if tree {
				name = method + "/tree"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				st, w, logger := reviewState(t, root)
				source := "src"
				if tree {
					source = "src/child"
					os.Mkdir(filepath.Join(root, "src"), 0o700)
				}
				stamp := time.Now().Add(-time.Hour)
				reviewWrite(t, root, source, "healthy!", stamp)
				reviewWrite(t, root, "dst", "old destination", stamp)
				if err := w.Refresh(source); err != nil {
					t.Fatal(err)
				}
				if err := w.Refresh("dst"); err != nil {
					t.Fatal(err)
				}
				original, _ := st.GetFile(source)
				reviewWrite(t, root, source, "corrupt!", stamp)
				g := New(root, nil, Config{}, logger)
				req := httptest.NewRequest(method, "http://node/src", nil)
				req.Header.Set("Destination", "http://node/dst")
				out := httptest.NewRecorder()
				g.server.Handler.ServeHTTP(out, req)
				if out.Code < 400 {
					t.Fatalf("%s advertised corrupt bytes as a trusted new write: %d", method, out.Code)
				}
				got, err := os.ReadFile(filepath.Join(root, "dst"))
				if err != nil || string(got) != "old destination" {
					t.Fatalf("old destination lost: %q %v", got, err)
				}
				meta, _ := st.GetFile(source)
				if meta == nil || meta.Deleted || meta.Hash != original.Hash {
					t.Fatalf("trusted source state lost: %+v", meta)
				}
			})
		}
	}
}
