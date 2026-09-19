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
	reserve := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		return addr
	}
	source := newTestNodeWithAddr(t, "upload-source", reserve(), nil)
	peer := newTestNodeWithAddr(t, "upload-peer", reserve(), []string{"http://" + source.addr})
	if err := os.Mkdir(filepath.Join(source.syncDir, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source.syncDir, "bucket"), "keep.txt", "keep bucket present")
	store, err := multipart.New(source.syncDir, multipart.Limits{}, source.logger)
	if err != nil {
		t.Fatal(err)
	}
	addr := reserve()
	gw := s3.New(source.syncDir, nil, s3.Config{ListenAddr: addr, Multipart: store}, source.logger)
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
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("S3 did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
