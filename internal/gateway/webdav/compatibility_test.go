package webdav

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestCopy_OverwriteTrue(t *testing.T) {
	g, ts := newTestGateway(t, nil, "", "")
	os.WriteFile(filepath.Join(g.syncDir, "src.txt"), []byte("source"), 0o644)
	os.WriteFile(filepath.Join(g.syncDir, "dst.txt"), []byte("old"), 0o644)

	resp := doReq(t, "COPY", ts.URL+"/src.txt", "", map[string]string{
		"Destination": ts.URL + "/dst.txt",
		"Overwrite":   "T",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for overwrite, got %d", resp.StatusCode)
	}
	data, _ := os.ReadFile(filepath.Join(g.syncDir, "dst.txt"))
	if string(data) != "source" {
		t.Fatalf("expected source, got %q", data)
	}
}

func TestCopy_OverwriteFalse(t *testing.T) {
	g, ts := newTestGateway(t, nil, "", "")
	os.WriteFile(filepath.Join(g.syncDir, "src.txt"), []byte("source"), 0o644)
	os.WriteFile(filepath.Join(g.syncDir, "dst.txt"), []byte("existing"), 0o644)

	resp := doReq(t, "COPY", ts.URL+"/src.txt", "", map[string]string{
		"Destination": ts.URL + "/dst.txt",
		"Overwrite":   "F",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", resp.StatusCode)
	}
	data, _ := os.ReadFile(filepath.Join(g.syncDir, "dst.txt"))
	if string(data) != "existing" {
		t.Fatalf("destination should not be modified")
	}
}

func TestCopy_SourceNotFound(t *testing.T) {
	_, ts := newTestGateway(t, nil, "", "")

	resp := doReq(t, "COPY", ts.URL+"/nonexistent", "", map[string]string{
		"Destination": ts.URL + "/dst.txt",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCopy_IntoSubdir(t *testing.T) {
	g, ts := newTestGateway(t, nil, "", "")
	os.WriteFile(filepath.Join(g.syncDir, "src.txt"), []byte("data"), 0o644)

	resp := doReq(t, "COPY", ts.URL+"/src.txt", "", map[string]string{
		"Destination": ts.URL + "/sub/copy.txt",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data, _ := os.ReadFile(filepath.Join(g.syncDir, "sub", "copy.txt"))
	if string(data) != "data" {
		t.Fatalf("expected data, got %q", data)
	}
}

func TestMove_SourceNotFound(t *testing.T) {
	_, ts := newTestGateway(t, nil, "", "")

	resp := doReq(t, "MOVE", ts.URL+"/nonexistent", "", map[string]string{
		"Destination": ts.URL + "/dst.txt",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestMove_MissingDestination(t *testing.T) {
	g, ts := newTestGateway(t, nil, "", "")
	os.WriteFile(filepath.Join(g.syncDir, "src.txt"), []byte("data"), 0o644)

	resp := doReq(t, "MOVE", ts.URL+"/src.txt", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing Destination, got %d", resp.StatusCode)
	}
}

func TestMove_Directory(t *testing.T) {
	g, ts := newTestGateway(t, nil, "", "")
	os.MkdirAll(filepath.Join(g.syncDir, "srcdir"), 0o755)
	os.WriteFile(filepath.Join(g.syncDir, "srcdir", "f.txt"), []byte("x"), 0o644)

	resp := doReq(t, "MOVE", ts.URL+"/srcdir", "", map[string]string{
		"Destination": ts.URL + "/dstdir",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data, err := os.ReadFile(filepath.Join(g.syncDir, "dstdir", "f.txt"))
	if err != nil {
		t.Fatal("file should exist in moved dir")
	}
	if string(data) != "x" {
		t.Fatalf("expected x, got %q", data)
	}
}

func TestDelete_Traversal(t *testing.T) {
	_, ts := newTestGateway(t, nil, "", "")

	resp := doReq(t, http.MethodDelete, ts.URL+"/../../etc/passwd", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		t.Fatal("should reject traversal in DELETE")
	}
}

func TestGet_Traversal(t *testing.T) {
	_, ts := newTestGateway(t, nil, "", "")

	resp := doReq(t, http.MethodGet, ts.URL+"/../../etc/passwd", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("should reject traversal in GET")
	}
}

func TestPropfind_Traversal(t *testing.T) {
	_, ts := newTestGateway(t, nil, "", "")

	resp := doReq(t, "PROPFIND", ts.URL+"/../../etc", "", map[string]string{
		"Depth": "0",
	})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMultiStatus {
		t.Fatal("should reject traversal in PROPFIND")
	}
}
