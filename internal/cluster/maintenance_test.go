package cluster

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/quorum"
)

func TestHTTPSMaintenanceAndBackupBoundary(t *testing.T) {
	c := newTestCluster(t)
	n1 := c.start("n1", true)
	c.leader()
	n2 := c.start("n2", false)
	c.join(n1, n2)
	n3 := c.start("n3", false)
	c.join(n1, n3)
	ctx := context.Background()
	if _, err := n1.Node().TransactOnce(ctx, "bucket", []quorum.Change{{Key: "b/bucket", MetaOnly: true}}, nil); err != nil {
		t.Fatal(err)
	}
	e, err := n1.Node().Put(ctx, "object", "o/bucket/key", strings.NewReader("durable backup"), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range c.nodes {
		deadline := time.Now().Add(3 * time.Second)
		for s.Node().Status().Index < e.Index {
			if time.Now().After(deadline) {
				t.Fatal("metadata did not arrive")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := s.Node().Reconcile(ctx, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	// Same-size bitrot must be repaired through the authenticated byte endpoint.
	path := filepath.Join(n2.options.Dir, "generations", "objects", e.Ref.Hash[:2], e.Ref.Hash[2:])
	if err := os.WriteFile(path, []byte("damaged backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := n2.Node().Scrub(ctx, 1<<30); err != nil {
		t.Fatal(err)
	}
	client := c.client("operator", "n1")
	defer client.CloseIdleConnections()
	metrics, err := client.Get("https://" + n1.Address() + "/v1/admin/metrics")
	if err != nil {
		t.Fatal(err)
	}
	mb, err := io.ReadAll(metrics.Body)
	metrics.Body.Close()
	if err != nil || !bytes.Contains(mb, []byte("birak_quorum_leader 1")) {
		t.Fatal(string(mb), err)
	}
	r, err := client.Get("https://" + n1.Address() + "/v1/admin/backup")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || r.StatusCode != 200 || r.Trailer.Get("X-Birak-Backup-Complete") != "true" {
		t.Fatal(r.Status, err, r.Trailer)
	}
	if !bytes.Contains(archive, []byte("durable backup")) {
		t.Fatal("backup omitted committed payload")
	}
	if _, err := n1.Node().Delete(ctx, "delete", "o/bucket/key"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, s := range c.nodes {
		path := filepath.Join(s.options.Dir, "generations", "objects", e.Ref.Hash[:2], e.Ref.Hash[2:])
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	result, err := n1.Node().Collect(ctx, time.Hour)
	if err != nil || len(result.Errors) != 0 {
		t.Fatal(result, err)
	}
	for _, s := range c.nodes {
		if result.Nodes[s.Node().Identity().Node].Files != 1 {
			t.Fatal(result)
		}
		if f, err := s.Node().LocalBlob(ctx, e.Ref); err == nil {
			f.Close()
			t.Fatal("collected blob remained")
		}
	}
	// Old binaries cannot participate in new Raft/blob traffic even with a valid
	// current certificate; request AND response formats are fenced.
	peer := c.client("n2", "n1")
	defer peer.CloseIdleConnections()
	req, _ := http.NewRequest("POST", "https://"+n1.Address()+"/v1/inventory", strings.NewReader("[]"))
	req.Header.Set("X-Birak-Format", "birak-quorum-v2")
	r, err = peer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 409 {
		t.Fatal(r.Status)
	}
}
