// Package webdav implements a WebDAV gateway that exposes syncDir over HTTP.
// Compatible with macOS Finder, Windows Explorer, Linux davfs2, Cyberduck,
// rclone, and other standard WebDAV clients.
package webdav

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/birak/birak/internal/watcher"
)

// Config holds WebDAV gateway configuration.
type Config struct {
	ListenAddr string `yaml:"listen_addr"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
}

// Gateway implements the WebDAV protocol over HTTP.
type Gateway struct {
	syncDir        string
	ignorePatterns []string
	config         Config
	logger         *slog.Logger
	server         *http.Server
}

// New creates a new WebDAV Gateway.
func New(syncDir string, ignorePatterns []string, cfg Config, logger *slog.Logger) *Gateway {
	g := &Gateway{
		syncDir:        syncDir,
		ignorePatterns: ignorePatterns,
		config:         cfg,
		logger:         logger.With("gateway", "webdav"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", g.route)

	g.server = &http.Server{
		Handler: g.logMiddleware(mux),
	}

	return g
}

// Name returns the protocol name.
func (g *Gateway) Name() string { return "webdav" }

// Start begins serving WebDAV requests. Blocks until ctx is cancelled or error.
func (g *Gateway) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", g.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("webdav gateway listen: %w", err)
	}
	g.logger.Info("WebDAV gateway started", "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		g.server.Close()
	}()

	if err := g.server.Serve(ln); err != http.ErrServerClosed {
		return fmt.Errorf("webdav gateway serve: %w", err)
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
			"user_agent", r.UserAgent(),
		)
	})
}

// davMethods lists all supported WebDAV methods.
const davMethods = "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK"

// route dispatches requests by HTTP method.
func (g *Gateway) route(w http.ResponseWriter, r *http.Request) {
	if !g.authenticate(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="birak"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case "OPTIONS":
		g.handleOptions(w, r)
	case "PROPFIND":
		g.handlePropfind(w, r)
	case "PROPPATCH":
		g.handleProppatch(w, r)
	case http.MethodGet:
		g.handleGet(w, r)
	case http.MethodHead:
		g.handleHead(w, r)
	case http.MethodPut:
		g.handlePut(w, r)
	case http.MethodDelete:
		g.handleDelete(w, r)
	case "MKCOL":
		g.handleMkcol(w, r)
	case "MOVE":
		g.handleMove(w, r)
	case "COPY":
		g.handleCopy(w, r)
	case "LOCK":
		g.handleLock(w, r)
	case "UNLOCK":
		g.handleUnlock(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
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

// resolvePath validates a URL path and returns the relative name and full
// filesystem path. Returns an error for paths that escape the sync directory
// or match ignore patterns.
func (g *Gateway) resolvePath(urlPath string) (relName string, fullPath string, err error) {
	cleaned := filepath.ToSlash(filepath.Clean(urlPath))
	cleaned = strings.TrimPrefix(cleaned, "/")

	if cleaned == "." || cleaned == "" {
		return "", g.syncDir, nil
	}

	// Prevent path traversal.
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", "", fmt.Errorf("path traversal")
	}

	// Check ignore patterns on the path and each segment.
	if watcher.ShouldIgnore(cleaned, g.ignorePatterns) {
		return "", "", fmt.Errorf("ignored path")
	}

	full := filepath.Join(g.syncDir, filepath.FromSlash(cleaned))

	// Belt-and-suspenders: ensure resolved path is under syncDir.
	absSync, _ := filepath.Abs(g.syncDir)
	absFull, _ := filepath.Abs(full)
	if !strings.HasPrefix(absFull, absSync+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path traversal")
	}

	return cleaned, full, nil
}

// resolveDestination extracts and validates the Destination header used by
// MOVE and COPY methods.
func (g *Gateway) resolveDestination(r *http.Request) (string, string, error) {
	dest := r.Header.Get("Destination")
	if dest == "" {
		return "", "", fmt.Errorf("missing Destination header")
	}

	u, err := url.Parse(dest)
	if err != nil {
		return "", "", fmt.Errorf("invalid Destination URL: %w", err)
	}

	return g.resolvePath(u.Path)
}

// hrefFromPath builds a URL-encoded href for a PROPFIND response.
func hrefFromPath(relName string, isDir bool) string {
	if relName == "" {
		return "/"
	}
	parts := strings.Split(relName, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	href := "/" + strings.Join(parts, "/")
	if isDir && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	return href
}
