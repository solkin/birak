// Package httpui implements an HTTP file server with a web-based UI.
// It provides a Material 3 Expressive styled file browser for managing
// files in syncDir through a standard web browser.
package httpui

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

//go:embed index.html
var indexHTML []byte

// Config holds HTTP file server configuration.
type Config struct {
	ListenAddr string `yaml:"listen_addr"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
}

// Gateway implements the HTTP file server with web UI.
type Gateway struct {
	syncDir        string
	ignorePatterns []string
	config         Config
	logger         *slog.Logger
	server         *http.Server
}

// New creates a new HTTP file server Gateway.
func New(syncDir string, ignorePatterns []string, cfg Config, logger *slog.Logger) *Gateway {
	g := &Gateway{
		syncDir:        syncDir,
		ignorePatterns: ignorePatterns,
		config:         cfg,
		logger:         logger.With("gateway", "http"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", g.route)

	g.server = &http.Server{
		Handler: g.logMiddleware(mux),
	}

	return g
}

// Name returns the protocol name.
func (g *Gateway) Name() string { return "http" }

// Start begins serving HTTP requests. Blocks until ctx is cancelled or error.
func (g *Gateway) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", g.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("http file server listen: %w", err)
	}
	g.logger.Info("HTTP file server started", "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		g.server.Close()
	}()

	if err := g.server.Serve(ln); err != http.ErrServerClosed {
		return fmt.Errorf("http file server serve: %w", err)
	}
	return nil
}

// Stop gracefully shuts down the gateway.
func (g *Gateway) Stop(ctx context.Context) error {
	return g.server.Shutdown(ctx)
}

// responseLogger wraps http.ResponseWriter to capture status code and body size.
type responseLogger struct {
	http.ResponseWriter
	status int
	size   int
}

func (r *responseLogger) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseLogger) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.size += n
	return n, err
}

// logMiddleware logs every incoming request.
func (g *Gateway) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rl := &responseLogger{ResponseWriter: w, status: 200}
		next.ServeHTTP(rl, r)
		g.logger.Info("request",
			"method", r.Method,
			"url", r.URL.String(),
			"status", rl.status,
			"size", rl.size,
		)
	})
}

// route dispatches requests between the SPA page and API endpoints.
func (g *Gateway) route(w http.ResponseWriter, r *http.Request) {
	if !g.authenticate(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="birak"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	path := r.URL.Path

	// Favicon — return empty to avoid serving the SPA.
	if path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// API routes under /_api/.
	if strings.HasPrefix(path, "/_api/") {
		g.routeAPI(w, r)
		return
	}

	// Everything else: serve the SPA page.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// routeAPI dispatches API requests.
func (g *Gateway) routeAPI(w http.ResponseWriter, r *http.Request) {
	apiPath := strings.TrimPrefix(r.URL.Path, "/_api")

	switch {
	case apiPath == "/list" && r.Method == http.MethodGet:
		g.handleList(w, r)
	case strings.HasPrefix(apiPath, "/dl/") && r.Method == http.MethodGet:
		g.handleDownload(w, r)
	case apiPath == "/upload" && r.Method == http.MethodPost:
		g.handleUpload(w, r)
	case apiPath == "/mkdir" && r.Method == http.MethodPost:
		g.handleMkdir(w, r)
	case apiPath == "/rename" && r.Method == http.MethodPost:
		g.handleRename(w, r)
	case apiPath == "/delete" && r.Method == http.MethodPost:
		g.handleDelete(w, r)
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// authenticate checks HTTP Basic Auth credentials.
// If no username/password configured, all requests are allowed.
func (g *Gateway) authenticate(r *http.Request) bool {
	if g.config.Username == "" && g.config.Password == "" {
		return true
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return user == g.config.Username && pass == g.config.Password
}
