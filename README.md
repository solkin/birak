# Birak — Distributed File Server

Birak is a distributed file server with built-in replication. Each node stores a full copy of the data and automatically keeps it in sync with other nodes over the network. Files are accessible via S3 API, WebDAV, SFTP, HTTP file browser, or the local filesystem — use whichever protocol fits your workflow.

## Key Features

- **Multi-protocol access** — S3 API, WebDAV, SFTP, HTTP file browser, or direct filesystem.
- **Automatic replication** — nodes discover changes in real time and replicate them to all peers.
- **Conflict resolution** — newest version wins, verified by SHA256 hash.
- **No single point of failure** — every node is equal; any node can accept reads and writes.
- **Zero external dependencies** — single Go binary, embedded SQLite for metadata.

## How It Works

A Birak cluster consists of one or more **nodes**. Each node has two directories:

- **sync_dir** — the directory where your files live. Any file you put here (or upload via S3/WebDAV/SFTP/browser) gets replicated to all other nodes.
- **meta_dir** — internal directory for the SQLite database that tracks file versions and sync state. You don't need to touch this.

Nodes know about each other through a **peers** list in the config. Each node polls its peers for changes and downloads new or updated files automatically. There is no central server — every node is a full replica.

It doesn't matter how files get into `sync_dir` — you can copy them with `cp`, sync with `rsync`, save from any application, or upload via S3/WebDAV/SFTP/browser. Birak watches the directory for changes and replicates everything to other nodes. Once synced, files are accessible through any of the supported protocols.

## Quick Start

### Docker

The fastest way to try Birak — no config file needed:

```bash
docker run -d \
  -e BIRAK_HTTP_ENABLED=true \
  -v ./sync:/data/sync \
  -v ./meta:/data/meta \
  -p 9100:9100 \
  -p 9400:9400 \
  solkin/birak:latest
```

Open `http://localhost:9400` — you'll see the file browser. Upload a file, and it will appear in the `./sync` directory on your host machine.

### Docker Compose (2-node cluster)

To see replication in action, run two nodes:

```yaml
# docker-compose.yaml
services:
  node1:
    image: solkin/birak:latest
    environment:
      BIRAK_NODE_ID: "node-1"
      BIRAK_PEERS: "http://node2:9100"
      BIRAK_HTTP_ENABLED: "true"
    volumes:
      - node1-sync:/data/sync
      - node1-meta:/data/meta
    ports:
      - "9101:9100"
      - "9401:9400"

  node2:
    image: solkin/birak:latest
    environment:
      BIRAK_NODE_ID: "node-2"
      BIRAK_PEERS: "http://node1:9100"
      BIRAK_HTTP_ENABLED: "true"
    volumes:
      - node2-sync:/data/sync
      - node2-meta:/data/meta
    ports:
      - "9102:9100"
      - "9402:9400"

volumes:
  node1-sync:
  node1-meta:
  node2-sync:
  node2-meta:
```

```bash
docker compose up -d
```

Open `http://localhost:9401` and `http://localhost:9402` — upload a file on one node and watch it appear on the other.

### Build from Source

```bash
go build -o birakd ./cmd/birakd
./birakd -config config.yaml
```

The daemon will create `sync_dir` and `meta_dir` if they don't exist, open a SQLite database at `meta_dir/birak.db`, and start synchronizing.

To stop — send `SIGINT` or `SIGTERM` (Ctrl+C). The daemon will gracefully finish all in-progress operations.

## Configuration

Birak can be configured via a YAML file, environment variables, or both. Environment variables take precedence over the config file. The config file itself is optional — you can run Birak entirely with env vars.

### YAML config file

```bash
./birakd -config config.yaml
```

**Minimal:**
```yaml
node_id: "node-1"
peers:
  - "http://192.168.1.2:9100"
```

**Full example:**
```yaml
node_id: "node-1"
sync_dir: "/data/sync"
meta_dir: "/data/meta"
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
gateways:
  s3:
    enabled: true
    listen_addr: ":9200"
    access_key: "admin"
    secret_key: "secret123"
  webdav:
    enabled: true
    listen_addr: ":9300"
    username: "user"
    password: "secret123"
  http:
    enabled: true
    listen_addr: ":9400"
    username: "user"
    password: "secret123"
  sftp:
    enabled: true
    listen_addr: ":9500"
    username: "user"
    password: "secret123"
```

The `ignore` and `sync` sections are optional — defaults will be used if omitted. Internal temp files (`.birak-tmp-*`) are always ignored regardless of configuration.

### Environment variables

Every setting has a corresponding `BIRAK_*` environment variable. Useful for Docker and CI/CD. List values (peers, ignore) are comma-separated.

```bash
export BIRAK_NODE_ID="node-1"
export BIRAK_PEERS="http://192.168.1.2:9100,http://192.168.1.3:9100"
export BIRAK_HTTP_ENABLED=true
./birakd
```

### Reference

| YAML | Env var | Default | Description |
|------|---------|---------|-------------|
| `node_id` | `BIRAK_NODE_ID` | `node-1` | Unique node ID |
| `sync_dir` | `BIRAK_SYNC_DIR` | `./sync` | Directory to synchronize |
| `meta_dir` | `BIRAK_META_DIR` | `./meta` | Directory for SQLite database |
| `listen_addr` | `BIRAK_LISTEN_ADDR` | `:9100` | Peer-to-peer HTTP server address |
| `peers` | `BIRAK_PEERS` | `[]` | Peer URLs (comma-separated in env) |
| `ignore` | `BIRAK_IGNORE` | `[]` | Ignore patterns (comma-separated in env) |
| `sync.poll_interval` | `BIRAK_SYNC_POLL_INTERVAL` | `3s` | Peer polling interval |
| `sync.batch_limit` | `BIRAK_SYNC_BATCH_LIMIT` | `1000` | Max entries per sync request |
| `sync.max_concurrent_downloads` | `BIRAK_SYNC_MAX_CONCURRENT_DOWNLOADS` | `5` | Concurrent downloads per peer |
| `sync.tombstone_ttl` | `BIRAK_SYNC_TOMBSTONE_TTL` | `168h` | Deleted file record TTL (7 days) |
| `sync.scan_interval` | `BIRAK_SYNC_SCAN_INTERVAL` | `5m` | Full filesystem scan interval |
| `sync.debounce_window` | `BIRAK_SYNC_DEBOUNCE_WINDOW` | `300ms` | Delay before processing file events |
| `gateways.s3.enabled` | `BIRAK_S3_ENABLED` | `false` | Enable S3 Gateway |
| `gateways.s3.listen_addr` | `BIRAK_S3_LISTEN_ADDR` | `:9200` | S3 Gateway address |
| `gateways.s3.access_key` | `BIRAK_S3_ACCESS_KEY` | _(empty)_ | S3 access key |
| `gateways.s3.secret_key` | `BIRAK_S3_SECRET_KEY` | _(empty)_ | S3 secret key |
| `gateways.webdav.enabled` | `BIRAK_WEBDAV_ENABLED` | `false` | Enable WebDAV Gateway |
| `gateways.webdav.listen_addr` | `BIRAK_WEBDAV_LISTEN_ADDR` | `:9300` | WebDAV Gateway address |
| `gateways.webdav.username` | `BIRAK_WEBDAV_USERNAME` | _(empty)_ | WebDAV username |
| `gateways.webdav.password` | `BIRAK_WEBDAV_PASSWORD` | _(empty)_ | WebDAV password |
| `gateways.http.enabled` | `BIRAK_HTTP_ENABLED` | `false` | Enable HTTP file browser |
| `gateways.http.listen_addr` | `BIRAK_HTTP_LISTEN_ADDR` | `:9400` | HTTP file browser address |
| `gateways.http.username` | `BIRAK_HTTP_USERNAME` | _(empty)_ | HTTP username |
| `gateways.http.password` | `BIRAK_HTTP_PASSWORD` | _(empty)_ | HTTP password |
| `gateways.sftp.enabled` | `BIRAK_SFTP_ENABLED` | `false` | Enable SFTP Gateway |
| `gateways.sftp.listen_addr` | `BIRAK_SFTP_LISTEN_ADDR` | `:9500` | SFTP Gateway address |
| `gateways.sftp.username` | `BIRAK_SFTP_USERNAME` | _(empty)_ | SFTP username |
| `gateways.sftp.password` | `BIRAK_SFTP_PASSWORD` | _(empty)_ | SFTP password |
| `gateways.sftp.host_key_path` | `BIRAK_SFTP_HOST_KEY_PATH` | _(auto)_ | Path to SSH host key (auto-generated if empty) |

## Access Protocols

Birak supports multiple ways to work with your files. All protocols operate on the same `sync_dir` — changes made through any protocol are automatically replicated to other nodes.

### HTTP File Browser

A built-in web-based file manager with Material 3 Expressive UI. No client software needed.

- Browse directories with breadcrumb navigation
- Paginated file listing for large directories
- Upload files and folders (button or drag-and-drop)
- Download, rename, delete files
- Create and delete folders

Open `http://localhost:9400` in any browser. If `username` and `password` are configured, the browser shows a native login prompt.

### S3 Gateway

S3-compatible API for use with AWS CLI, SDKs, and any S3 client.

- **Buckets** are top-level directories in `sync_dir`. Bucket `photos` = directory `sync_dir/photos/`.
- **Objects** are files inside buckets. Key `2024/img.jpg` in bucket `photos` = file `sync_dir/photos/2024/img.jpg`.

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

**Usage with AWS CLI:**

```bash
aws configure set aws_access_key_id admin
aws configure set aws_secret_access_key secret123

aws --endpoint-url http://localhost:9200 s3 mb s3://photos
aws --endpoint-url http://localhost:9200 s3 cp image.jpg s3://photos/2024/image.jpg
aws --endpoint-url http://localhost:9200 s3 ls s3://photos/
aws --endpoint-url http://localhost:9200 s3 cp s3://photos/2024/image.jpg ./local.jpg
aws --endpoint-url http://localhost:9200 s3 rm s3://photos/2024/image.jpg
```

### WebDAV Gateway

Standard WebDAV protocol. Compatible with macOS Finder, Windows Explorer, Linux davfs2, Cyberduck, rclone.

| Method | Description |
|--------|-------------|
| `OPTIONS` | Supported methods, DAV compliance class 1, 2 |
| `PROPFIND` | Directory listing / file properties |
| `GET` / `HEAD` | Download file / metadata |
| `PUT` | Upload file (atomic write) |
| `DELETE` | Delete file or directory |
| `MKCOL` | Create directory |
| `MOVE` | Move / rename |
| `COPY` | Copy file or directory |
| `LOCK` / `UNLOCK` | Stub (fake token for client compatibility) |

**Connecting:**

- **macOS Finder:** Go → Connect to Server (Cmd+K) → `http://localhost:9300`
- **Linux:** `sudo mount -t davfs http://localhost:9300 /mnt/birak`
- **rclone:** `rclone config` (type: webdav, url: `http://localhost:9300`)

### SFTP Gateway

Standard SFTP protocol over SSH. Compatible with OpenSSH `sftp`, FileZilla, WinSCP, Cyberduck, and other SFTP clients.

- Browse, upload, download, rename, delete files and directories
- Password authentication (or open access if credentials are omitted)
- SSH host key is auto-generated on first run and persisted in `meta_dir`
- Supports `posix-rename@openssh.com` extension

**Usage:**

```bash
sftp -P 9500 user@localhost
sftp> ls
sftp> put report.pdf
sftp> get photo.jpg
sftp> mkdir backups
sftp> rm old-file.txt
```

## How Sync Works

1. Every file change (create, modify, delete) gets a new monotonic **version** in the local SQLite database.
2. Each peer polls others: `GET /changes?since=<cursor>` — returns only files changed since the last sync.
3. For each changed file: if the SHA256 hash differs and the incoming `mod_time` is newer, the file is downloaded.
4. The cursor is saved — next poll only returns new changes.
5. A new node joining the cluster starts with `since=0` and downloads everything.

### Conflict Resolution

If the same file is modified on two nodes simultaneously, the version with the newer `mod_time` wins. The file hash is checked to avoid redundant downloads.

### Cycle Prevention

When a node writes a file received from a peer, it marks it in memory. The file watcher sees the event but skips it if the hash matches. The mark is removed after 5 seconds.

### Deletions

When a file is deleted, a **tombstone** record (`deleted=true`) is created. Peers receive it and delete the file locally. Tombstones are purged after `tombstone_ttl` (default 7 days).

## Peer-to-Peer HTTP API

The internal API used by nodes to synchronize. Can also be used for monitoring or custom integrations.

### GET /changes?since=N&limit=1000

Returns files changed since version N.

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

### GET /files/{name...}

Downloads a file by path.

```bash
curl -O 'http://localhost:9100/files/report.txt'
curl -O 'http://localhost:9100/files/docs/drafts/spec.pdf'
```

### GET /status

Returns node status.

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

## Development

### Project Structure

```
birak/
  Dockerfile                         — multi-stage Docker build
  cmd/birakd/main.go                 — entrypoint, CLI, graceful shutdown
  internal/
    config/config.go              — YAML config parsing
    store/store.go                — SQLite: files + cursors tables
    watcher/watcher.go            — fsnotify + debounce + periodic scan
    server/server.go              — HTTP API for peer synchronization
    syncer/syncer.go              — polling, conflict resolution, downloads
    gateway/gateway.go            — Gateway interface
    gateway/s3/                   — S3 Gateway
    gateway/webdav/               — WebDAV Gateway
    gateway/httpui/               — HTTP file browser (embedded SPA)
    gateway/sftp/                 — SFTP Gateway
  integration_test.go             — multi-node integration tests
```

### Running Tests

```bash
# All tests
go test -v -timeout 120s ./...

# Unit tests for a specific package
go test -v ./internal/store/
go test -v ./internal/gateway/s3/
go test -v ./internal/gateway/webdav/
go test -v ./internal/gateway/httpui/
go test -v ./internal/gateway/sftp/

# Integration tests only (spins up real nodes)
go test -v -timeout 120s -run TestIntegration
```
