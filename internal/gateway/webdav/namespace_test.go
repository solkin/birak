package webdav

import (
	"github.com/birak/birak/internal/gateway"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceVisibility(t *testing.T) {
	g, ts := newTestGateway(t, nil, "", "")
	root := g.syncDir
	for name, body := range map[string]string{"visible.txt": "visible", ".birak-tmp-partial": "partial"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reserved := filepath.Join(root, gateway.ReservedDirName)
	if err := os.MkdirAll(reserved, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reserved, "state"), []byte("internal state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(reserved, filepath.Join(root, "state-alias")); err != nil {
		t.Fatal(err)
	}
	resp := doReq(t, "PROPFIND", ts.URL+"/", "", map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("propfind: %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "visible.txt") {
		t.Fatal("visible file missing")
	}
	for _, hidden := range []string{gateway.ReservedDirName, ".birak-tmp-partial", "state-alias"} {
		if strings.Contains(body, hidden) {
			t.Errorf("listing exposed %s", hidden)
		}
	}
	resp = doReq(t, http.MethodGet, ts.URL+"/state-alias/state", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("reserved state accessible through alias")
	}
}
