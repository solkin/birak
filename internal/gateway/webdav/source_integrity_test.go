package webdav

import (
	"fmt"
	"net/http"
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

func TestCopyDirectoryOverwriteIndexesAllEntriesCleanly(t *testing.T) {
	root := t.TempDir()
	st, w, logger := reviewState(t, root)
	stamp := time.Now().Add(-time.Hour)

	const files = 12
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range files {
		rel := fmt.Sprintf("src/f-%02d.bin", i)
		reviewWrite(t, root, rel, fmt.Sprintf("payload-%02d", i), stamp)
		if err := w.Refresh(rel); err != nil {
			t.Fatal(err)
		}
	}

	g := New(root, nil, Config{}, logger)

	// First COPY creates dst; second COPY overwrites dst via staged replacement.
	for pass := range 2 {
		req := httptest.NewRequest("COPY", "http://node/src", nil)
		req.Header.Set("Destination", "http://node/dst")
		req.Header.Set("Overwrite", "T")
		out := httptest.NewRecorder()
		g.server.Handler.ServeHTTP(out, req)
		wantCode := http.StatusCreated
		if pass == 1 {
			wantCode = http.StatusNoContent
		}
		if out.Code != wantCode {
			t.Fatalf("pass %d COPY status = %d, want %d (%s)", pass, out.Code, wantCode, out.Body.String())
		}
	}

	for i := range files {
		srcMeta, err := st.GetFile(fmt.Sprintf("src/f-%02d.bin", i))
		if err != nil || srcMeta == nil {
			t.Fatalf("src meta missing for %d: %v", i, err)
		}
		dstMeta, err := st.GetFile(fmt.Sprintf("dst/f-%02d.bin", i))
		if err != nil || dstMeta == nil || dstMeta.Deleted {
			t.Fatalf("dst meta missing for %d: %+v (%v)", i, dstMeta, err)
		}
		if dstMeta.Hash != srcMeta.Hash || dstMeta.Size != srcMeta.Size {
			t.Fatalf("dst/f-%02d mismatch: got %+v want hash=%s size=%d", i, dstMeta, srcMeta.Hash, srcMeta.Size)
		}
	}

	// Copy from child onto ancestor (/a/b -> /a) must keep source and destination
	// prehashed maps separate even when a nested path aliases a source path.
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	reviewWrite(t, root, "a/b/b/item.txt", "nested-b", stamp)
	reviewWrite(t, root, "a/b/item.txt", "outer-b", stamp)
	if err := w.Refresh("a/b/b/item.txt"); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("a/b/item.txt"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("COPY", "http://node/a/b", nil)
	req.Header.Set("Destination", "http://node/a")
	req.Header.Set("Overwrite", "T")
	out := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(out, req)
	if out.Code != http.StatusNoContent {
		t.Fatalf("COPY /a/b -> /a status = %d (%s)", out.Code, out.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "a", "b", "item.txt"))
	if err != nil || string(got) != "nested-b" {
		t.Fatalf("a/b/item.txt = %q (%v), want nested-b", got, err)
	}
	meta, err := st.GetFile("a/b/item.txt")
	if err != nil || meta == nil || meta.Deleted || meta.Size != int64(len("nested-b")) {
		t.Fatalf("indexed a/b/item.txt = %+v (%v)", meta, err)
	}
}
