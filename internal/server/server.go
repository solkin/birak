package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// Cluster protocol headers. Peers exchange them on every metadata request so a
// stale cursor or a rebuilt database is detected on the very next poll instead
// of silently hiding changes forever.
const (
	HeaderProtocol  = "X-Birak-Protocol"
	ProtocolVersion = "3"
	// HeaderEpoch carries the responding node's process incarnation.
	HeaderEpoch = "X-Birak-Epoch"
	// HeaderMaxVersion carries the responding node's highest assigned version.
	HeaderMaxVersion = "X-Birak-Max-Version"
	// HeaderNodeID identifies the caller so the callee can track how far that
	// peer has consumed its change stream.
	HeaderNodeID    = "X-Birak-Node-Id"
	HeaderNodeEpoch = "X-Birak-Node-Epoch"
	// HeaderSecret carries the optional cluster shared secret.
	HeaderSecret = "X-Birak-Secret"
	// HeaderMode carries the source file's permission bits on /files responses,
	// so a replica is created with the same mode rather than 0600.
	HeaderMode = "X-Birak-Mode"
)

// StatsProvider exposes live replication state for /status. The syncer
// implements it; a node with no peers may pass nil.
type StatsProvider interface {
	PeerStats() []PeerStatus
}

type localStatsProvider interface {
	ScanStatus() watcher.ScanStatus
	CheckStorage() error
}

// PeerStatus is the per-peer replication state reported by /status. It is what
// makes a stalled peer visible: without it, a halted stream is only detectable
// by manually comparing file counts between nodes.
type PeerStatus struct {
	Peer            string `json:"peer"`
	Cursor          int64  `json:"cursor"`
	PeerMaxVersion  int64  `json:"peer_max_version"`
	Lag             int64  `json:"lag"`
	Epoch           string `json:"epoch"`
	Healthy         bool   `json:"healthy"`
	LastSuccessAgo  int64  `json:"last_success_ms_ago"`
	LastError       string `json:"last_error,omitempty"`
	ConsecutiveErrs int64  `json:"consecutive_errors"`
	Pending         int64  `json:"pending_repairs"`
	LastReconcileMS int64  `json:"last_reconcile_ms_ago"`
}

// Server provides the HTTP API for peers to pull changes and files.
type Server struct {
	store          *store.Store
	syncDir        string
	nodeID         string
	ignorePatterns []string
	secret         string
	stats          StatsProvider
	logger         *slog.Logger
	mux            *http.ServeMux
}

// Config holds the server's optional settings.
type Config struct {
	// Secret, when set, is required in the X-Birak-Secret header of every
	// cluster endpoint. Empty keeps the API open, matching earlier releases.
	Secret string
	// Stats supplies live peer state for /status; may be nil.
	Stats StatsProvider
}

// New creates a new HTTP server.
func New(s *store.Store, syncDir, nodeID string, ignorePatterns []string, cfg Config, logger *slog.Logger) *Server {
	srv := &Server{
		store:          s,
		syncDir:        syncDir,
		nodeID:         nodeID,
		ignorePatterns: ignorePatterns,
		secret:         cfg.Secret,
		stats:          cfg.Stats,
		logger:         logger,
		mux:            http.NewServeMux(),
	}
	srv.mux.HandleFunc("GET /changes", srv.guard(srv.handleChanges))
	srv.mux.HandleFunc("GET /manifest", srv.guard(srv.handleManifest))
	srv.mux.HandleFunc("GET /meta/{name...}", srv.guard(srv.handleMeta))
	srv.mux.HandleFunc("GET /files/{name...}", srv.guard(srv.handleFile))
	srv.mux.HandleFunc("GET /status", srv.guard(srv.handleStatus))
	// Liveness stays unauthenticated so a Kubernetes probe needs no secret.
	srv.mux.HandleFunc("GET /healthz", srv.handleHealthz)
	srv.mux.HandleFunc("GET /readyz", srv.handleReadyz)
	return srv
}

// SetStats attaches the stats provider after construction, which breaks the
// initialization cycle between the server and the syncer.
func (s *Server) SetStats(p StatsProvider) {
	s.stats = p
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// guard enforces the shared secret and stamps every cluster response with this
// node's epoch and max version.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderProtocol, ProtocolVersion)
		if s.secret != "" {
			got := r.Header.Get(HeaderSecret)
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.secret)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Header.Get(HeaderNodeID) != "" && r.Header.Get(HeaderProtocol) != ProtocolVersion {
			http.Error(w, "incompatible replication protocol; upgrade every node", http.StatusUpgradeRequired)
			return
		}
		w.Header().Set(HeaderEpoch, s.store.Incarnation())
		w.Header().Set(HeaderNodeID, s.nodeID)
		if maxVer, err := s.store.MaxVersion(); err == nil {
			w.Header().Set(HeaderMaxVersion, strconv.FormatInt(maxVer, 10))
		}
		next(w, r)
	}
}

// Readiness depends on local storage and a complete successful checksum scan;
// peer outages affect replication health, not process liveness.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	local, ok := s.stats.(localStatsProvider)
	if !ok || local.CheckStorage() != nil || !local.ScanStatus().Ready {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

// handleHealthz reports process liveness. It deliberately does not touch the
// database or the peers: a probe must fail only when this process is unusable,
// not when a peer is down.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, "ok")
}

// handleChanges returns file metadata entries with version > since.
// Query params: since (required), limit (optional, default 1000).
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	sinceStr := r.URL.Query().Get("since")
	if sinceStr == "" {
		http.Error(w, `missing "since" parameter`, http.StatusBadRequest)
		return
	}
	since, err := strconv.ParseInt(sinceStr, 10, 64)
	if err != nil || since < 0 {
		http.Error(w, fmt.Sprintf("invalid since value: %v", err), http.StatusBadRequest)
		return
	}

	limit, ok := parseLimit(w, r, 1000)
	if !ok {
		return
	}

	// Record receipt for diagnostics, including a consumer rewind. This is not
	// proof of application and never authorizes tombstone GC. A cursor for an
	// older source incarnation acknowledges nothing in this incarnation.
	if peer := r.Header.Get(HeaderNodeID); peer != "" {
		ack := since
		if epoch := r.URL.Query().Get("epoch"); epoch != "" && epoch != s.store.Incarnation() {
			ack = 0
		}
		if err := s.store.RecordAckEpoch(peer, r.Header.Get(HeaderNodeEpoch), ack); err != nil {
			s.logger.Warn("record peer ack failed", "peer", peer, "error", err)
		}
	}

	changes, err := s.store.GetChanges(since, limit)
	if err != nil {
		s.logger.Error("get changes failed", "since", since, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if len(changes) > 0 {
		s.logger.Info("serving changes", "since", since, "count", len(changes))
	}

	writeJSON(w, s.logger, changes)
}

// handleManifest returns entries in name order, tombstones included, starting
// after the "after" cursor. Full reconciliation walks it to find divergence the
// version stream cannot express — anything skipped, lost, or written while this
// node was unreachable.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	after := r.URL.Query().Get("after")
	limit, ok := parseLimit(w, r, 1000)
	if !ok {
		return
	}

	entries, err := s.store.ListManifest(after, limit)
	if err != nil {
		s.logger.Error("list manifest failed", "after", after, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.logger, entries)
}

// handleMeta returns the current metadata for a single name. The repair worker
// calls it before retrying so it always targets the peer's newest state instead
// of re-fetching a version that has already been superseded.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cleaned, _, err := gateway.SafePath(s.syncDir, name, s.ignorePatterns)
	if err != nil || cleaned == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	meta, err := s.store.GetFile(cleaned)
	if err != nil {
		s.logger.Error("get file meta failed", "name", cleaned, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if meta == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, s.logger, meta)
}

// handleFile serves a file's raw content from the sync directory.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing file name", http.StatusBadRequest)
		return
	}

	cleaned, fullPath, err := gateway.SafePath(s.syncDir, name, s.ignorePatterns)
	if err != nil || cleaned == "" {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	info, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.Error("stat file failed", "name", cleaned, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "not a file", http.StatusBadRequest)
		return
	}

	s.logger.Debug("serving file", "name", cleaned, "size", info.Size())

	// Open the file ourselves and use http.ServeContent instead of
	// http.ServeFile. ServeFile has built-in behaviour that redirects any
	// URL ending with "/index.html" to "./" (301), which causes the sync
	// client to follow the redirect and hit the directory — returning 400.
	// ServeContent has no such redirect logic and serves the bytes as-is.
	f, err := os.Open(fullPath)
	if err != nil {
		s.logger.Error("open file failed", "name", cleaned, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	// Advertise the source mode so the replica does not inherit the 0600 of the
	// downloader's temp file, which would make it unreadable to other UIDs.
	w.Header().Set(HeaderMode, strconv.FormatUint(uint64(info.Mode().Perm()), 8))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// StatusResponse is the response for /status.
type StatusResponse struct {
	NodeID     string `json:"node_id"`
	Epoch      string `json:"epoch"`
	MaxVersion int64  `json:"max_version"`
	FileCount  int64  `json:"file_count"`

	// Repairs and Peers make replication health observable. A peer whose
	// stream has stalled shows up here rather than only as a file-count drift.
	Repairs store.RepairStats   `json:"repairs"`
	Peers   []PeerStatus        `json:"peers"`
	Local   *watcher.ScanStatus `json:"local,omitempty"`
}

// handleStatus returns daemon health and current state.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	maxVer, err := s.store.MaxVersion()
	if err != nil {
		s.logger.Error("get max version failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	count, err := s.store.FileCount()
	if err != nil {
		s.logger.Error("get file count failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	repairs, err := s.store.RepairQueueStats()
	if err != nil {
		s.logger.Error("get repair stats failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := StatusResponse{
		NodeID:     s.nodeID,
		Epoch:      s.store.Incarnation(),
		MaxVersion: maxVer,
		FileCount:  count,
		Repairs:    repairs,
		Peers:      []PeerStatus{},
	}
	if local, ok := s.stats.(localStatsProvider); ok {
		status := local.ScanStatus()
		if err := local.CheckStorage(); err != nil {
			status.Ready = false
			status.LastError = err.Error()
		}
		resp.Local = &status
	}
	if s.stats != nil {
		if peers := s.stats.PeerStats(); peers != nil {
			resp.Peers = peers
		}
	}

	writeJSON(w, s.logger, resp)
}

// parseLimit reads a bounded "limit" query parameter, writing the error
// response itself and reporting whether the caller may continue.
func parseLimit(w http.ResponseWriter, r *http.Request, def int) (int, bool) {
	limit := def
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid limit value", http.StatusBadRequest)
			return 0, false
		}
		limit = parsed
		if limit > 10000 {
			limit = 10000
		}
	}
	return limit, true
}

func writeJSON(w http.ResponseWriter, logger *slog.Logger, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Error("encode response failed", "error", err)
	}
}
