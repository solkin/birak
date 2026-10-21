package httpui

import (
	"bufio"
	"encoding/json"
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

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// Requests that can never succeed are answered 400 with a generic message.
// They used to be 500s carrying the error text, which names absolute paths:
// {"path":"apps/.."} answered `delete failed: invalid mutation path "/…/sync"`.
func TestErrors_InvalidNamesAreBadRequestsWithoutPaths(t *testing.T) {
	long := strings.Repeat("n", 300)
	cases := []struct {
		name, url string
		body      any // JSON body, or an upload relpath when a string with "upload:" prefix
	}{
		{"delete root alias", "/_api/delete", map[string]string{"path": "apps/.."}},
		{"delete NUL", "/_api/delete", map[string]string{"path": "a\x00b"}},
		{"rename from root alias", "/_api/rename", map[string]string{"from": "apps/..", "to": "x"}},
		{"rename to root alias", "/_api/rename", map[string]string{"from": "a.txt", "to": "apps/.."}},
		{"rename to NUL", "/_api/rename", map[string]string{"from": "a.txt", "to": "b\x00.txt"}},
		{"rename to long name", "/_api/rename", map[string]string{"from": "a.txt", "to": long}},
		{"rename dir into itself", "/_api/rename", map[string]string{"from": "apps", "to": "apps/x"}},
		{"mkdir NUL", "/_api/mkdir", map[string]string{"path": "a\x00b"}},
		{"mkdir long name", "/_api/mkdir", map[string]string{"path": "apps/" + long}},
		{"mkdir traversal", "/_api/mkdir", map[string]string{"path": "../../etc"}},
		{"upload long name", "/_api/upload", "upload:" + long + ".apk"},
		{"upload NUL", "/_api/upload", "upload:a\x00.apk"},
		{"list NUL", "/_api/list?path=a%00b", nil},
	}
	for _, watched := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/watched=%v", tc.name, watched), func(t *testing.T) {
				var g *Gateway
				var dir string
				if watched {
					g, dir = newWatchedGateway(t)
				} else {
					g, dir = newTestGateway(t, "", "")
				}
				if err := os.Mkdir(filepath.Join(dir, "apps"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
					t.Fatal(err)
				}
				var w *httptest.ResponseRecorder
				switch body := tc.body.(type) {
				case nil:
					w = serve(g, http.MethodGet, tc.url, nil, noAuth())
				case string:
					w = uploadForm(t, g, tc.url,
						formPart{name: "files", filename: "f.apk", value: "F"},
						formPart{name: "relpaths", value: strings.TrimPrefix(body, "upload:")})
				default:
					w = serve(g, http.MethodPost, tc.url, jsonBody(body), noAuth())
				}
				if w.Code != http.StatusBadRequest {
					t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
				}
				assertNoPaths(t, w, dir)
				if _, err := os.Stat(filepath.Join(dir, "apps")); err != nil {
					t.Fatalf("refused request changed the volume: %v", err)
				}
			})
		}
	}
}

// newWatchedGateway serves a volume whose commits pass through the watcher's
// hooks, as in birakd. Those refuse some mutations the filesystem alone would
// carry out — and their errors name absolute paths.
func newWatchedGateway(t *testing.T) (*Gateway, string) {
	t.Helper()
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	return New(root, []string{".DS_Store", ".birak-tmp-*"}, Config{ListenAddr: ":0"}, logger), root
}

// A clash with what is already on the volume is a conflict, not a server
// failure, and is not explained with paths either.
func TestErrors_ConflictsAreNotServerErrors(t *testing.T) {
	g, dir := newTestGateway(t, "", "")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, req := range []struct{ url, body string }{
		{"/_api/mkdir", `{"path":"a.txt/sub"}`},
		{"/_api/rename", `{"from":"a.txt","to":"a.txt/b.txt"}`},
	} {
		w := serve(g, http.MethodPost, req.url, strings.NewReader(req.body), noAuth())
		if w.Code != http.StatusConflict {
			t.Fatalf("%s %s: expected 409, got %d: %s", req.url, req.body, w.Code, w.Body.String())
		}
		assertNoPaths(t, w, dir)
	}
}

func assertNoPaths(t *testing.T, w *httptest.ResponseRecorder, dir string) {
	t.Helper()
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["error"] == "" {
		t.Fatalf("not a JSON error: %q (%v)", w.Body.String(), err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	for _, p := range []string{dir, real, filepath.Base(dir)} {
		if p != "" && strings.Contains(resp["error"], p) {
			t.Fatalf("error exposes a filesystem path: %q", resp["error"])
		}
	}
}

// A client that stops reading a download is routine; it is logged at Debug,
// not as an error.
func TestDownload_ClientAbortIsNotAnError(t *testing.T) {
	g, dir, logs := newLoggedGateway(t, Config{})
	bigFile(t, filepath.Join(dir, "big.apk"), 64<<20)
	srv := httptest.NewServer(g.server.Handler)
	defer srv.Close()

	conn := startDownload(t, srv, "big.apk")
	conn.Close()

	rec := logs.waitFor(t, "download aborted by client")
	if rec.Level != slog.LevelDebug {
		t.Fatalf("client abort logged at %v", rec.Level)
	}
	if errs := logs.errors(); len(errs) != 0 {
		t.Fatalf("client abort logged as an error: %v", errs)
	}
}

// A download the server cannot finish is not mistaken for a client abort: a
// reader revoked mid-stream by a quarantine is reported.
func TestDownload_RevokedReadIsReported(t *testing.T) {
	g, dir, logs := newLoggedGateway(t, Config{})
	path := filepath.Join(dir, "big.apk")
	bigFile(t, path, 64<<20)
	srv := httptest.NewServer(g.server.Handler)
	defer srv.Close()

	conn := startDownload(t, srv, "big.apk")
	defer conn.Close()
	unlock := fileops.Lock(dir)
	fileops.RevokeReadersLocked(dir, path)
	unlock()
	io.Copy(io.Discard, conn) // let the server reach its next read

	rec := logs.waitFor(t, "download revoked")
	if rec.Level != slog.LevelWarn {
		t.Fatalf("revoked download logged at %v", rec.Level)
	}
	if _, ok := logs.find("download aborted by client"); ok {
		t.Fatal("server-side read failure was logged as a client abort")
	}
}

func bigFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// startDownload requests a file over a raw connection and reads the response
// headers and a little of the body, so the server is mid-stream.
func startDownload(t *testing.T, srv *httptest.Server, name string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /_api/dl/%s HTTP/1.1\r\nHost: %s\r\n\r\n", name, srv.Listener.Addr())
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status %d", resp.StatusCode)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	return conn
}
