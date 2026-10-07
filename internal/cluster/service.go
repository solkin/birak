package cluster

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/birak/birak/internal/generation"
	"github.com/birak/birak/internal/quorum"
	"github.com/hashicorp/raft"
)

type Member struct {
	Grace   time.Duration `json:"grace,omitempty" yaml:"-"`
	ID      string        `json:"id" yaml:"id"`
	Address string        `json:"address" yaml:"address"`
}
type Options struct {
	BackupTimeout                       time.Duration
	Dir, ID, Cluster, Listen, Advertise string
	Bootstrap                           bool
	Seeds                               []Member
	Credentials                         *Credentials
	OperationTimeout                    time.Duration
	TransferTimeout                     time.Duration
	MaxBlobBytes                        int64
	RaftConfig                          *raft.Config
}

type Service struct {
	options    Options
	Transport  *Transport
	node       atomic.Pointer[quorum.Node]
	server     *http.Server
	listener   net.Listener
	mu         sync.RWMutex
	candidates map[raft.ServerID]raft.ServerAddress
	s3         http.Handler
	done       chan error
	admission  sync.Mutex
	transfers  chan struct{}
}

func Open(o Options) (_ *Service, err error) {
	if !ValidID(o.ID) || !ValidID(o.Cluster) || o.Credentials == nil || o.Credentials.Principal != (Principal{Cluster: o.Cluster, Role: "node", ID: o.ID}) {
		return nil, errors.New("invalid cluster service identity")
	}
	if o.BackupTimeout <= 0 {
		o.BackupTimeout = 168 * time.Hour
	}
	if o.OperationTimeout <= 0 {
		o.OperationTimeout = 2 * time.Minute
	}
	if o.TransferTimeout <= 0 {
		o.TransferTimeout = 30 * time.Minute
	}
	if o.MaxBlobBytes <= 0 {
		return nil, errors.New("max blob bytes must be positive")
	}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return nil, err
	}
	address := o.Advertise
	if address == "" {
		address = ln.Addr().String()
	}
	if _, _, err = net.SplitHostPort(address); err != nil {
		ln.Close()
		return nil, err
	}
	s := &Service{options: o, listener: ln, transfers: make(chan struct{}, 8), candidates: map[raft.ServerID]raft.ServerAddress{}, done: make(chan error, 1)}
	s.Transport = NewTransport(raft.ServerID(o.ID), raft.ServerAddress(address), o.Credentials, 3*time.Second)
	s.server = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, ReadTimeout: o.TransferTimeout, MaxHeaderBytes: 64 << 10}
	n, err := quorum.Open(quorum.Options{Dir: o.Dir, Identity: quorum.Identity{Cluster: o.Cluster, Node: raft.ServerID(o.ID), Format: quorum.Format}, Bootstrap: o.Bootstrap, Transport: s.Transport, Peers: s, TransferTimeout: o.TransferTimeout, RaftConfig: o.RaftConfig, Timeout: o.OperationTimeout})
	if err != nil {
		s.Transport.Close()
		ln.Close()
		return nil, err
	}
	s.node.Store(n)
	go func() {
		err := s.server.Serve(tls.NewListener(ln, o.Credentials.ServerTLS()))
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.done <- err
	}()
	return s, nil
}

func (s *Service) Node() *quorum.Node         { return s.node.Load() }
func (s *Service) Address() string            { return string(s.Transport.Address) }
func (s *Service) Done() <-chan error         { return s.done }
func (s *Service) SetS3(handler http.Handler) { s.mu.Lock(); s.s3 = handler; s.mu.Unlock() }
func (s *Service) Close(ctx context.Context) error {
	err := s.server.Shutdown(ctx)
	if err != nil {
		s.server.Close()
	}
	err = errors.Join(err, s.Node().Close(), s.Transport.Close())
	return err
}

func (s *Service) peer(id raft.ServerID) (raft.ServerAddress, error) {
	if n := s.node.Load(); n != nil {
		c, err := n.Configuration()
		if err != nil {
			return "", err
		}
		for _, m := range c.Servers {
			if m.ID == id {
				return m.Address, nil
			}
		}
	}
	s.mu.RLock()
	address, ok := s.candidates[id]
	s.mu.RUnlock()
	if ok {
		return address, nil
	}
	for _, m := range s.options.Seeds {
		if raft.ServerID(m.ID) == id {
			return raft.ServerAddress(m.Address), nil
		}
	}
	return "", errors.New("unknown peer")
}

func (s *Service) authorized(p Principal) bool {
	if p.Cluster != s.options.Cluster {
		return false
	}
	if p.Role == "admin" {
		return true
	}
	n := s.node.Load()
	if n == nil {
		return false
	}
	c, err := n.Configuration()
	if err != nil {
		return false
	}
	if len(c.Servers) > 0 {
		for _, m := range c.Servers {
			if string(m.ID) == p.ID {
				return true
			}
		}
		return false
	}
	// A fresh learner accepts only explicitly configured seed identities. Once
	// a configuration is installed it becomes the sole source of node authority.
	for _, m := range s.options.Seeds {
		if m.ID == p.ID {
			return true
		}
	}
	return false
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Birak-Format", quorum.Format)
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", 403)
		return
	}
	cert := r.TLS.PeerCertificates[0]
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		http.Error(w, "expired certificate", 403)
		return
	}
	p, err := CertificatePrincipal(cert)
	if err != nil || !s.authorized(p) {
		http.Error(w, "not admitted", 403)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/admin/") {
		if p.Role != "admin" {
			http.Error(w, "operator certificate required", 403)
			return
		}
		s.admin(w, r)
		return
	}
	if p.Role != "node" {
		http.Error(w, "node certificate required", 403)
		return
	}
	if r.URL.Path != "/v1/identity" && r.Header.Get("X-Birak-Format") != quorum.Format {
		http.Error(w, "wire format mismatch", 409)
		return
	}
	if r.URL.Path == "/v1/collect" && r.Method == "POST" {
		s.collect(w, r, p)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/raft/") {
		s.Transport.serveRPC(w, r, p, strings.TrimPrefix(r.URL.Path, "/v1/raft/"))
		return
	}
	if r.URL.Path == "/v1/identity" && r.Method == "GET" {
		json.NewEncoder(w).Encode(s.Node().Identity())
		return
	}
	if r.URL.Path == "/v1/inventory" && r.Method == "POST" {
		s.inventory(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/blobs/") {
		s.blob(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/s3/") || r.URL.Path == "/s3" {
		status := s.Node().Status()
		voter := false
		for _, m := range status.Members.Servers {
			if string(m.ID) == p.ID && m.Suffrage == raft.Voter {
				voter = true
			}
		}
		if !voter || status.State != "Leader" || !status.Voter {
			http.Error(w, "not a serving voter", 503)
			return
		}
		s.mu.RLock()
		handler := s.s3
		s.mu.RUnlock()
		if handler == nil {
			http.Error(w, "S3 unavailable", 503)
			return
		}
		clone := r.Clone(r.Context())
		u := *r.URL
		u.Path = strings.TrimPrefix(u.Path, "/s3")
		if u.Path == "" {
			u.Path = "/"
		}
		if u.RawPath != "" {
			u.RawPath = strings.TrimPrefix(u.RawPath, "/s3")
		}
		clone.URL = &u
		handler.ServeHTTP(w, clone)
		return
	}
	http.NotFound(w, r)
}

func decodeBody(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func (s *Service) admin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/admin/metrics" && r.Method == "GET" {
		s.metrics(w)
		return
	}
	if r.URL.Path == "/v1/admin/backup" && r.Method == "GET" {
		ctx, cancel := context.WithTimeout(r.Context(), s.options.BackupTimeout)
		defer cancel()
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Trailer", "X-Birak-Backup-Complete")
		if err := s.Node().ExportS3(ctx, w); err == nil {
			w.Header().Set("X-Birak-Backup-Complete", "true")
		}
		return
	}
	if r.URL.Path == "/v1/admin/status" && r.Method == "GET" {
		json.NewEncoder(w).Encode(s.Node().Status())
		return
	}
	if r.URL.Path == "/v1/admin/ready" && r.Method == "GET" {
		if err := s.Node().Ready(r.Context()); err != nil {
			http.Error(w, "not ready", 503)
		}
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var m Member
	if err := decodeBody(r, &m); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if !ValidID(m.ID) {
		http.Error(w, "invalid node ID", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.options.TransferTimeout)
	defer cancel()
	var err error
	switch strings.TrimPrefix(r.URL.Path, "/v1/admin/") {
	case "collect":
		if m.Grace == 0 {
			m.Grace = 24 * time.Hour
		}
		result, e := s.Node().Collect(ctx, m.Grace)
		if e != nil {
			http.Error(w, e.Error(), 503)
			return
		}
		json.NewEncoder(w).Encode(result)
		return
	case "end-collection":
		err = s.Node().EndCollection(ctx)
	case "scrub":
		err = s.Node().Scrub(ctx, 64<<20)
	case "join":
		if _, _, e := net.SplitHostPort(m.Address); e != nil {
			http.Error(w, "invalid address", 400)
			return
		}
		s.admission.Lock()
		defer s.admission.Unlock()
		s.mu.Lock()
		s.candidates[raft.ServerID(m.ID)] = raft.ServerAddress(m.Address)
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.candidates, raft.ServerID(m.ID)); s.mu.Unlock() }()
		err = s.Node().AddLearner(ctx, raft.ServerID(m.ID), raft.ServerAddress(m.Address))
	case "catch-up":
		err = s.Node().CatchUp(ctx, raft.ServerID(m.ID))
	case "promote":
		err = s.Node().Promote(ctx, raft.ServerID(m.ID))
	case "remove":
		err = s.Node().Remove(ctx, raft.ServerID(m.ID))
	case "cancel":
		err = s.Node().CancelChange(ctx, raft.ServerID(m.ID))
	case "snapshot":
		err = s.Node().Snapshot(ctx)
	case "transfer":
		err = s.Node().TransferLeadership(ctx, raft.ServerID(m.ID))
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	json.NewEncoder(w).Encode(s.Node().Status())
}

func (s *Service) Identity(ctx context.Context, id raft.ServerID) (quorum.Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	address, err := s.peer(id)
	if err != nil {
		return quorum.Identity{}, err
	}
	r, err := s.Transport.request(ctx, id, address, "GET", "/v1/identity", nil)
	if err != nil {
		return quorum.Identity{}, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return quorum.Identity{}, errors.New("peer identity refused")
	}
	var identity quorum.Identity
	err = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&identity)
	return identity, err
}
func blobPath(ref generation.Ref) string {
	return "/v1/blobs/" + ref.Hash + "?size=" + strconv.FormatInt(ref.Size, 10)
}
func (s *Service) Receive(ctx context.Context, id raft.ServerID, ref generation.Ref, body io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, s.options.TransferTimeout)
	defer cancel()
	address, err := s.peer(id)
	if err != nil {
		return err
	}
	r, err := s.Transport.request(ctx, id, address, "PUT", blobPath(ref), body)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return errors.New("durable generation transfer refused")
	}
	return nil
}
func (s *Service) Open(ctx context.Context, id raft.ServerID, ref generation.Ref) (io.ReadCloser, error) {
	address, err := s.peer(id)
	if err != nil {
		return nil, err
	}
	r, err := s.Transport.request(ctx, id, address, "GET", blobPath(ref), nil)
	if err != nil {
		return nil, err
	}
	if r.StatusCode != 200 {
		r.Body.Close()
		return nil, errors.New("generation unavailable")
	}
	return r.Body, nil
}
func (s *Service) blob(w http.ResponseWriter, r *http.Request) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		http.Error(w, "transfer capacity exceeded", 503)
		return
	}
	size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	ref := generation.Ref{Hash: strings.TrimPrefix(r.URL.Path, "/v1/blobs/"), Size: size}
	if err != nil || ref.Validate() != nil || size > s.options.MaxBlobBytes {
		http.Error(w, "invalid generation", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.options.TransferTimeout)
	defer cancel()
	switch r.Method {
	case "PUT":
		if err := s.Node().ReceiveBlob(ctx, ref, r.Body); err != nil {
			http.Error(w, "generation not durable", 503)
		}
	case "GET":
		f, err := s.Node().LocalBlob(ctx, ref)
		if err != nil {
			http.Error(w, "generation unavailable", 503)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Length", strconv.FormatInt(ref.Size, 10))
		io.Copy(w, f)
	default:
		http.Error(w, "method", 405)
	}
}

// ForwardS3 preserves the signed host, path, query and body inside the mutually
// authenticated channel. It never redirects client credentials to an endpoint.
func (s *Service) ForwardS3(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.options.TransferTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	status := s.Node().Status()
	if !status.Voter || status.Leader == "" || status.Leader == status.Identity.Node {
		http.Error(w, "no serving leader", 503)
		return
	}
	address, err := s.peer(status.Leader)
	if err != nil {
		http.Error(w, "no leader", 503)
		return
	}
	target := &url.URL{Scheme: "https", Host: string(address)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = "/s3" + req.URL.Path
		if req.URL.RawPath != "" {
			req.URL.RawPath = "/s3" + req.URL.RawPath
		}
		req.Header.Del("X-Forwarded-For")
		req.Header.Set("X-Birak-Format", quorum.Format)
	}
	proxy.Transport = s.Transport.client(status.Leader).Transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { http.Error(w, "leader unavailable", 503) }
	proxy.ServeHTTP(w, r)
}
