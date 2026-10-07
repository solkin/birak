package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/birak/birak/internal/cluster"
	"github.com/birak/birak/internal/quorum"
	"gopkg.in/yaml.v3"
)

func TestQuorumDaemonHelper(t *testing.T) {
	path := os.Getenv("BIRAK_TEST_DAEMON_CONFIG")
	if path == "" {
		return
	}
	if err := runBootstrap(path, os.Getenv("BIRAK_TEST_BOOTSTRAP") == "1"); err != nil {
		t.Fatal(err)
	}
}

type daemonProcess struct {
	id, address, s3, dir, config string
	cmd                          *exec.Cmd
	log                          string
}
type daemonLab struct {
	t      *testing.T
	dir    string
	nodes  []*daemonProcess
	admin  *cluster.Credentials
	client *http.Client
}

func unusedAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}
func newDaemonLab(t *testing.T) *daemonLab {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("strict NTFS durability not yet validated")
	}
	dir := t.TempDir()
	if err := cluster.CreateCA(dir, "e2e"); err != nil {
		t.Fatal(err)
	}
	if err := cluster.IssueCertificate(dir, cluster.Principal{Cluster: "e2e", Role: "admin", ID: "op"}, nil); err != nil {
		t.Fatal(err)
	}
	cred, err := cluster.LoadCredentials(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "admin-op.crt"), filepath.Join(dir, "admin-op.key"), cluster.Principal{Cluster: "e2e", Role: "admin", ID: "op"})
	if err != nil {
		t.Fatal(err)
	}
	l := &daemonLab{t: t, dir: dir, admin: cred, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	t.Cleanup(func() {
		l.client.CloseIdleConnections()
		for _, n := range l.nodes {
			l.kill(n)
			if t.Failed() {
				b, _ := os.ReadFile(n.log)
				t.Logf("%s log:\n%s", n.id, b)
			}
		}
	})
	return l
}
func (l *daemonLab) add() *daemonProcess {
	l.t.Helper()
	id := fmt.Sprintf("n%d", len(l.nodes)+1)
	if err := cluster.IssueCertificate(l.dir, cluster.Principal{Cluster: "e2e", Role: "node", ID: id}, []string{"127.0.0.1"}); err != nil {
		l.t.Fatal(err)
	}
	n := &daemonProcess{id: id, address: unusedAddress(l.t), s3: unusedAddress(l.t), dir: filepath.Join(l.dir, id)}
	n.config = filepath.Join(l.dir, id+".yaml")
	n.log = filepath.Join(l.dir, id+".log")
	q := map[string]any{"cluster_id": "e2e", "state_dir": n.dir, "listen_addr": n.address, "advertise_addr": n.address, "ca_file": filepath.Join(l.dir, "ca.crt"), "cert_file": filepath.Join(l.dir, "node-"+id+".crt"), "key_file": filepath.Join(l.dir, "node-"+id+".key"), "operation_timeout": "2s", "transfer_timeout": "20s"}
	if len(l.nodes) > 0 {
		q["seeds"] = []cluster.Member{{ID: l.nodes[0].id, Address: l.nodes[0].address}}
	}
	cfg := map[string]any{"node_id": id, "storage_mode": "quorum", "quorum": q, "max_upload_bytes": 8 << 20, "log_level": "warn", "sync_dir": filepath.Join(l.dir, "legacy-"+id), "meta_dir": filepath.Join(l.dir, "legacy-meta-"+id), "multipart": map[string]any{"min_part_bytes": 1024, "max_part_bytes": 8 << 20}, "gateways": map[string]any{"s3": map[string]any{"enabled": true, "listen_addr": n.s3, "access_key": "access", "secret_key": "secret"}}}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		l.t.Fatal(err)
	}
	if err = os.WriteFile(n.config, b, 0600); err != nil {
		l.t.Fatal(err)
	}
	l.nodes = append(l.nodes, n)
	l.start(n, len(l.nodes) == 1)
	return n
}
func (l *daemonLab) start(n *daemonProcess, bootstrap bool) {
	l.t.Helper()
	binary, err := os.Executable()
	if err != nil {
		l.t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestQuorumDaemonHelper$")
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "BIRAK_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "BIRAK_TEST_DAEMON_CONFIG="+n.config)
	if bootstrap {
		cmd.Env = append(cmd.Env, "BIRAK_TEST_BOOTSTRAP=1")
	}
	f, err := os.OpenFile(n.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		l.t.Fatal(err)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	err = cmd.Start()
	f.Close()
	if err != nil {
		l.t.Fatal(err)
	}
	n.cmd = cmd
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, code := l.adminCall(n, "status", nil); code == 200 {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	l.t.Fatal("daemon did not start", n.id)
}
func (l *daemonLab) kill(n *daemonProcess) {
	if n.cmd != nil {
		n.cmd.Process.Kill()
		n.cmd.Wait()
		n.cmd = nil
	}
}
func (l *daemonLab) adminCall(n *daemonProcess, op string, m *cluster.Member) ([]byte, int) {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: l.admin.ClientTLS(n.id)}, Timeout: 20 * time.Second}
	defer client.CloseIdleConnections()
	method := "GET"
	var body io.Reader
	if m != nil {
		method = "POST"
		b, _ := json.Marshal(m)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+n.address+"/v1/admin/"+op, body)
	r, err := client.Do(req)
	if err != nil {
		return []byte(err.Error()), 0
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return b, r.StatusCode
}
func (l *daemonLab) leader() *daemonProcess {
	l.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range l.nodes {
			if n.cmd == nil {
				continue
			}
			b, code := l.adminCall(n, "status", nil)
			var s quorum.Status
			if code == 200 && json.Unmarshal(b, &s) == nil && s.State == "Leader" {
				if _, code := l.adminCall(n, "ready", nil); code == 200 {
					return n
				}
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	l.t.Fatal("no ready daemon leader")
	return nil
}
func (l *daemonLab) change(leader, member *daemonProcess, op string) {
	l.t.Helper()
	if b, code := l.adminCall(leader, op, &cluster.Member{ID: member.id, Address: member.address}); code != 200 {
		l.t.Fatalf("%s %s: %d %s", op, member.id, code, b)
	}
}
func (l *daemonLab) request(n *daemonProcess, method, path string, body []byte, headers map[string]string) (int, []byte, http.Header, error) {
	req, err := http.NewRequest(method, "http://"+n.s3+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Independent SigV4 signer: no production S3 signing helper is used.
	hash := sha256.Sum256(body)
	payload := hex.EncodeToString(hash[:])
	if v := req.Header.Get("X-Amz-Content-Sha256"); v != "" {
		payload = v
	}
	now := time.Now().UTC()
	date := now.Format("20060102")
	amz := now.Format("20060102T150405Z")
	scope := date + "/us-east-1/s3/aws4_request"
	req.Header.Set("X-Amz-Date", amz)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	query := strings.ReplaceAll(req.URL.Query().Encode(), "+", "%20")
	canonical := method + "\n" + req.URL.EscapedPath() + "\n" + query + "\nhost:" + req.URL.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + amz + "\n\nhost;x-amz-content-sha256;x-amz-date\n" + payload
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amz + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	mac := func(key []byte, value string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(value))
		return h.Sum(nil)
	}
	key := mac([]byte("AWS4secret"), date)
	key = mac(key, "us-east-1")
	key = mac(key, "s3")
	key = mac(key, "aws4_request")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=access/"+scope+", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+hex.EncodeToString(mac(key, toSign)))
	r, err := l.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	return r.StatusCode, b, r.Header, err
}
func (l *daemonLab) expect(n *daemonProcess, method, path string, body []byte, headers map[string]string, want int) ([]byte, http.Header) {
	l.t.Helper()
	code, b, h, err := l.request(n, method, path, body, headers)
	if err != nil || code != want {
		l.t.Fatalf("%s %s via %s: status %d want %d, %s (%v)", method, path, n.id, code, want, b, err)
	}
	return b, h
}

func TestQuorumDaemonS3SurvivesProcessLoss(t *testing.T) {
	l := newDaemonLab(t)
	n1 := l.add()
	l.leader()
	l.expect(n1, "PUT", "/apk", nil, nil, 200)
	l.expect(n1, "PUT", "/empty", nil, nil, 200)
	l.expect(n1, "DELETE", "/empty?lifecycle", nil, nil, 501)
	l.expect(n1, "HEAD", "/empty", nil, nil, 200)
	l.expect(n1, "DELETE", "/empty", nil, nil, 204)
	for _, key := range []string{"index.html", "a/../opaque", "repeated//slash"} {
		l.expect(n1, "PUT", "/apk/"+key, []byte(key), nil, 200)
		b, _ := l.expect(n1, "GET", "/apk/"+key, nil, nil, 200)
		if string(b) != key {
			t.Fatal("key canonicalized", key, string(b))
		}
	}
	n2 := l.add()
	l.expect(n2, "PUT", "/apk/learner", []byte("unsafe"), nil, 503)
	l.change(n1, n2, "join")
	// Catch-up and ordinary signed S3 writes share the live cluster.
	done := make(chan int, 1)
	go func() { _, code := l.adminCall(n1, "catch-up", &cluster.Member{ID: n2.id}); done <- code }()
	for i := 0; i < 8; i++ {
		l.expect(n1, "PUT", fmt.Sprintf("/apk/join-%d", i), []byte(fmt.Sprint(i)), nil, 200)
	}
	if code := <-done; code != 200 {
		t.Fatal("concurrent catch-up failed", code)
	}
	l.change(n1, n2, "promote")
	n3 := l.add()
	for _, op := range []string{"join", "catch-up", "promote"} {
		l.change(n1, n3, op)
	}
	for i := 0; i < 8; i++ {
		b, _ := l.expect(n2, "GET", fmt.Sprintf("/apk/join-%d", i), nil, nil, 200)
		if string(b) != fmt.Sprint(i) {
			t.Fatal("lost acknowledged join write")
		}
	}
	init, _ := l.expect(n2, "POST", "/apk/multipart?uploads", nil, nil, 200)
	var upload struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(init, &upload); err != nil || upload.UploadID == "" {
		t.Fatal(string(init), err)
	}
	parts := [][]byte{bytes.Repeat([]byte("A"), 8192), bytes.Repeat([]byte("B"), 4096), bytes.Repeat([]byte("C"), 33)}
	tags := make([]string, len(parts))
	errs := make(chan error, len(parts))
	var wg sync.WaitGroup
	for i, part := range parts {
		wg.Add(1)
		go func(i int, part []byte) {
			defer wg.Done()
			code, b, h, err := l.request(n2, "PUT", fmt.Sprintf("/apk/multipart?uploadId=%s&partNumber=%d", upload.UploadID, i+1), part, nil)
			if err != nil || code != 200 {
				errs <- fmt.Errorf("part %d: %d %s %v", i, code, b, err)
				return
			}
			tags[i] = h.Get("ETag")
		}(i, part)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// SIGKILL the leader after all part acknowledgements, before completion.
	l.kill(n1)
	leader := l.leader()
	entry := n2
	if leader == n2 {
		entry = n3
	}
	complete := "<CompleteMultipartUpload>"
	for i, tag := range tags {
		complete += fmt.Sprintf("<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, tag)
	}
	complete += "</CompleteMultipartUpload>"
	path := "/apk/multipart?uploadId=" + upload.UploadID
	l.expect(entry, "POST", path, []byte(complete), nil, 200)
	got, h := l.expect(entry, "GET", "/apk/multipart", nil, nil, 200)
	if !bytes.Equal(got, bytes.Join(parts, nil)) || !strings.HasSuffix(h.Get("ETag"), "-3\"") {
		t.Fatal("multipart lost after leader death", h)
	}
	l.expect(entry, "PUT", "/apk/multipart", []byte("newer"), nil, 200)
	l.expect(entry, "POST", path, []byte(strings.ReplaceAll(complete, "><", ">\n<")), nil, 200)
	got, _ = l.expect(entry, "GET", "/apk/multipart", nil, nil, 200)
	if string(got) != "newer" {
		t.Fatal("completion retry overwrote newer version")
	}
	l.expect(entry, "DELETE", "/apk/multipart", nil, nil, 204)
	l.expect(entry, "POST", path, []byte(complete), nil, 200)
	l.expect(entry, "GET", "/apk/multipart", nil, nil, 404)
	l.expect(entry, "PUT", "/apk/bad", []byte("tampered"), map[string]string{"X-Amz-Content-Sha256": strings.Repeat("0", 64)}, 400)
	l.expect(entry, "GET", "/apk/bad", nil, nil, 404)
	l.expect(entry, "POST", "/apk?delete", []byte("<Delete><Object><Key>index.html</Key></Object></Delete>"), map[string]string{"X-Amz-Content-Sha256": strings.Repeat("0", 64)}, 400)
	l.expect(entry, "GET", "/apk/index.html", nil, nil, 200)
	l.expect(entry, "PUT", "/apk/conditional", []byte("first"), map[string]string{"If-None-Match": "*"}, 200)
	l.expect(entry, "PUT", "/apk/conditional", []byte("second"), map[string]string{"If-None-Match": "*"}, 412)
	l.expect(entry, "GET", "/apk/index.html", nil, map[string]string{"Range": "bytes=0-4"}, 206)
	// Lose the second voter: membership must not silently shrink to 1/1.
	l.kill(entry)
	l.expect(leader, "PUT", "/apk/no-quorum", []byte("must not ACK"), nil, 503)
	l.expect(leader, "GET", "/apk/conditional", nil, nil, 503)
	l.start(n1, false)
	leader = l.leader()
	l.expect(leader, "GET", "/apk/conditional", nil, nil, 200)
	l.start(entry, false)
	leader = l.leader()
	l.change(leader, leader, "snapshot")
	// Full cluster restart preserves committed objects, tombstones and receipts.
	for _, n := range l.nodes {
		l.kill(n)
	}
	for _, n := range l.nodes {
		l.start(n, false)
	}
	leader = l.leader()
	l.expect(leader, "GET", "/apk/multipart", nil, nil, 404)
	l.expect(leader, "POST", path, []byte(complete), nil, 200)
	l.expect(leader, "GET", "/apk/multipart", nil, nil, 404)
	for _, n := range l.nodes {
		if _, err := os.Stat(filepath.Join(l.dir, "legacy-"+n.id)); !os.IsNotExist(err) {
			t.Fatal("filesystem path initialized in quorum mode", err)
		}
	}
}
