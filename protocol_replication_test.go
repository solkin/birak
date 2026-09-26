package birak_test

import (
	"context"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/gateway/s3"
	"github.com/birak/birak/internal/multipart"
)

func TestMultipartReplicationPublishesOnlyCompletedObject(t *testing.T) {
	source := newTestNodeWithAddr(t, "upload-source", reserveAddr(t), nil)
	peer := newTestNodeWithAddr(t, "upload-peer", reserveAddr(t), []string{"http://" + source.addr})
	if err := os.Mkdir(filepath.Join(source.syncDir, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source.syncDir, "bucket"), "keep.txt", "keep bucket present")
	store, err := multipart.New(source.syncDir, multipart.Limits{}, source.logger)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveS3(t, source, store)
	client := &http.Client{Timeout: 5 * time.Second}
	request := func(method, path, body string) ([]byte, http.Header) {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
		}
		return data, resp.Header
	}
	data, _ := request("POST", "/bucket/uploaded.txt?uploads", "")
	var up struct {
		ID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(data, &up); err != nil || up.ID == "" {
		t.Fatalf("upload ID: %s %v", data, err)
	}
	_, headers := request("PUT", "/bucket/uploaded.txt?partNumber=1&uploadId="+up.ID, "completed payload")
	// A replicated marker establishes that the live watcher and peer polling have
	// run after staging, avoiding a timing-only assertion that nothing appeared.
	writeFile(t, source.syncDir, "barrier.txt", "after staging")
	waitForSync(t, 10*time.Second, func() bool { return fileExists(peer.syncDir, "barrier.txt") })
	for _, node := range []*testNode{source, peer} {
		if fileExists(node.syncDir, "bucket/uploaded.txt") {
			t.Fatal("incomplete upload appeared as an object")
		}
		assertNoStagingMetadata(t, node)
	}
	if fileExists(peer.syncDir, filepath.Join(gateway.ReservedDirName, "multipart", up.ID)) {
		t.Fatal("staged upload replicated")
	}
	body := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>" + headers.Get("ETag") + "</ETag></Part></CompleteMultipartUpload>"
	request("POST", "/bucket/uploaded.txt?uploadId="+up.ID, body)
	waitForSync(t, 10*time.Second, func() bool {
		got, err := os.ReadFile(filepath.Join(peer.syncDir, "bucket", "uploaded.txt"))
		return err == nil && string(got) == "completed payload"
	})
	for _, node := range []*testNode{source, peer} {
		assertNoStagingMetadata(t, node)
	}
	if fileExists(source.syncDir, filepath.Join(gateway.ReservedDirName, "multipart", up.ID)) {
		t.Fatal("completed staging was not removed")
	}
}

func assertNoStagingMetadata(t *testing.T, node *testNode) {
	t.Helper()
	entries, err := node.store.ListManifest("", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, gateway.ReservedDirName+"/") {
			t.Fatalf("staging indexed on %s: %s", node.id, entry.Name)
		}
		for _, part := range strings.Split(entry.Name, "/") {
			if gateway.IsScratchFile(part) {
				t.Fatalf("scratch indexed on %s: %s", node.id, entry.Name)
			}
		}
	}
}

// DeleteObjects deletes each key through the same commit as DeleteObject, so
// every deletion of a batch is indexed and replicated like any other, and the
// directories it empties are cleaned up on both nodes.
func TestDeleteObjectsReplicatesEveryDeletion(t *testing.T) {
	source := newTestNodeWithAddr(t, "batch-source", reserveAddr(t), nil)
	peer := newTestNodeWithAddr(t, "batch-peer", reserveAddr(t), []string{"http://" + source.addr})
	bucket := filepath.Join(source.syncDir, "bucket")
	if err := os.MkdirAll(filepath.Join(bucket, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	gone := []string{"one.txt", "two.txt", "nested/three.txt"}
	for _, name := range append([]string{"keep.txt"}, gone...) {
		writeFile(t, bucket, name, "contents of "+name)
	}
	waitForSync(t, 10*time.Second, func() bool {
		for _, name := range append([]string{"keep.txt"}, gone...) {
			if !fileExists(filepath.Join(peer.syncDir, "bucket"), name) {
				return false
			}
		}
		return true
	})

	addr := serveS3(t, source, nil)
	var body strings.Builder
	body.WriteString("<Delete>")
	for _, name := range gone {
		body.WriteString("<Object><Key>" + name + "</Key></Object>")
	}
	body.WriteString("</Delete>")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post("http://"+addr+"/bucket?delete", "application/xml", strings.NewReader(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Deleted []struct {
			Key string `xml:"Key"`
		} `xml:"Deleted"`
		Errors []struct {
			Key  string `xml:"Key"`
			Code string `xml:"Code"`
		} `xml:"Error"`
	}
	if err := xml.Unmarshal(data, &result); err != nil || resp.StatusCode != http.StatusOK ||
		len(result.Deleted) != len(gone) || len(result.Errors) != 0 {
		t.Fatalf("DeleteObjects: %d %s %v", resp.StatusCode, data, err)
	}

	for _, name := range gone {
		waitForDeletionMetadata(t, "bucket/"+name, source, peer)
	}
	waitForSync(t, 10*time.Second, func() bool {
		return !fileExists(source.syncDir, "bucket/nested") && !fileExists(peer.syncDir, "bucket/nested")
	})
	if got := readFile(t, filepath.Join(peer.syncDir, "bucket"), "keep.txt"); got != "contents of keep.txt" {
		t.Fatalf("an object not named changed on the peer: %q", got)
	}
}

// reserveAddr returns a loopback address that nothing listens on yet.
func reserveAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// serveS3 runs an S3 gateway over a node's files until the test ends and
// returns its address once it answers.
func serveS3(t *testing.T, node *testNode, store *multipart.Store) string {
	t.Helper()
	addr := reserveAddr(t)
	gw := s3.New(node.syncDir, nil, s3.Config{ListenAddr: addr, Multipart: store}, node.logger)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := gw.Stop(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("S3 did not stop")
		}
	})
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/")
		if err == nil {
			resp.Body.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatal("S3 did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
