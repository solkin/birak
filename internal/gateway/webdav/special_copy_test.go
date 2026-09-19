package webdav

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Run the request in a subprocess so a regression that opens a FIFO for reading
// fails promptly instead of blocking every mutation behind the global lock.
func TestCopyRejectsFIFOWithoutBlocking(t *testing.T) {
	if mode := os.Getenv("BIRAK_TEST_FIFO_COPY"); mode != "" {
		root := t.TempDir()
		_, w, logger := reviewState(t, root)
		source := filepath.Join(root, "src")
		if mode == "tree" {
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			source = filepath.Join(source, "pipe")
		}
		if err := syscall.Mkfifo(source, 0o600); err != nil {
			t.Fatal(err)
		}
		reviewWrite(t, root, "dst", "keep", time.Now())
		if err := w.Refresh("dst"); err != nil {
			t.Fatal(err)
		}
		g := New(root, nil, Config{}, logger)
		req := httptest.NewRequest("COPY", "http://node/src", nil)
		req.Header.Set("Destination", "http://node/dst")
		out := httptest.NewRecorder()
		g.server.Handler.ServeHTTP(out, req)
		if out.Code < 400 {
			t.Fatalf("FIFO COPY succeeded: %d", out.Code)
		}
		if body, err := os.ReadFile(filepath.Join(root, "dst")); err != nil || string(body) != "keep" {
			t.Fatalf("destination lost: %q %v", body, err)
		}
		return
	}
	for _, mode := range []string{"file", "tree"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCopyRejectsFIFOWithoutBlocking$")
			cmd.Env = append(os.Environ(), "BIRAK_TEST_FIFO_COPY="+mode)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("COPY blocked on FIFO: %v\n%s", ctx.Err(), out)
			}
			if err != nil {
				t.Fatalf("COPY failed: %v\n%s", err, out)
			}
		})
	}
}
