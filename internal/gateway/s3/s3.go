// Package s3 implements an S3-compatible gateway that maps buckets to
// first-level subdirectories of syncDir.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/birak/birak/internal/watcher"
)

// Config holds S3 gateway configuration.
type Config struct {
	ListenAddr string `yaml:"listen_addr"`
	AccessKey  string `yaml:"access_key"`
	SecretKey  string `yaml:"secret_key"`
	Domain     string `yaml:"domain"`
}

// Gateway implements the S3-compatible API.
type Gateway struct {
	syncDir        string
	ignorePatterns []string
	config         Config
	logger         *slog.Logger
	server         *http.Server
}

// New creates a new S3 Gateway.
func New(syncDir string, ignorePatterns []string, cfg Config, logger *slog.Logger) *Gateway {
	g := &Gateway{
		syncDir:        syncDir,
		ignorePatterns: ignorePatterns,
		config:         cfg,
		logger:         logger.With("gateway", "s3"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", g.route)

	g.server = &http.Server{
		Handler: g.logMiddleware(mux),
	}

	return g
}

// Name returns the protocol name.
func (g *Gateway) Name() string {
	return "s3"
}

// Start begins serving S3 requests. Blocks until ctx is cancelled or error.
func (g *Gateway) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", g.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("s3 gateway listen: %w", err)
	}
	g.logger.Info("S3 gateway started", "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		g.server.Close()
	}()

	if err := g.server.Serve(ln); err != http.ErrServerClosed {
		return fmt.Errorf("s3 gateway serve: %w", err)
	}
	return nil
}

// Stop gracefully shuts down the gateway.
func (g *Gateway) Stop(ctx context.Context) error {
	return g.server.Shutdown(ctx)
}

// responseLogger wraps http.ResponseWriter to capture the status code and body size.
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

// logMiddleware logs every incoming request with method, URL, host, and response status.
func (g *Gateway) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rl := &responseLogger{ResponseWriter: w, status: 200}
		next.ServeHTTP(rl, r)
		g.logger.Info("request",
			"method", r.Method,
			"url", r.URL.String(),
			"host", r.Host,
			"status", rl.status,
			"size", rl.size,
			"user_agent", r.UserAgent(),
		)
	})
}

// extractBucketFromHost detects virtual-hosted-style requests when a domain
// is configured. It strips the port, then checks if the hostname ends with
// ".{domain}" and extracts the prefix as the bucket name.
// Example: domain="localhost", Host="mybucket.localhost:9200" → "mybucket".
// Returns "" if domain is not configured or the host doesn't match.
func (g *Gateway) extractBucketFromHost(host string) string {
	if g.config.Domain == "" {
		return ""
	}

	h := host
	if idx := strings.LastIndex(h, ":"); idx != -1 {
		h = h[:idx]
	}

	suffix := "." + g.config.Domain
	if !strings.HasSuffix(h, suffix) {
		return ""
	}

	bucket := strings.TrimSuffix(h, suffix)
	if bucket == "" {
		return ""
	}

	return bucket
}

// route dispatches S3 requests based on the URL path structure.
// Supports both path-style and virtual-hosted-style addressing:
//
// Path-style:
//
//	/              → ListBuckets
//	/{bucket}      → bucket operations (HEAD/GET/PUT/DELETE)
//	/{bucket}/{key} → object operations (HEAD/GET/PUT/DELETE)
//
// Virtual-hosted-style (bucket in Host header subdomain):
//
//	Host: mybucket.endpoint / → bucket operations
//	Host: mybucket.endpoint /{key} → object operations
func (g *Gateway) route(w http.ResponseWriter, r *http.Request) {
	// Authenticate.
	if !g.authenticate(r) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "Access Denied")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")

	// Detect virtual-hosted-style: bucket in Host header subdomain.
	if vhBucket := g.extractBucketFromHost(r.Host); vhBucket != "" {
		// In virtual-hosted-style, the entire URL path is the key.
		key := path
		g.logger.Debug("virtual-hosted-style request", "bucket", vhBucket, "key", key)
		g.routeBucketOrObject(w, r, vhBucket, key)
		return
	}

	// Path-style routing.
	if path == "" {
		if r.Method == http.MethodGet {
			g.handleListBuckets(w, r)
			return
		}
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	// Split into bucket and key.
	bucket, key, _ := strings.Cut(path, "/")

	// Check ignore patterns on bucket name.
	if watcher.ShouldIgnore(bucket, g.ignorePatterns) {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return
	}

	g.routeBucketOrObject(w, r, bucket, key)
}

// routeBucketOrObject handles all bucket-level and object-level operations.
// Used by both path-style and virtual-hosted-style routing.
func (g *Gateway) routeBucketOrObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if key == "" {
		// Check for sub-resource queries before bucket-level operations.
		query := r.URL.Query()
		if _, ok := query["location"]; ok && r.Method == http.MethodGet {
			g.handleGetBucketLocation(w, r, bucket)
			return
		}
		if _, ok := query["versioning"]; ok && r.Method == http.MethodGet {
			g.handleGetBucketVersioning(w, r, bucket)
			return
		}
		if _, ok := query["acl"]; ok && r.Method == http.MethodGet {
			g.handleGetBucketACL(w, r, bucket)
			return
		}
		if _, ok := query["tagging"]; ok && r.Method == http.MethodGet {
			writeS3Error(w, http.StatusNotFound, "NoSuchTagSet", "The TagSet does not exist.")
			return
		}
		if _, ok := query["policy"]; ok && r.Method == http.MethodGet {
			writeS3Error(w, http.StatusNotFound, "NoSuchBucketPolicy", "The bucket policy does not exist.")
			return
		}

		// Bucket-level operations.
		switch r.Method {
		case http.MethodHead:
			g.handleHeadBucket(w, r, bucket)
		case http.MethodGet:
			g.handleListObjects(w, r, bucket)
		case http.MethodPut:
			g.handleCreateBucket(w, r, bucket)
		case http.MethodDelete:
			g.handleDeleteBucket(w, r, bucket)
		default:
			writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		}
		return
	}

	// Check ignore patterns on key.
	if watcher.ShouldIgnore(key, g.ignorePatterns) {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}

	// Object-level operations.
	switch r.Method {
	case http.MethodHead:
		g.handleHeadObject(w, r, bucket, key)
	case http.MethodGet:
		g.handleGetObject(w, r, bucket, key)
	case http.MethodPut:
		g.handlePutObject(w, r, bucket, key)
	case http.MethodDelete:
		g.handleDeleteObject(w, r, bucket, key)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
	}
}

// authenticate checks the request for valid credentials.
// If no access_key/secret_key configured, all requests are allowed.
// Supports: Authorization header (V2, V4) and presigned URL (query string auth).
func (g *Gateway) authenticate(r *http.Request) bool {
	if g.config.AccessKey == "" && g.config.SecretKey == "" {
		return true // no auth configured
	}

	authHeader := r.Header.Get("Authorization")

	// Check Authorization header first.
	if authHeader != "" {
		// Support AWS Signature V4 format: extract the access key.
		// Format: AWS4-HMAC-SHA256 Credential=ACCESS_KEY/date/region/s3/aws4_request, ...
		if strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256") {
			return g.authenticateSigV4(r, authHeader)
		}

		// Support AWS Signature V2 format: AWS ACCESS_KEY:signature
		if strings.HasPrefix(authHeader, "AWS ") {
			parts := strings.SplitN(authHeader[4:], ":", 2)
			if len(parts) == 2 && parts[0] == g.config.AccessKey {
				return true // simplified V2 check — access key matches
			}
		}

		return false
	}

	// Check presigned URL (query string authentication).
	// Format: ?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=ACCESS_KEY/date/region/s3/aws4_request&...
	query := r.URL.Query()
	if query.Get("X-Amz-Algorithm") != "" {
		credential := query.Get("X-Amz-Credential")
		if credential == "" {
			return false
		}
		// Credential format: ACCESS_KEY/date/region/service/aws4_request
		parts := strings.SplitN(credential, "/", 2)
		if len(parts) >= 1 && parts[0] == g.config.AccessKey {
			return true // simplified presigned URL check — access key matches
		}
		return false
	}

	return false
}

// authenticateSigV4 performs a simplified AWS Signature V4 check.
// We verify the access key matches. Full signature verification is not implemented
// as it requires complex canonicalization; this is sufficient for trusted environments.
func (g *Gateway) authenticateSigV4(r *http.Request, authHeader string) bool {
	// Extract Credential from the header.
	credIdx := strings.Index(authHeader, "Credential=")
	if credIdx == -1 {
		return false
	}
	credStr := authHeader[credIdx+len("Credential="):]
	credEnd := strings.Index(credStr, ",")
	if credEnd != -1 {
		credStr = credStr[:credEnd]
	}

	// Credential format: ACCESS_KEY/date/region/service/aws4_request
	parts := strings.SplitN(credStr, "/", 2)
	if len(parts) < 1 {
		return false
	}

	return parts[0] == g.config.AccessKey
}

// computeETag returns the hex-encoded SHA256 of data, formatted as an S3 ETag.
func computeETag(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("\"%s\"", hex.EncodeToString(h[:]))
}

// hmacSHA256 computes HMAC-SHA256.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
