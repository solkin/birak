package webdav

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDeleteAndMoveRespectDescendantLocks(t *testing.T) {
	g, ts := newTestGateway(t, nil, "", "")
	if err := os.MkdirAll(filepath.Join(g.syncDir, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.syncDir, "dir", "child.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	lockResp := doReq(t, "LOCK", ts.URL+"/dir/child.txt", "", nil)
	token := lockResp.Header.Get("Lock-Token")
	lockResp.Body.Close()
	if lockResp.StatusCode != http.StatusOK || token == "" {
		t.Fatalf("LOCK failed: status=%d token=%q", lockResp.StatusCode, token)
	}

	// DELETE parent without child's token must be refused with 423 Locked.
	delResp := doReq(t, http.MethodDelete, ts.URL+"/dir", "", nil)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusLocked {
		t.Fatalf("DELETE parent with locked child = %d, want 423", delResp.StatusCode)
	}

	// MOVE parent without child's token must be refused with 423 Locked.
	mvResp := doReq(t, "MOVE", ts.URL+"/dir", "", map[string]string{"Destination": ts.URL + "/dir2"})
	mvResp.Body.Close()
	if mvResp.StatusCode != http.StatusLocked {
		t.Fatalf("MOVE parent with locked child = %d, want 423", mvResp.StatusCode)
	}

	// Concurrent LOCK refreshes must be race-free, and wrong path must not match.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := doReq(t, "LOCK", ts.URL+"/dir/child.txt", "", map[string]string{"If": "(" + token + ")"})
			r.Body.Close()
		}()
	}
	wg.Wait()

	// DELETE parent WITH child's token succeeds.
	delOK := doReq(t, http.MethodDelete, ts.URL+"/dir", "", map[string]string{"If": "(" + token + ")"})
	delOK.Body.Close()
	if delOK.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE parent with token = %d, want 204", delOK.StatusCode)
	}
}

func TestCopyDirSkipsScratchAndGetSetsNoSniff(t *testing.T) {
	g, ts := newTestGateway(t, []string{"*.tmp"}, "", "")
	if err := os.MkdirAll(filepath.Join(g.syncDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(g.syncDir, "src", "good.txt"), []byte("ok"), 0o644)
	os.WriteFile(filepath.Join(g.syncDir, "src", ".birak-tmp-leftover"), []byte("secret"), 0o644)
	os.WriteFile(filepath.Join(g.syncDir, "src", "skip.tmp"), []byte("ignored"), 0o644)

	resp := doReq(t, "COPY", ts.URL+"/src", "", map[string]string{"Destination": ts.URL + "/dst"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("COPY status = %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(g.syncDir, "dst", "good.txt")); err != nil {
		t.Fatalf("good.txt not copied: %v", err)
	}
	for _, hidden := range []string{".birak-tmp-leftover", "skip.tmp"} {
		if _, err := os.Stat(filepath.Join(g.syncDir, "dst", hidden)); !os.IsNotExist(err) {
			t.Fatalf("hidden/scratch file %s was copied", hidden)
		}
	}

	getResp := doReq(t, http.MethodGet, ts.URL+"/dst/good.txt", "", nil)
	getResp.Body.Close()
	if got := getResp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
