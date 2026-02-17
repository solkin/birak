# Birak — Distributed File Server

Birak is a distributed file server with built-in replication. Each node stores a full copy of the data and automatically keeps it in sync with other nodes over the network. Files are accessible via S3 API, WebDAV, HTTP file browser, or the local filesystem — use whichever protocol fits your workflow.

## Key Features

- **Multi-protocol access** — S3 API (AWS CLI, SDKs), WebDAV (Finder, Explorer, rclone), HTTP file browser, or direct filesystem access.
- **Automatic replication** — nodes discover changes in real time (fsnotify) and replicate them to all peers over HTTP.
- **Conflict resolution** — newest version wins, verified by SHA256 hash.
- **No single point of failure** — every node is equal; any node can accept reads and writes.
- **Zero external dependencies** — single Go binary, embedded SQLite for metadata.
- **Configurable ignore rules** — glob patterns to exclude files from replication.

## Quick Start

### Build

```bash
go build -o birakd ./cmd/birakd
```

### Configuration

Create a YAML config file for each node:

**node1.yaml**
```yaml
node_id: "node-1"
sync_dir: "/data/node1/sync"
meta_dir: "/data/node1/meta"
listen_addr: ":9100"
peers:
  - "http://192.168.1.2:9100"
  - "http://192.168.1.3:9100"
ignore:
  - ".DS_Store"
  - "Thumbs.db"
  - "*.swp"
sync:
  poll_interval: 3s
  batch_limit: 1000
  max_concurrent_downloads: 5
  tombstone_ttl: 168h       # 7 days
  scan_interval: 5m
  debounce_window: 300ms
```

**node2.yaml**
```yaml
node_id: "node-2"
sync_dir: "/data/node2/sync"
meta_dir: "/data/node2/meta"
listen_addr: ":9100"
peers:
  - "http://192.168.1.1:9100"
  - "http://192.168.1.3:9100"
```

The `ignore` and `sync` sections are optional — defaults will be used if omitted. Internal temp files (`.birak-tmp-*`) are always ignored regardless of configuration.

### Running

```bash
./birakd -config node1.yaml
```

The daemon will create `sync_dir` and `meta_dir` if they don't exist, open a SQLite database at `meta_dir/birak.db`, and start synchronizing.

To stop — send `SIGINT` or `SIGTERM` (Ctrl+C). The daemon will gracefully finish all in-progress operations.

## HTTP API

Three endpoints, easy to test with curl:

### GET /changes?since=N&limit=1000

Returns files with `version > N`. Used by peers for synchronization.

```bash
curl 'http://localhost:9100/changes?since=0&limit=100'
```

```json
[
  {
    "name": "docs/report.txt",
    "mod_time": 1738800000000000000,
    "size": 1024,
    "hash": "a1b2c3d4e5f6...",
    "deleted": false,
    "version": 42
  }
]
```

The `name` field contains a relative path from `sync_dir` (e.g. `docs/report.txt` for a file in a subdirectory).

### GET /files/{name...}

Downloads file contents. Supports nested paths.

```bash
curl -O 'http://localhost:9100/files/report.txt'
curl -O 'http://localhost:9100/files/docs/drafts/spec.pdf'
```

### GET /status

Node status.

```bash
curl 'http://localhost:9100/status'
```

```json
{
  "node_id": "node-1",
  "max_version": 42,
  "file_count": 1500
}
```

## Sync Algorithm

1. Every file change (create, modify, delete) updates the record in the SQLite `files` table with a new monotonic `version`.
2. Peer B polls peer A: `GET /changes?since=<cursor>`.
3. For each file in the response: if the SHA256 differs from the local copy and the incoming `mod_time` is newer — the file is downloaded.
4. The cursor is updated — the next request will only return new changes.
5. Initial sync (including a new peer joining) — the same request with `since=0`.

### Cycle Prevention

When the daemon writes a file received from a peer, it marks it in the in-memory `recentlySynced` set. The watcher sees the fsnotify event but skips the file if the hash matches the expected value. The mark is automatically removed after 5 seconds.

### Deletions

When a file is deleted, a tombstone (`deleted=true`) is created. Peers receive it via `/changes` and delete the file locally. Tombstones are purged after `tombstone_ttl` (default 7 days).

## S3 Gateway

Birak can act as an S3-compatible gateway, providing access to files in `sync_dir` via the standard S3 API.

### Concept

- **Buckets** are top-level directories in `sync_dir`. Bucket `photos` = directory `sync_dir/photos/`.
- **Objects** are files inside buckets. Key `2024/img.jpg` in bucket `photos` = file `sync_dir/photos/2024/img.jpg`.
- Creating/deleting buckets = creating/deleting directories. Buckets are not defined in the config.
- Changes made via the S3 API are picked up by the watcher and synchronized to other nodes.

### Configuration

```yaml
gateways:
  s3:
    enabled: true
    listen_addr: ":9200"
    access_key: "admin"       # optional
    secret_key: "secret123"   # optional
```

If `access_key` and `secret_key` are not set, authentication is disabled.

### Supported Operations

| Operation | Description |
|-----------|-------------|
| `ListBuckets` | List buckets (GET /) |
| `CreateBucket` | Create bucket (PUT /{bucket}) |
| `DeleteBucket` | Delete empty bucket (DELETE /{bucket}) |
| `HeadBucket` | Check bucket existence (HEAD /{bucket}) |
| `ListObjectsV2` | List objects with prefix/delimiter (GET /{bucket}) |
| `PutObject` | Upload file (PUT /{bucket}/{key}) |
| `GetObject` | Download file (GET /{bucket}/{key}) |
| `DeleteObject` | Delete file (DELETE /{bucket}/{key}) |
| `HeadObject` | File metadata (HEAD /{bucket}/{key}) |

### Usage with AWS CLI

```bash
# Configure
aws configure set aws_access_key_id admin
aws configure set aws_secret_access_key secret123

# Create bucket
aws --endpoint-url http://localhost:9200 s3 mb s3://photos

# Upload file
aws --endpoint-url http://localhost:9200 s3 cp image.jpg s3://photos/2024/image.jpg

# Download file
aws --endpoint-url http://localhost:9200 s3 cp s3://photos/2024/image.jpg ./local.jpg

# List
aws --endpoint-url http://localhost:9200 s3 ls s3://photos/

# Delete
aws --endpoint-url http://localhost:9200 s3 rm s3://photos/2024/image.jpg
```

## WebDAV Gateway

Birak can act as a WebDAV server, providing access to files in `sync_dir` via the standard WebDAV protocol. Compatible with macOS Finder, Windows Explorer, Linux davfs2, Cyberduck, rclone, and other WebDAV clients.

### Concept

- WebDAV is an HTTP extension. The WebDAV server root = `sync_dir`.
- Files and directories are accessible directly by path (no bucket concept like in S3).
- Changes made via WebDAV are picked up by the watcher and synchronized to other nodes.
- Locks (LOCK/UNLOCK) are stubs: a fake token is returned, sufficient for Finder and Windows Explorer.

### Configuration

```yaml
gateways:
  webdav:
    enabled: true
    listen_addr: ":9300"
    username: "user"           # optional
    password: "secret123"      # optional
```

If `username` and `password` are not set, authentication is disabled (open access).

### Supported Operations

| Method | Description |
|--------|-------------|
| `OPTIONS` | List supported methods, DAV compliance class 1, 2 |
| `PROPFIND` | Directory listing / file properties (Depth: 0, 1) |
| `PROPPATCH` | Modify properties (stub — acknowledges without persisting) |
| `GET` | Download file |
| `HEAD` | File metadata |
| `PUT` | Upload file (atomic write via temp file) |
| `DELETE` | Delete file or directory |
| `MKCOL` | Create directory |
| `MOVE` | Move / rename |
| `COPY` | Copy file or directory |
| `LOCK` | Lock (stub — fake token) |
| `UNLOCK` | Unlock (stub) |

### Connecting

**macOS Finder:**
1. Finder → Go → Connect to Server (⌘K)
2. Enter: `http://localhost:9300`
3. If authentication is configured — enter username and password

**Linux (davfs2):**
```bash
sudo mount -t davfs http://localhost:9300 /mnt/birak
```

**Cyberduck / rclone:**
```bash
rclone config  # type: webdav, url: http://localhost:9300
rclone ls birak:/
rclone copy localfile.txt birak:/path/to/file.txt
```

## HTTP File Browser

Birak includes a built-in web-based file manager with a Material 3 Expressive UI. Access and manage files directly from any web browser — no client software needed.

### Features

- Browse directories with breadcrumb navigation
- Paginated file listing for large directories
- Upload files (button or drag-and-drop)
- Download, rename, delete files
- Create and delete folders
- Responsive layout for mobile devices

### Configuration

```yaml
gateways:
  http:
    enabled: true
    listen_addr: ":9400"
    username: "user"           # optional
    password: "secret123"      # optional
```

If `username` and `password` are not set, authentication is disabled. When configured, the browser shows a native login prompt (HTTP Basic Auth).

### Usage

Open `http://localhost:9400` in any web browser. All changes made through the file browser are picked up by the watcher and synchronized to other nodes.

## Project Structure

```
birak/
  cmd/birakd/main.go              — entrypoint, CLI, graceful shutdown
  internal/
    config/config.go              — YAML config parsing
    store/store.go                — SQLite: files + cursors tables
    store/store_test.go           — store unit tests
    watcher/watcher.go            — fsnotify + debounce + periodic scan
    watcher/cleanup_test.go       — directory cleanup unit tests
    server/server.go              — HTTP API for peer synchronization
    syncer/syncer.go              — polling, conflict resolution, downloads
    gateway/gateway.go            — Gateway interface
    gateway/s3/s3.go              — S3 Gateway: routing, auth
    gateway/s3/handlers.go        — S3 operation handlers
    gateway/s3/s3_test.go         — S3 Gateway unit tests
    gateway/webdav/webdav.go      — WebDAV Gateway: routing, auth
    gateway/webdav/handlers.go    — WebDAV operation handlers
    gateway/webdav/webdav_test.go — WebDAV Gateway unit tests
    gateway/httpui/httpui.go      — HTTP file browser: routing, auth
    gateway/httpui/handlers.go    — HTTP file browser API handlers
    gateway/httpui/index.html     — Material 3 Expressive SPA (embedded)
    gateway/httpui/httpui_test.go — HTTP file browser unit tests
  integration_test.go             — integration tests (2-3 nodes)
```

## Tests

```bash
# All tests
go test -v -timeout 120s ./...

# Store unit tests only
go test -v ./internal/store/

# S3 Gateway tests only
go test -v ./internal/gateway/s3/

# WebDAV Gateway tests only
go test -v ./internal/gateway/webdav/

# HTTP file browser tests only
go test -v ./internal/gateway/httpui/

# Integration tests only
go test -v -timeout 120s -run TestIntegration
```

Integration tests spin up 2-3 real nodes on localhost with HTTP servers and verify:
- File creation, modification, deletion
- Nested directory synchronization (including deep nesting)
- 3-node full-mesh synchronization
- Conflict resolution (newer wins)
- Initial sync of non-empty directories
- Bulk synchronization (20 files)
- Ignoring files by custom patterns
- File deletion in subdirectories with empty directory cleanup
- Source-node directory cleanup on file deletion

S3 Gateway tests cover:
- Authentication (V2, V4, no auth, invalid keys)
- Bucket operations (create, delete, list, HeadBucket)
- Object operations (PutObject, GetObject, HeadObject, DeleteObject)
- Listing with prefix/delimiter (virtual directories)
- Nested keys, overwrite, empty files, large files
- Ignored files, path traversal protection
- Server lifecycle (Start/Stop)

WebDAV Gateway tests cover:
- Authentication (Basic Auth: no auth, valid/invalid credentials, disabled auth)
- PROPFIND (root, files, Depth: 0/1, ignored file filtering, XML validity)
- File operations (GET, HEAD, PUT new/overwrite, auto-creation of parent directories)
- File and directory deletion, root deletion prevention
- MKCOL (directory creation, duplicates, missing parent)
- MOVE (rename, overwrite, overwrite prevention with Overwrite: F)
- COPY (files, recursive directory copy)
- LOCK/UNLOCK (fake tokens)
- PROPPATCH (stub)
- Correct serving of index.html without redirect
- Path traversal protection
- URL encoding of paths with spaces and special characters

HTTP file browser tests cover:
- Authentication (Basic Auth: required, valid, invalid, disabled)
- SPA page serving (root, any path, favicon)
- Directory listing (root, subdirectory, empty, not found, ignored files filtering, pagination, dirs-before-files ordering)
- File download (simple, nested, not found, directory rejection, path traversal)
- File upload (to root, to subdirectory, ignored file rejection)
- Directory creation (simple, nested, empty path)
- Rename (file, directory, not found)
- Delete (file, directory recursive, root prevention, not found, path traversal)

## Configuration Reference

| Parameter | Default | Description |
|-----------|---------|-------------|
| `node_id` | `node-1` | Unique node ID |
| `sync_dir` | `./sync` | Directory to synchronize |
| `meta_dir` | `./meta` | Directory for SQLite database (metadata) |
| `listen_addr` | `:9100` | HTTP server address |
| `peers` | `[]` | List of peer URLs |
| `ignore` | `[]` _(empty)_ | Glob patterns for ignored files (`.birak-tmp-*` is always ignored internally) |
| `sync.poll_interval` | `3s` | Peer polling interval |
| `sync.batch_limit` | `1000` | Max entries per request |
| `sync.max_concurrent_downloads` | `5` | Concurrent downloads per peer |
| `sync.tombstone_ttl` | `168h` | Tombstone TTL (7 days) |
| `sync.scan_interval` | `5m` | Periodic scan interval |
| `sync.debounce_window` | `300ms` | fsnotify event debounce window |
| `gateways.s3.enabled` | `false` | Enable S3 Gateway |
| `gateways.s3.listen_addr` | `:9200` | S3 Gateway address |
| `gateways.s3.access_key` | _(empty)_ | Access key (optional) |
| `gateways.s3.secret_key` | _(empty)_ | Secret key (optional) |
| `gateways.webdav.enabled` | `false` | Enable WebDAV Gateway |
| `gateways.webdav.listen_addr` | `:9300` | WebDAV Gateway address |
| `gateways.webdav.username` | _(empty)_ | Username (optional) |
| `gateways.webdav.password` | _(empty)_ | Password (optional) |
| `gateways.http.enabled` | `false` | Enable HTTP file browser |
| `gateways.http.listen_addr` | `:9400` | HTTP file browser address |
| `gateways.http.username` | _(empty)_ | Username (optional) |
| `gateways.http.password` | _(empty)_ | Password (optional) |
