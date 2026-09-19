package syncer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/birak/birak/internal/server"
)

func TestReplicationBypassesPreviouslyCachedFile(t *testing.T) {
	source, _ := auditSyncer(t)
	dest, _ := auditSyncer(t)
	auditIndex(t, source, auditMeta("file", "OLD", 100), "OLD")
	origin := httptest.NewServer(server.New(source.store, source.syncDir, "origin", nil, server.Config{}, source.logger).Handler())
	defer origin.Close()
	resp, err := origin.Client().Get(origin.URL + "/files/file")
	if err != nil {
		t.Fatal(err)
	}
	cached, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(cached) != "OLD" {
		t.Fatalf("cache fixture: %q %v", cached, err)
	}
	oldHeaders := resp.Header.Clone()
	oldHeaders.Del("Cache-Control") // An entry retained from before the upgrade.
	auditIndex(t, source, auditMeta("file", "NEW", 200), "NEW")
	target, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	var cachedHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files/file" && !strings.Contains(r.Header.Get("Cache-Control"), "no-cache") {
			cachedHits.Add(1)
			for name, values := range oldHeaders {
				w.Header()[name] = append([]string(nil), values...)
			}
			_, _ = w.Write(cached)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	if _, err := dest.syncOnce(context.Background(), proxy.URL); err != nil {
		t.Fatal(err)
	}
	if n := cachedHits.Load(); n != 0 {
		t.Fatalf("replication reused stale bytes from HTTP cache: %d", n)
	}
	assertNamespaceBytes(t, dest, "file", "NEW")
	if count, err := dest.store.PendingRepairCount(proxy.URL); err != nil || count != 0 {
		t.Fatalf("stale cache left repair work: %d %v", count, err)
	}
}
