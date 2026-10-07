package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/birak/birak/internal/cluster"
	"github.com/birak/birak/internal/quorum"
)

func TestDaemonCollectionSurvivesSIGKILLDuringBlobTransfer(t *testing.T) {
	l := newDaemonLab(t)
	n1 := l.add()
	l.leader()
	n2 := l.add()
	l.change(n1, n2, "join")
	l.change(n1, n2, "promote")
	n3 := l.add()
	l.change(n1, n3, "join")
	l.change(n1, n3, "promote")
	l.expect(n1, "PUT", "/bucket", nil, nil, 200)
	old := []byte("old committed payload")
	sum := sha256.Sum256(old)
	hash := hex.EncodeToString(sum[:])
	l.expect(n1, "PUT", "/bucket/key", old, nil, 200)
	// Explicit catch-up makes the disk-loss assertions independent of which two
	// voters won the foreground data race.
	l.change(n1, n2, "catch-up")
	l.change(n1, n3, "catch-up")
	l.expect(n1, "DELETE", "/bucket/key", nil, nil, 204)
	oldTime := time.Now().Add(-48 * time.Hour)
	for _, n := range l.nodes {
		p := filepath.Join(n.dir, "generations", "objects", hash[:2], hash[2:])
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}

	// Hold an authenticated pre-freeze receive in the old leader. Its blob gate
	// prevents physical GC while the Raft fence can already be committed.
	creds, err := cluster.LoadCredentials(filepath.Join(l.dir, "ca.crt"), filepath.Join(l.dir, "node-n2.crt"), filepath.Join(l.dir, "node-n2.key"), cluster.Principal{Cluster: "e2e", Role: "node", ID: "n2"})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: creds.ClientTLS("n1")}, Timeout: 20 * time.Second}
	defer client.CloseIdleConnections()
	body := bytes.Repeat([]byte("uncommitted"), 8192)
	digest := sha256.Sum256(body)
	blobHash := hex.EncodeToString(digest[:])
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	req, _ := http.NewRequest("PUT", "https://"+n1.address+"/v1/blobs/"+blobHash+"?size="+strconv.Itoa(len(body)), pr)
	req.ContentLength = int64(len(body))
	req.Header.Set("X-Birak-Format", quorum.Format)
	transfer := make(chan int, 1)
	go func() {
		r, err := client.Do(req)
		if err != nil {
			transfer <- 0
			return
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		transfer <- r.StatusCode
	}()
	if _, err := pw.Write(body[:len(body)/2]); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(filepath.Join(n1.dir, "generations", "staging"))
		if err != nil {
			t.Fatal(err)
		}
		receiving := false
		for _, entry := range entries {
			info, err := entry.Info()
			if err == nil && info.Size() > 0 {
				receiving = true
			}
		}
		if receiving {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receive did not enter staging")
		}
		time.Sleep(10 * time.Millisecond)
	}
	collected := make(chan int, 1)
	go func() {
		_, code := l.adminCall(n1, "collect", &cluster.Member{ID: n1.id, Grace: time.Hour})
		collected <- code
	}()
	for {
		b, code := l.adminCall(n1, "status", nil)
		var status quorum.Status
		if code == 200 && json.Unmarshal(b, &status) == nil && status.Collection != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("collection fence not committed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.kill(n1)
	pw.Close()
	if code := <-transfer; code == 200 {
		t.Fatal("interrupted receive acknowledged")
	}
	if code := <-collected; code == 200 {
		t.Fatal("killed collector reported success")
	}

	var leader *daemonProcess
	deadline = time.Now().Add(15 * time.Second)
	for leader == nil {
		for _, n := range []*daemonProcess{n2, n3} {
			b, code := l.adminCall(n, "status", nil)
			var status quorum.Status
			if code == 200 && json.Unmarshal(b, &status) == nil && status.State == "Leader" && status.Collection != nil {
				leader = n
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("new leader lost collection fence")
		}
		if leader == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if b, code := l.adminCall(leader, "collect", &cluster.Member{ID: leader.id, Grace: time.Hour}); code != 200 {
		t.Fatal(string(b), code)
	}
	l.expect(leader, "PUT", "/bucket/new", []byte("after recovery"), nil, 200)
	l.expect(leader, "GET", "/bucket/key", nil, nil, 404)
	l.start(n1, false)
	deadline = time.Now().Add(10 * time.Second)
	for {
		b, code := l.adminCall(n1, "status", nil)
		var status quorum.Status
		if code == 200 && json.Unmarshal(b, &status) == nil && string(status.Leader) == leader.id && status.Collection == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("returned member has not applied current leadership")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b, code := l.adminCall(leader, "collect", &cluster.Member{ID: leader.id, Grace: time.Hour}); code != 200 {
		t.Fatal(string(b), code)
	} else {
		var result quorum.CollectionResult
		if err := json.Unmarshal(b, &result); err != nil || len(result.Errors) != 0 {
			t.Fatal(string(b), err)
		}
	}
	if _, err := os.Stat(filepath.Join(n1.dir, "generations", "objects", hash[:2], hash[2:])); !os.IsNotExist(err) {
		t.Fatal("returned node retained old garbage", err)
	}
	if b, _ := l.expect(n1, "GET", "/bucket/new", nil, nil, 200); string(b) != "after recovery" {
		t.Fatal(string(b))
	}
}
