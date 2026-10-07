package quorum

// This deliberately test-only, loopback HTTP adapter drives real separate
// processes and TCP Raft links. It is not a production transport or public API.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

type httpPeers struct {
	mu     sync.RWMutex
	urls   map[raft.ServerID]string
	client *http.Client
}

func (p *httpPeers) request(ctx context.Context, id raft.ServerID, method, path string, body io.Reader) (*http.Response, error) {
	p.mu.RLock()
	base := p.urls[id]
	p.mu.RUnlock()
	if base == "" {
		return nil, errors.New("unknown test peer")
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, fmt.Errorf("peer HTTP %d", res.StatusCode)
	}
	return res, nil
}
func (p *httpPeers) Identity(ctx context.Context, id raft.ServerID) (Identity, error) {
	r, err := p.request(ctx, id, "GET", "/identity", nil)
	if err != nil {
		return Identity{}, err
	}
	defer r.Body.Close()
	var identity Identity
	err = json.NewDecoder(r.Body).Decode(&identity)
	return identity, err
}
func (p *httpPeers) Receive(ctx context.Context, id raft.ServerID, ref generation.Ref, body io.Reader) error {
	r, err := p.request(ctx, id, "PUT", "/blob/"+ref.Hash+"?size="+strconv.FormatInt(ref.Size, 10), body)
	if err != nil {
		return err
	}
	return r.Body.Close()
}
func (p *httpPeers) Open(ctx context.Context, id raft.ServerID, ref generation.Ref) (io.ReadCloser, error) {
	r, err := p.request(ctx, id, "GET", "/blob/"+ref.Hash+"?size="+strconv.FormatInt(ref.Size, 10), nil)
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}

type childConfig struct {
	ID                 raft.ServerID
	Dir                string
	Bootstrap          bool
	RaftAddr, HTTPAddr string
}
type childReady struct{ RaftAddr, HTTPAddr string }

func TestQuorumChild(t *testing.T) {
	encoded := os.Getenv("BIRAK_QUORUM_TEST_CHILD")
	if encoded == "" {
		return
	}
	var cfg childConfig
	if err := json.Unmarshal([]byte(encoded), &cfg); err != nil {
		t.Fatal(err)
	}
	tr, err := raft.NewTCPTransport(cfg.RaftAddr, nil, 3, time.Second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	peers := &httpPeers{urls: map[raft.ServerID]string{}, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	rcfg := raft.DefaultConfig()
	rcfg.LogOutput = io.Discard
	rcfg.HeartbeatTimeout = 300 * time.Millisecond
	rcfg.ElectionTimeout = 300 * time.Millisecond
	rcfg.LeaderLeaseTimeout = 200 * time.Millisecond
	rcfg.CommitTimeout = 10 * time.Millisecond
	n, err := Open(Options{Dir: cfg.Dir, Identity: Identity{Cluster: "process-test", Node: cfg.ID, Format: Format}, Bootstrap: cfg.Bootstrap, Transport: tr, Peers: peers, RaftConfig: rcfg, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/identity", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(n.id) })
	mux.HandleFunc("/peers", func(w http.ResponseWriter, r *http.Request) {
		var urls map[raft.ServerID]string
		if err := json.NewDecoder(r.Body).Decode(&urls); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		peers.mu.Lock()
		peers.urls = urls
		peers.mu.Unlock()
	})
	mux.HandleFunc("/blob/", func(w http.ResponseWriter, r *http.Request) {
		size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		ref := generation.Ref{Hash: strings.TrimPrefix(r.URL.Path, "/blob/"), Size: size}
		if r.Method == "PUT" {
			if err = n.objects.Receive(r.Context(), ref, r.Body); err != nil {
				http.Error(w, err.Error(), 503)
			}
			return
		}
		f, err := n.objects.Open(r.Context(), ref)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		defer f.Close()
		io.Copy(w, f)
	})
	mux.HandleFunc("/put", func(w http.ResponseWriter, r *http.Request) {
		e, err := n.Put(r.Context(), r.URL.Query().Get("id"), r.URL.Query().Get("key"), r.Body, 1<<20)
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		json.NewEncoder(w).Encode(e)
	})
	mux.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		_, f, err := n.Read(r.Context(), r.URL.Query().Get("key"))
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		defer f.Close()
		io.Copy(w, f)
	})
	mux.HandleFunc("/delete", func(w http.ResponseWriter, r *http.Request) {
		_, err := n.Delete(r.Context(), r.URL.Query().Get("id"), r.URL.Query().Get("key"))
		if err != nil {
			http.Error(w, err.Error(), 503)
		}
	})
	mux.HandleFunc("/join", func(w http.ResponseWriter, r *http.Request) {
		id := raft.ServerID(r.URL.Query().Get("id"))
		err := n.AddLearner(r.Context(), id, raft.ServerAddress(r.URL.Query().Get("address")))
		if err == nil {
			err = n.Promote(r.Context(), id)
		}
		if err != nil {
			http.Error(w, err.Error(), 503)
		}
	})
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if err := n.Snapshot(r.Context()); err != nil {
			http.Error(w, err.Error(), 503)
		}
	})
	mux.HandleFunc("/leader", func(w http.ResponseWriter, r *http.Request) {
		if err := n.barrier(r.Context()); err != nil {
			http.Error(w, err.Error(), 503)
		}
	})
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go http.Serve(listener, mux)
	if err := json.NewEncoder(os.Stdout).Encode(childReady{RaftAddr: string(tr.LocalAddr()), HTTPAddr: listener.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	select {}
}

type child struct {
	cmd    *exec.Cmd
	cfg    childConfig
	ready  childReady
	stderr bytes.Buffer
}

func startChild(t *testing.T, cfg childConfig) *child {
	t.Helper()
	c := &child{cfg: cfg}
	b, _ := json.Marshal(cfg)
	c.cmd = exec.Command(os.Args[0], "-test.run=^TestQuorumChild$", "-test.timeout=120s")
	c.cmd.Env = append(os.Environ(), "BIRAK_QUORUM_TEST_CHILD="+string(b))
	c.cmd.Stderr = &c.stderr
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.kill(t) })
	done := make(chan error, 1)
	go func() { done <- json.NewDecoder(stdout).Decode(&c.ready) }()
	select {
	case err := <-done:
		if err != nil {
			c.kill(t)
			t.Fatalf("child start: %v\n%s", err, c.stderr.String())
		}
	case <-time.After(15 * time.Second):
		c.kill(t)
		t.Fatal("child startup timeout")
	}
	c.cfg.Bootstrap = false
	c.cfg.RaftAddr = c.ready.RaftAddr
	c.cfg.HTTPAddr = c.ready.HTTPAddr
	return c
}
func (c *child) kill(t *testing.T) {
	t.Helper()
	if c.cmd.ProcessState != nil {
		return
	}
	c.cmd.Process.Kill()
	c.cmd.Wait()
	if strings.Contains(c.stderr.String(), "DATA RACE") {
		t.Fatal(c.stderr.String())
	}
}
func callChild(c *child, method, path string, body io.Reader) ([]byte, error) {
	client := http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest(method, "http://"+c.ready.HTTPAddr+path, body)
	if err != nil {
		return nil, err
	}
	r, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err == nil && r.StatusCode != 200 {
		err = fmt.Errorf("HTTP %d: %s", r.StatusCode, b)
	}
	return b, err
}
func childCall(t *testing.T, c *child, method, path, data string) []byte {
	t.Helper()
	b, err := callChild(c, method, path, strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func childLeader(t *testing.T, children ...*child) *child {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range children {
			if c.cmd.ProcessState == nil {
				if _, err := callChild(c, "GET", "/leader", nil); err == nil {
					return c
				}
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("process cluster has no leader")
	return nil
}
func connectChildren(t *testing.T, children []*child) {
	t.Helper()
	urls := map[raft.ServerID]string{}
	for _, c := range children {
		urls[c.cfg.ID] = "http://" + c.ready.HTTPAddr
	}
	b, _ := json.Marshal(urls)
	for _, c := range children {
		childCall(t, c, "POST", "/peers", string(b))
	}
}

func TestProcessSIGKILLAfterACKAndFullRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("strict directory durability not yet supported")
	}
	var children []*child
	for i := 1; i <= 3; i++ {
		children = append(children, startChild(t, childConfig{ID: raft.ServerID(fmt.Sprint("n", i)), Dir: filepath.Join(t.TempDir(), "node"), Bootstrap: i == 1, RaftAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"}))
	}
	connectChildren(t, children)
	leader := childLeader(t, children...)
	for _, c := range children[1:] {
		childCall(t, leader, "POST", "/join?id="+string(c.cfg.ID)+"&address="+c.ready.RaftAddr, "")
	}
	childCall(t, leader, "PUT", "/put?id=first&key=key", "acknowledged bytes")
	leader.kill(t)
	leader = childLeader(t, children...)
	if b := childCall(t, leader, "GET", "/read?key=key", ""); string(b) != "acknowledged bytes" {
		t.Fatalf("lost ACK: %q", b)
	}
	childCall(t, leader, "PUT", "/put?id=second&key=key", "replacement")
	childCall(t, leader, "POST", "/delete?id=tombstone&key=deleted", "")
	childCall(t, leader, "POST", "/snapshot", "")
	for _, c := range children {
		c.kill(t)
	}
	for i, c := range children {
		children[i] = startChild(t, c.cfg)
	}
	connectChildren(t, children)
	leader = childLeader(t, children...)
	if b := childCall(t, leader, "GET", "/read?key=key", ""); string(b) != "replacement" {
		t.Fatalf("restart lost latest version: %q", b)
	}
	// An old successful operation retry must not republish its old generation.
	childCall(t, leader, "PUT", "/put?id=first&key=key", "acknowledged bytes")
	if b := childCall(t, leader, "GET", "/read?key=key", ""); string(b) != "replacement" {
		t.Fatalf("retry resurrected old version: %q", b)
	}
	if _, err := callChild(leader, "GET", "/read?key=deleted", nil); err == nil {
		t.Fatal("deleted key resurrected")
	}
}
