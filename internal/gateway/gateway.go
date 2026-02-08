// Package gateway defines the interface for file access protocols (S3, FTP, WebDAV, etc.).
package gateway

import "context"

// Gateway is an interface that each protocol-specific file access server must implement.
type Gateway interface {
	// Start begins serving requests. Blocks until ctx is cancelled or a fatal error occurs.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the gateway.
	Stop(ctx context.Context) error

	// Name returns the protocol name (e.g. "s3", "ftp", "webdav").
	Name() string
}
