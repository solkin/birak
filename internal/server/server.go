package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

// Server provides the HTTP API for peers to pull changes and files.
type Server struct {
	store          *store.Store
	syncDir        string
	nodeID         string
	ignorePatterns []string
	logger         *slog.Logger
	mux            *http.ServeMux
}

// New creates a new HTTP server.
func New(s *store.Store, syncDir, nodeID string, ignorePatterns []string, logger *slog.Logger) *Server {
	srv := &Server{
		store:          s,
		syncDir:        syncDir,
		nodeID:         nodeID,
		ignorePatterns: ignorePatterns,
		logger:         logger,
		mux:            http.NewServeMux(),
	}
	srv.mux.HandleFunc("GET /changes", srv.handleChanges)
	srv.mux.HandleFunc("GET /files/{name...}", srv.handleFile)
	srv.mux.HandleFunc("GET /status", srv.handleStatus)
	return srv
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
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
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid since value: %v", err), http.StatusBadRequest)
		return
	}

	limit := 1000
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			http.Error(w, "invalid limit value", http.StatusBadRequest)
			return
		}
		if limit > 10000 {
			limit = 10000
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

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(changes); err != nil {
		s.logger.Error("encode changes response failed", "error", err)
	}
}

// handleFile serves a file's raw content from the sync directory.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing file name", http.StatusBadRequest)
		return
	}

	// Prevent path traversal: clean the path and ensure it doesn't escape.
	cleaned := filepath.ToSlash(filepath.Clean(name))
	if strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") || cleaned == ".." {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}

	// Check ignore patterns.
	if watcher.ShouldIgnore(cleaned, s.ignorePatterns) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	fullPath := filepath.Join(s.syncDir, filepath.FromSlash(cleaned))

	// Verify the resolved path is still under syncDir (belt-and-suspenders).
	absSync, _ := filepath.Abs(s.syncDir)
	absFile, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(absFile, absSync+string(filepath.Separator)) && absFile != absSync {
		http.Error(w, "invalid file name", http.StatusBadRequest)
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

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// StatusResponse is the response for /status.
type StatusResponse struct {
	NodeID     string `json:"node_id"`
	MaxVersion int64  `json:"max_version"`
	FileCount  int64  `json:"file_count"`
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

	resp := StatusResponse{
		NodeID:     s.nodeID,
		MaxVersion: maxVer,
		FileCount:  count,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
