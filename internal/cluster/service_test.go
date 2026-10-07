package cluster

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/birak/birak/internal/quorum"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

type testCluster struct {
	t           *testing.T
	dir         string
	nodes       []*Service
	credentials map[string]*Credentials
}

func newTestCluster(t *testing.T) *testCluster {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("strict storage durability not yet validated on NTFS")
	}
	dir := t.TempDir()
	if err := CreateCA(dir, "test"); err != nil {
		t.Fatal(err)
	}
	c := &testCluster{t: t, dir: dir, credentials: map[string]*Credentials{}}
	c.issue("admin", "operator")
	return c
}
func (c *testCluster) issue(role, id string) *Credentials {
	c.t.Helper()
	if err := IssueCertificate(c.dir, Principal{Cluster: "test", Role: role, ID: id}, []string{"127.0.0.1", "localhost"}); err != nil {
		c.t.Fatal(err)
	}
	cred, err := LoadCredentials(filepath.Join(c.dir, "ca.crt"), filepath.Join(c.dir, role+"-"+id+".crt"), filepath.Join(c.dir, role+"-"+id+".key"), Principal{Cluster: "test", Role: role, ID: id})
	if err != nil {
		c.t.Fatal(err)
	}
	c.credentials[id] = cred
	return cred
}
func (c *testCluster) start(id string, bootstrap bool) *Service {
	c.t.Helper()
	cred := c.issue("node", id)
	rcfg := raft.DefaultConfig()
	rcfg.LogOutput = io.Discard
	rcfg.HeartbeatTimeout = 300 * time.Millisecond
	rcfg.ElectionTimeout = 300 * time.Millisecond
	rcfg.LeaderLeaseTimeout = 200 * time.Millisecond
	rcfg.TrailingLogs = 2
	rcfg.CommitTimeout = 5 * time.Millisecond
	var seeds []Member
	if len(c.nodes) > 0 {
		seeds = []Member{{ID: c.nodes[0].options.ID, Address: c.nodes[0].Address()}}
	}
	s, err := Open(Options{Dir: filepath.Join(c.t.TempDir(), "node"), ID: id, Cluster: "test", Listen: "127.0.0.1:0", Credentials: cred, Bootstrap: bootstrap, Seeds: seeds, MaxBlobBytes: 8 << 20, OperationTimeout: 5 * time.Second, TransferTimeout: 10 * time.Second, RaftConfig: rcfg})
	if err != nil {
		c.t.Fatal(err)
	}
	c.nodes = append(c.nodes, s)
	c.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.Close(ctx)
	})
	return s
}
func (c *testCluster) leader(excluded ...string) *Service {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range c.nodes {
			skip := false
			for _, id := range excluded {
				if s.options.ID == id {
					skip = true
				}
			}
			if skip {
				continue
			}
			if s.Node().Status().State == "Leader" {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				err := s.Node().Ready(ctx)
				cancel()
				if err == nil {
					return s
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatal("no authenticated leader")
	return nil
}
func (c *testCluster) client(identity, node string) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: c.credentials[identity].ClientTLS(node)}, Timeout: 15 * time.Second}
}
func adminCall(t *testing.T, client *http.Client, s *Service, op string, m Member) int {
	t.Helper()
	b, _ := json.Marshal(m)
	method := "POST"
	if op == "status" || op == "ready" {
		method = "GET"
	}
	req, err := http.NewRequest(method, "https://"+s.Address()+"/v1/admin/"+op, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 {
		t.Logf("admin %s: %d %s", op, r.StatusCode, body)
	}
	return r.StatusCode
}
func (c *testCluster) join(leader, learner *Service) {
	c.t.Helper()
	client := c.client("operator", leader.options.ID)
	defer client.CloseIdleConnections()
	for _, op := range []string{"join", "catch-up", "promote"} {
		if status := adminCall(c.t, client, leader, op, Member{ID: learner.options.ID, Address: learner.Address()}); status != 200 {
			c.t.Fatalf("%s failed: %d", op, status)
		}
	}
}

func TestAuthenticatedRaftAndGenerationReplication(t *testing.T) {
	c := newTestCluster(t)
	n1 := c.start("n1", true)
	c.leader()
	n2 := c.start("n2", false)
	c.join(n1, n2)
	n3 := c.start("n3", false)
	c.join(n1, n3)
	e, err := n1.Node().Put(context.Background(), "write", "key", strings.NewReader("durable bytes"), 100)
	if err != nil {
		t.Fatal(err)
	}
	copies := 0
	for _, n := range c.nodes {
		f, err := n.Node().LocalBlob(context.Background(), e.Ref)
		if err == nil {
			copies++
			f.Close()
		}
	}
	if copies < 2 {
		t.Fatalf("ACK only %d copies", copies)
	}
	if err := n1.Node().TransferLeadership(context.Background(), "n2"); err != nil {
		t.Fatal(err)
	}
	leader := c.leader()
	_, f, err := leader.Node().Read(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != "durable bytes" {
		t.Fatalf("got %q", b)
	}
}

func TestTLSIdentityRolesAndRevocation(t *testing.T) {
	c := newTestCluster(t)
	n1 := c.start("n1", true)
	c.leader()
	client := c.client("n1", "n1")
	defer client.CloseIdleConnections()
	if status := adminCall(t, client, n1, "status", Member{}); status != 403 {
		t.Fatal("node gained operator access", status)
	}
	c.issue("node", "outsider")
	outsider := c.client("outsider", "n1")
	defer outsider.CloseIdleConnections()
	r, err := outsider.Get("https://" + n1.Address() + "/v1/identity")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatal("nonmember admitted", r.StatusCode)
	}
	wrongServer := c.client("operator", "outsider")
	defer wrongServer.CloseIdleConnections()
	if r, err := wrongServer.Get("https://" + n1.Address() + "/v1/admin/status"); err == nil {
		r.Body.Close()
		t.Fatal("accepted wrong server identity")
	}
	noCertificate := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: c.credentials["operator"].Roots, MinVersion: tls.VersionTLS13}}, Timeout: time.Second}
	defer noCertificate.CloseIdleConnections()
	if r, err := noCertificate.Get("https://" + n1.Address() + "/v1/admin/status"); err == nil {
		r.Body.Close()
		t.Fatal("accepted unauthenticated TLS")
	}
	spoof := &raft.RequestVoteRequest{RPCHeader: raft.RPCHeader{ID: []byte("outsider"), ProtocolVersion: raft.ProtocolVersionMax}}
	if err := n1.Transport.RequestVote("n1", raft.ServerAddress(n1.Address()), spoof, &raft.RequestVoteResponse{}); err == nil {
		t.Fatal("accepted forged Raft sender")
	}
	n2 := c.start("n2", false)
	c.join(n1, n2)
	n3 := c.start("n3", false)
	c.join(n1, n3)
	former := c.client("n2", "n1")
	defer former.CloseIdleConnections()
	r, err = former.Get("https://" + n1.Address() + "/v1/identity")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	operator := c.client("operator", "n1")
	defer operator.CloseIdleConnections()
	if status := adminCall(t, operator, n1, "remove", Member{ID: "n2"}); status != 200 {
		t.Fatal(status)
	}
	r, err = former.Get("https://" + n1.Address() + "/v1/identity")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatal("revoked keep-alive still authorized", r.StatusCode)
	}
}

func TestAuthenticatedSnapshotInstallsOnFreshLearner(t *testing.T) {
	c := newTestCluster(t)
	n1 := c.start("n1", true)
	c.leader()
	for i := 0; i < 12; i++ {
		if _, err := n1.Node().Transact(context.Background(), fmt.Sprint(i), []quorum.Change{{Key: fmt.Sprint(i), MetaOnly: true, Attributes: quorum.Attributes{Value: "metadata"}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := n1.Node().Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	n2 := c.start("n2", false)
	c.join(n1, n2)
	if err := n1.Node().TransferLeadership(context.Background(), "n2"); err != nil {
		t.Fatal(err)
	}
	leader := c.leader()
	records, err := leader.Node().View(context.Background(), "")
	if err != nil || len(records) != 12 {
		t.Fatal("snapshot lost metadata", len(records), err)
	}
}
