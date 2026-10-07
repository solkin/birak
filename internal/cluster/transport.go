package cluster

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

const maxRPC = 8 << 20

// Transport sends bounded metadata RPCs and streaming snapshots over mTLS.
// The authenticated sender must match each Raft RPCHeader.ID. Authorization is
// checked per request, so removing a member revokes existing keep-alive sessions.
type Transport struct {
	ID          raft.ServerID
	Address     raft.ServerAddress
	Credentials *Credentials
	Timeout     time.Duration
	consumer    chan raft.RPC
	closed      chan struct{}
	once        sync.Once
	mu          sync.Mutex
	clients     map[raft.ServerID]*http.Client
	heartbeat   func(raft.RPC)
}

func NewTransport(id raft.ServerID, address raft.ServerAddress, credentials *Credentials, timeout time.Duration) *Transport {
	return &Transport{ID: id, Address: address, Credentials: credentials, Timeout: timeout, consumer: make(chan raft.RPC, 64), closed: make(chan struct{}), clients: make(map[raft.ServerID]*http.Client)}
}
func (t *Transport) Consumer() <-chan raft.RPC                               { return t.consumer }
func (t *Transport) LocalAddr() raft.ServerAddress                           { return t.Address }
func (t *Transport) EncodePeer(_ raft.ServerID, a raft.ServerAddress) []byte { return []byte(a) }
func (t *Transport) DecodePeer(b []byte) raft.ServerAddress                  { return raft.ServerAddress(b) }
func (t *Transport) AppendEntriesPipeline(raft.ServerID, raft.ServerAddress) (raft.AppendPipeline, error) {
	return nil, raft.ErrPipelineReplicationNotSupported
}
func (t *Transport) SetHeartbeatHandler(fn func(raft.RPC)) {
	t.mu.Lock()
	t.heartbeat = fn
	t.mu.Unlock()
}
func (t *Transport) Close() error {
	t.once.Do(func() {
		close(t.closed)
		t.mu.Lock()
		defer t.mu.Unlock()
		for _, c := range t.clients {
			c.CloseIdleConnections()
		}
	})
	return nil
}

func (t *Transport) client(id raft.ServerID) *http.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.clients[id]; c != nil {
		return c
	}
	tr := &http.Transport{TLSClientConfig: t.Credentials.ClientTLS(string(id)), Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 0, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second, ForceAttemptHTTP2: true}
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.clients[id] = c
	return c
}

func (t *Transport) request(ctx context.Context, id raft.ServerID, address raft.ServerAddress, method, path string, body io.Reader) (*http.Response, error) {
	select {
	case <-t.closed:
		return nil, raft.ErrTransportShutdown
	default:
	}
	if _, _, err := net.SplitHostPort(string(address)); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+string(address)+path, body)
	if err != nil {
		return nil, err
	}
	return t.client(id).Do(req)
}

func (t *Transport) call(id raft.ServerID, address raft.ServerAddress, kind string, args, reply any, data io.Reader) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if len(b) > maxRPC {
		return errors.New("RPC too large")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	body := io.MultiReader(bytes.NewReader(size[:]), bytes.NewReader(b))
	timeout := t.Timeout
	if data != nil {
		body = io.MultiReader(body, data)
		timeout = max(timeout, 30*time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	r, err := t.request(ctx, id, address, "POST", "/v1/raft/"+kind, body)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("Raft transport HTTP %d", r.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(r.Body, maxRPC)).Decode(reply); err != nil {
		return err
	}
	// Responses also carry a bound sender identity.
	if header, ok := reply.(interface{ GetRPCHeader() raft.RPCHeader }); ok && string(header.GetRPCHeader().ID) != string(id) {
		return errors.New("Raft response identity mismatch")
	}
	return nil
}
func (t *Transport) AppendEntries(id raft.ServerID, a raft.ServerAddress, q *raft.AppendEntriesRequest, r *raft.AppendEntriesResponse) error {
	return t.call(id, a, "append", q, r, nil)
}
func (t *Transport) RequestVote(id raft.ServerID, a raft.ServerAddress, q *raft.RequestVoteRequest, r *raft.RequestVoteResponse) error {
	return t.call(id, a, "vote", q, r, nil)
}
func (t *Transport) RequestPreVote(id raft.ServerID, a raft.ServerAddress, q *raft.RequestPreVoteRequest, r *raft.RequestPreVoteResponse) error {
	return t.call(id, a, "prevote", q, r, nil)
}
func (t *Transport) TimeoutNow(id raft.ServerID, a raft.ServerAddress, q *raft.TimeoutNowRequest, r *raft.TimeoutNowResponse) error {
	return t.call(id, a, "timeout", q, r, nil)
}
func (t *Transport) InstallSnapshot(id raft.ServerID, a raft.ServerAddress, q *raft.InstallSnapshotRequest, r *raft.InstallSnapshotResponse, data io.Reader) error {
	return t.call(id, a, "snapshot", q, r, data)
}

func (t *Transport) serveRPC(w http.ResponseWriter, r *http.Request, p Principal, kind string) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var command interface{ GetRPCHeader() raft.RPCHeader }
	switch kind {
	case "append":
		command = &raft.AppendEntriesRequest{}
	case "vote":
		command = &raft.RequestVoteRequest{}
	case "prevote":
		command = &raft.RequestPreVoteRequest{}
	case "timeout":
		command = &raft.TimeoutNowRequest{}
	case "snapshot":
		command = &raft.InstallSnapshotRequest{}
	default:
		http.NotFound(w, r)
		return
	}
	var frame [4]byte
	if _, err := io.ReadFull(r.Body, frame[:]); err != nil {
		http.Error(w, "frame", 400)
		return
	}
	n := binary.BigEndian.Uint32(frame[:])
	if n == 0 || n > maxRPC {
		http.Error(w, "frame too large", 413)
		return
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r.Body, b); err != nil {
		http.Error(w, "frame", 400)
		return
	}
	if err := json.Unmarshal(b, command); err != nil {
		http.Error(w, "invalid RPC", 400)
		return
	}
	if string(command.GetRPCHeader().ID) != p.ID {
		http.Error(w, "RPC identity mismatch", 403)
		return
	}
	response := make(chan raft.RPCResponse, 1)
	rpc := raft.RPC{Command: command, RespChan: response}
	if snap, ok := command.(*raft.InstallSnapshotRequest); ok {
		if snap.Size < 0 || snap.Size > 1<<30 {
			http.Error(w, "size", 400)
			return
		}
		rpc.Reader = io.LimitReader(r.Body, snap.Size)
	}
	t.mu.Lock()
	heartbeat := t.heartbeat
	t.mu.Unlock()
	appendRequest, isAppend := command.(*raft.AppendEntriesRequest)
	if isAppend && len(appendRequest.Entries) == 0 && appendRequest.PrevLogEntry == 0 && appendRequest.PrevLogTerm == 0 && appendRequest.LeaderCommitIndex == 0 && heartbeat != nil {
		heartbeat(rpc)
	} else {
		select {
		case t.consumer <- rpc:
		case <-r.Context().Done():
			return
		case <-t.closed:
			http.Error(w, "closed", 503)
			return
		}
	}
	select {
	case result := <-response:
		if result.Error != nil {
			http.Error(w, "Raft request failed", 503)
			return
		}
		// Raft leaves some response headers empty (snapshot/timeout). Bind
		// every response to this server's authenticated transport identity.
		header := raft.RPCHeader{ProtocolVersion: raft.ProtocolVersionMax, ID: []byte(t.ID), Addr: []byte(t.Address)}
		switch reply := result.Response.(type) {
		case *raft.AppendEntriesResponse:
			reply.RPCHeader = header
		case *raft.RequestVoteResponse:
			reply.RPCHeader = header
		case *raft.RequestPreVoteResponse:
			reply.RPCHeader = header
		case *raft.InstallSnapshotResponse:
			reply.RPCHeader = header
		case *raft.TimeoutNowResponse:
			reply.RPCHeader = header
		default:
			http.Error(w, "invalid RPC response", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result.Response)
	case <-r.Context().Done():
	case <-t.closed:
		http.Error(w, "closed", 503)
	}
}
