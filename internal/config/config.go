package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultMaxRepairQueue mirrors store.DefaultRepairLimit. It is duplicated
// rather than imported so that config stays free of the storage layer.
const defaultMaxRepairQueue = 100_000

// defaultScrubBytesPerSecond mirrors watcher.DefaultScrubRate, duplicated so
// that config stays free of the indexing layer.
const defaultScrubBytesPerSecond = 8 << 20

// defaultNodeID names a standalone node that did not choose a name. A node with
// peers must choose one: two nodes that both fell back to it reject each other
// as duplicates while each keeps accepting writes, so the cluster splits.
const defaultNodeID = "node-1"

// defaultMaxExternalDeletions mirrors watcher.DefaultMaxExternalDeletions.
const defaultMaxExternalDeletions = 1000

// Config holds all daemon configuration.
type Config struct {
	// StorageMode accepts the explicit filesystem setting and rejects old modes
	// before the daemon creates or opens storage. Replication has one model.
	StorageMode string   `yaml:"storage_mode"`
	NodeID      string   `yaml:"node_id"`
	SyncDir     string   `yaml:"sync_dir"`
	MetaDir     string   `yaml:"meta_dir"`
	ListenAddr  string   `yaml:"listen_addr"`
	Peers       []string `yaml:"peers"`
	Ignore      []string `yaml:"ignore"`
	// LogLevel is debug, info, warn or error. Every indexed file and every
	// cursor move is a debug record written synchronously, some of it while the
	// store's write mutex is held, so debug logging on a busy node is a
	// throughput cost and a stalled log consumer is a replication outage.
	LogLevel string `yaml:"log_level"`
	// ClusterSecret, when set, is required in the X-Birak-Secret header of
	// every peer-to-peer request. It is mandatory once peers are configured.
	// Empty leaves the sync API of a standalone node open, which is only safe
	// when the listen address is unreachable from outside the host.
	ClusterSecret string `yaml:"cluster_secret"`
	// TLSCertFile and TLSKeyFile serve the peer API over HTTPS. Peers then use
	// https:// URLs; the secret and file bytes no longer cross the network in
	// clear text.
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
	// PeerCAFile adds a PEM bundle to the system roots used to verify https://
	// peers, for clusters that sign peer certificates with a private CA.
	PeerCAFile string `yaml:"peer_ca_file"`
	// MetricsToken, when set, also admits /metrics requests that carry
	// "Authorization: Bearer <token>", so a scraper does not need the cluster
	// secret, which would let it read every file.
	MetricsToken string `yaml:"metrics_token"`
	// AccessLog writes one line per gateway request at info level.
	AccessLog bool `yaml:"access_log"`
	// MaxUploadBytes caps the size of a single uploaded object/file across all
	// gateways (S3, WebDAV, HTTP UI, SFTP). 0 means unlimited.
	MaxUploadBytes int64           `yaml:"max_upload_bytes"`
	Multipart      MultipartConfig `yaml:"multipart"`
	Sync           SyncConfig      `yaml:"sync"`
	Gateways       GatewaysConfig  `yaml:"gateways"`
}

// MultipartConfig holds limits and retention settings for S3 multipart uploads.
// Sizes are in bytes; durations use Go duration syntax ("24h", "30m").
type MultipartConfig struct {
	// MinPartBytes is the minimum size of every part but the last (S3: 5 MiB).
	MinPartBytes int64 `yaml:"min_part_bytes"`
	// MaxPartBytes is the maximum size of a single part (S3: 5 GiB).
	MaxPartBytes int64 `yaml:"max_part_bytes"`
	// MaxParts is the highest accepted part number (S3: 10000).
	MaxParts int `yaml:"max_parts"`
	// MaxActiveUploads caps simultaneously staged uploads; 0 means unlimited.
	MaxActiveUploads int `yaml:"max_active_uploads"`
	// MaxConcurrentPartUploads caps in-flight part uploads across all uploads;
	// 0 means unlimited. Requests over the cap are rejected with SlowDown, which
	// every S3 SDK retries with backoff.
	MaxConcurrentPartUploads int `yaml:"max_concurrent_part_uploads"`
	// UploadTTL is how long an untouched incomplete upload is kept before the
	// janitor discards it.
	UploadTTL time.Duration `yaml:"upload_ttl"`
	// CleanupInterval is how often the janitor sweeps.
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
	// TempFileMaxAge is how old an orphaned atomic-write scratch file must be
	// before a running server removes it.
	TempFileMaxAge time.Duration `yaml:"temp_file_max_age"`
}

// GatewaysConfig holds configuration for all gateways.
type GatewaysConfig struct {
	S3     S3GatewayConfig     `yaml:"s3"`
	WebDAV WebDAVGatewayConfig `yaml:"webdav"`
	HTTP   HTTPGatewayConfig   `yaml:"http"`
	SFTP   SFTPGatewayConfig   `yaml:"sftp"`
}

// AllowAnonymous on a gateway is the only way to run it without credentials.
// Without it an enabled gateway must have both credentials: a missing
// environment variable used to open the whole volume for anonymous writes.

// SFTPGatewayConfig holds configuration for the SFTP gateway.
type SFTPGatewayConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ListenAddr     string `yaml:"listen_addr"`
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
	AllowAnonymous bool   `yaml:"allow_anonymous"`
	HostKeyPath    string `yaml:"host_key_path"`
}

// HTTPGatewayConfig holds configuration for the HTTP file server gateway.
type HTTPGatewayConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ListenAddr     string `yaml:"listen_addr"`
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
	AllowAnonymous bool   `yaml:"allow_anonymous"`
}

// WebDAVGatewayConfig holds configuration for the WebDAV gateway.
type WebDAVGatewayConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ListenAddr     string `yaml:"listen_addr"`
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
	AllowAnonymous bool   `yaml:"allow_anonymous"`
}

// S3GatewayConfig holds configuration for the S3 gateway.
type S3GatewayConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ListenAddr     string `yaml:"listen_addr"`
	AccessKey      string `yaml:"access_key"`
	SecretKey      string `yaml:"secret_key"`
	AllowAnonymous bool   `yaml:"allow_anonymous"`
	Domain         string `yaml:"domain"`
	// Buckets are created at startup when missing, so that they exist before a
	// client that cannot create them, or should not, first asks for them.
	Buckets     []string `yaml:"buckets"`
	TLSCertFile string   `yaml:"tls_cert_file"`
	TLSKeyFile  string   `yaml:"tls_key_file"`
}

// SyncConfig holds sync-specific tuning parameters.
type SyncConfig struct {
	PollInterval           time.Duration `yaml:"poll_interval"`
	BatchLimit             int           `yaml:"batch_limit"`
	MaxConcurrentDownloads int           `yaml:"max_concurrent_downloads"`
	// MaxRepairQueue caps queued repair entries per peer; 0 disables the cap.
	// A full queue stops that peer's cursor instead of growing the database.
	MaxRepairQueue int64 `yaml:"max_repair_queue"`
	// TombstoneTTL is retained for config compatibility. Automatic tombstone
	// GC is disabled until a membership/application-acknowledgement protocol exists.
	TombstoneTTL time.Duration `yaml:"tombstone_ttl"`
	// ScanInterval is the stat-only sweep: it notices anything whose size or
	// timestamp moved. It no longer re-reads file bytes, so it is cheap enough
	// to run often on a large tree.
	ScanInterval time.Duration `yaml:"scan_interval"`
	// ScrubBytesPerSecond is the continuous byte budget for re-verifying stored
	// checksums. It replaces "read everything every scan_interval", whose cost
	// grew with the data and could not be bounded. 0 disables verification.
	ScrubBytesPerSecond int64         `yaml:"scrub_bytes_per_second"`
	DebounceWindow      time.Duration `yaml:"debounce_window"`
	// RepairInterval is the periodic rescan for new or due repairs. Repair
	// workers also refill on completion while preserving per-item backoff.
	RepairInterval time.Duration `yaml:"repair_interval"`
	// ReconcileInterval is how often a full manifest comparison runs against
	// every peer. This is the backstop that catches anything the version
	// cursor has already moved past; set it to 0 to disable (not recommended).
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// ReconcilePageBudget caps manifest pages per comparison pass, so a large
	// tree is compared continuously instead of in a burst. 0 compares it all
	// in one pass.
	ReconcilePageBudget int `yaml:"reconcile_page_budget"`
	// MaxExternalDeletions caps the deletions this node infers from files
	// missing on disk — not deleted through a gateway or by a peer — and
	// publishes within ten minutes. A larger group is held whole and the node
	// reports a fault until the files return or the cap is raised. 0 disables
	// the cap.
	MaxExternalDeletions int `yaml:"max_external_deletions"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		StorageMode: "filesystem",
		SyncDir:     "./sync",
		MetaDir:     "./meta",
		ListenAddr:  ":9100",
		Ignore:      []string{},
		LogLevel:    "info",
		AccessLog:   true,
		// Unlike the other multipart limits, zero means unlimited for the active
		// upload cap. Seed its documented default here so an omitted setting is
		// distinguishable from an explicit max_active_uploads: 0 in YAML.
		Multipart: MultipartConfig{MaxActiveUploads: 10000},
		Gateways: GatewaysConfig{
			S3: S3GatewayConfig{
				Enabled:    false,
				ListenAddr: ":9200",
			},
			WebDAV: WebDAVGatewayConfig{
				Enabled:    false,
				ListenAddr: ":9300",
			},
			HTTP: HTTPGatewayConfig{
				Enabled:    false,
				ListenAddr: ":9400",
			},
			SFTP: SFTPGatewayConfig{
				Enabled:    false,
				ListenAddr: ":9500",
			},
		},
		Sync: SyncConfig{
			PollInterval:           3 * time.Second,
			BatchLimit:             1000,
			MaxConcurrentDownloads: 5,
			MaxRepairQueue:         defaultMaxRepairQueue,
			TombstoneTTL:           168 * time.Hour, // 7 days
			ScanInterval:           5 * time.Minute,
			ScrubBytesPerSecond:    defaultScrubBytesPerSecond,
			MaxExternalDeletions:   defaultMaxExternalDeletions,
			DebounceWindow:         300 * time.Millisecond,
			RepairInterval:         30 * time.Second,
			ReconcileInterval:      time.Hour,
			ReconcilePageBudget:    64,
		},
	}
}

// Load applies an optional YAML file and environment overrides. An explicitly
// supplied file must exist; an invalid override must never fall back silently.
func Load(path string) (Config, error) {
	cfg := DefaultConfig()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return cfg, fmt.Errorf("environment: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// applyEnv overrides config values with environment variables.
// Env vars take precedence over the config file.
func applyEnv(c *Config) error {
	for key, target := range map[string]*string{
		"BIRAK_STORAGE_MODE":       &c.StorageMode,
		"BIRAK_NODE_ID":            &c.NodeID,
		"BIRAK_SYNC_DIR":           &c.SyncDir,
		"BIRAK_META_DIR":           &c.MetaDir,
		"BIRAK_LISTEN_ADDR":        &c.ListenAddr,
		"BIRAK_CLUSTER_SECRET":     &c.ClusterSecret,
		"BIRAK_TLS_CERT_FILE":      &c.TLSCertFile,
		"BIRAK_TLS_KEY_FILE":       &c.TLSKeyFile,
		"BIRAK_PEER_CA_FILE":       &c.PeerCAFile,
		"BIRAK_METRICS_TOKEN":      &c.MetricsToken,
		"BIRAK_LOG_LEVEL":          &c.LogLevel,
		"BIRAK_S3_LISTEN_ADDR":     &c.Gateways.S3.ListenAddr,
		"BIRAK_S3_ACCESS_KEY":      &c.Gateways.S3.AccessKey,
		"BIRAK_S3_SECRET_KEY":      &c.Gateways.S3.SecretKey,
		"BIRAK_S3_DOMAIN":          &c.Gateways.S3.Domain,
		"BIRAK_S3_TLS_CERT_FILE":   &c.Gateways.S3.TLSCertFile,
		"BIRAK_S3_TLS_KEY_FILE":    &c.Gateways.S3.TLSKeyFile,
		"BIRAK_WEBDAV_LISTEN_ADDR": &c.Gateways.WebDAV.ListenAddr,
		"BIRAK_WEBDAV_USERNAME":    &c.Gateways.WebDAV.Username,
		"BIRAK_WEBDAV_PASSWORD":    &c.Gateways.WebDAV.Password,
		"BIRAK_HTTP_LISTEN_ADDR":   &c.Gateways.HTTP.ListenAddr,
		"BIRAK_HTTP_USERNAME":      &c.Gateways.HTTP.Username,
		"BIRAK_HTTP_PASSWORD":      &c.Gateways.HTTP.Password,
		"BIRAK_SFTP_LISTEN_ADDR":   &c.Gateways.SFTP.ListenAddr,
		"BIRAK_SFTP_USERNAME":      &c.Gateways.SFTP.Username,
		"BIRAK_SFTP_PASSWORD":      &c.Gateways.SFTP.Password,
		"BIRAK_SFTP_HOST_KEY_PATH": &c.Gateways.SFTP.HostKeyPath,
	} {
		if raw := os.Getenv(key); raw != "" {
			*target = raw
		}
	}
	if raw := os.Getenv("BIRAK_PEERS"); raw != "" {
		c.Peers = splitList(raw)
	}
	if raw := os.Getenv("BIRAK_IGNORE"); raw != "" {
		c.Ignore = splitList(raw)
	}
	if raw := os.Getenv("BIRAK_S3_BUCKETS"); raw != "" {
		c.Gateways.S3.Buckets = splitList(raw)
	}
	for key, target := range map[string]*int64{
		"BIRAK_MAX_UPLOAD_BYTES":            &c.MaxUploadBytes,
		"BIRAK_SYNC_MAX_REPAIR_QUEUE":       &c.Sync.MaxRepairQueue,
		"BIRAK_SYNC_SCRUB_BYTES_PER_SECOND": &c.Sync.ScrubBytesPerSecond,
		"BIRAK_MULTIPART_MIN_PART_BYTES":    &c.Multipart.MinPartBytes,
		"BIRAK_MULTIPART_MAX_PART_BYTES":    &c.Multipart.MaxPartBytes,
	} {
		if raw := os.Getenv(key); raw != "" {
			value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*target = value
		}
	}
	for key, target := range map[string]*int{
		"BIRAK_SYNC_BATCH_LIMIT":                      &c.Sync.BatchLimit,
		"BIRAK_SYNC_MAX_CONCURRENT_DOWNLOADS":         &c.Sync.MaxConcurrentDownloads,
		"BIRAK_SYNC_RECONCILE_PAGE_BUDGET":            &c.Sync.ReconcilePageBudget,
		"BIRAK_SYNC_MAX_EXTERNAL_DELETIONS":           &c.Sync.MaxExternalDeletions,
		"BIRAK_MULTIPART_MAX_PARTS":                   &c.Multipart.MaxParts,
		"BIRAK_MULTIPART_MAX_ACTIVE_UPLOADS":          &c.Multipart.MaxActiveUploads,
		"BIRAK_MULTIPART_MAX_CONCURRENT_PART_UPLOADS": &c.Multipart.MaxConcurrentPartUploads,
	} {
		if raw := os.Getenv(key); raw != "" {
			value, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*target = value
		}
	}
	for key, target := range map[string]*time.Duration{
		"BIRAK_SYNC_POLL_INTERVAL":          &c.Sync.PollInterval,
		"BIRAK_SYNC_TOMBSTONE_TTL":          &c.Sync.TombstoneTTL,
		"BIRAK_SYNC_SCAN_INTERVAL":          &c.Sync.ScanInterval,
		"BIRAK_SYNC_DEBOUNCE_WINDOW":        &c.Sync.DebounceWindow,
		"BIRAK_SYNC_REPAIR_INTERVAL":        &c.Sync.RepairInterval,
		"BIRAK_SYNC_RECONCILE_INTERVAL":     &c.Sync.ReconcileInterval,
		"BIRAK_MULTIPART_UPLOAD_TTL":        &c.Multipart.UploadTTL,
		"BIRAK_MULTIPART_CLEANUP_INTERVAL":  &c.Multipart.CleanupInterval,
		"BIRAK_MULTIPART_TEMP_FILE_MAX_AGE": &c.Multipart.TempFileMaxAge,
	} {
		if raw := os.Getenv(key); raw != "" {
			value, err := time.ParseDuration(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*target = value
		}
	}
	for key, target := range map[string]*bool{
		"BIRAK_ACCESS_LOG":             &c.AccessLog,
		"BIRAK_S3_ENABLED":             &c.Gateways.S3.Enabled,
		"BIRAK_S3_ALLOW_ANONYMOUS":     &c.Gateways.S3.AllowAnonymous,
		"BIRAK_WEBDAV_ENABLED":         &c.Gateways.WebDAV.Enabled,
		"BIRAK_WEBDAV_ALLOW_ANONYMOUS": &c.Gateways.WebDAV.AllowAnonymous,
		"BIRAK_HTTP_ENABLED":           &c.Gateways.HTTP.Enabled,
		"BIRAK_HTTP_ALLOW_ANONYMOUS":   &c.Gateways.HTTP.AllowAnonymous,
		"BIRAK_SFTP_ENABLED":           &c.Gateways.SFTP.Enabled,
		"BIRAK_SFTP_ALLOW_ANONYMOUS":   &c.Gateways.SFTP.AllowAnonymous,
	} {
		if raw := os.Getenv(key); raw != "" {
			value, err := parseBool(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*target = value
		}
	}
	return nil
}

// splitList reads a comma-separated list, dropping blanks around and between
// its items.
func splitList(s string) []string {
	var items []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("must be true/false, yes/no, or 1/0")
	}
}

// logLevels are the accepted log_level names, in increasing severity.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// SlogLevel is the configured level. validate rejects every other spelling, so
// a typo cannot silently leave the daemon logging at a level nobody asked for.
func (c Config) SlogLevel() slog.Level {
	if level, ok := logLevels[strings.ToLower(strings.TrimSpace(c.LogLevel))]; ok {
		return level
	}
	return slog.LevelInfo
}

func (c *Config) validate() error {
	if c.StorageMode != "filesystem" {
		return fmt.Errorf("unsupported storage_mode %q: Birak supports filesystem replication only; existing quorum state cannot be opened as filesystem storage", c.StorageMode)
	}
	if (c.Gateways.S3.TLSCertFile == "") != (c.Gateways.S3.TLSKeyFile == "") {
		return fmt.Errorf("S3 TLS certificate and key must be configured together")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("tls_cert_file and tls_key_file must be configured together")
	}
	if _, ok := logLevels[strings.ToLower(strings.TrimSpace(c.LogLevel))]; !ok && c.LogLevel != "" {
		return fmt.Errorf("log_level must be one of debug, info, warn, error (got %q)", c.LogLevel)
	}
	// A trailing comma in BIRAK_PEERS or an empty YAML item is a typo, not a
	// peer: polling "" fails forever and shows as a permanently broken peer.
	peers := c.Peers[:0]
	seen := make(map[string]bool)
	for _, peer := range c.Peers {
		key := strings.TrimRight(strings.TrimSpace(peer), "/")
		if key == "" {
			continue
		}
		if err := checkPeerURL(key); err != nil {
			return err
		}
		if seen[key] {
			return fmt.Errorf("duplicate peer %q", peer)
		}
		seen[key] = true
		peers = append(peers, key)
	}
	c.Peers = peers

	c.NodeID = strings.TrimSpace(c.NodeID)
	if c.NodeID == "" {
		if len(c.Peers) > 0 {
			return fmt.Errorf("node_id is required when peers are configured; give every node its own, e.g. node_id: %s on the node that used the default", defaultNodeID)
		}
		c.NodeID = defaultNodeID
	}
	if strings.ContainsFunc(c.NodeID, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("node_id must not contain spaces or control characters (got %q)", c.NodeID)
	}
	if len(c.Peers) > 0 && c.ClusterSecret == "" {
		return fmt.Errorf("cluster_secret is required when peers are configured: without it anyone who reaches listen_addr reads every file")
	}
	if err := c.checkCredentials(); err != nil {
		return err
	}
	if c.SyncDir == "" {
		return fmt.Errorf("sync_dir is required")
	}
	if c.MetaDir == "" {
		return fmt.Errorf("meta_dir is required")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	if c.Sync.PollInterval <= 0 {
		return fmt.Errorf("sync.poll_interval must be positive")
	}
	if c.Sync.BatchLimit <= 0 {
		return fmt.Errorf("sync.batch_limit must be positive")
	}
	if c.Sync.MaxConcurrentDownloads <= 0 {
		return fmt.Errorf("sync.max_concurrent_downloads must be positive")
	}
	if c.Sync.MaxRepairQueue < 0 {
		return fmt.Errorf("sync.max_repair_queue must not be negative")
	}
	if c.Sync.TombstoneTTL <= 0 {
		return fmt.Errorf("sync.tombstone_ttl must be positive")
	}
	if c.Sync.ScanInterval <= 0 {
		return fmt.Errorf("sync.scan_interval must be positive")
	}
	if c.Sync.DebounceWindow < 0 {
		return fmt.Errorf("sync.debounce_window must not be negative")
	}
	if c.Sync.ScrubBytesPerSecond < 0 {
		return fmt.Errorf("sync.scrub_bytes_per_second must not be negative")
	}
	if c.Sync.MaxExternalDeletions < 0 {
		return fmt.Errorf("sync.max_external_deletions must not be negative")
	}
	if c.Sync.RepairInterval < 0 {
		return fmt.Errorf("sync.repair_interval must not be negative")
	}
	if c.Sync.ReconcileInterval < 0 {
		return fmt.Errorf("sync.reconcile_interval must not be negative")
	}
	if c.Sync.ReconcilePageBudget < 0 {
		return fmt.Errorf("sync.reconcile_page_budget must not be negative")
	}
	if c.MaxUploadBytes < 0 {
		return fmt.Errorf("max_upload_bytes must not be negative")
	}
	for _, pattern := range c.Ignore {
		if _, err := filepath.Match(pattern, "test"); err != nil {
			return fmt.Errorf("invalid ignore pattern %q: %w", pattern, err)
		}
	}
	if c.Gateways.S3.Enabled && c.Gateways.S3.ListenAddr == "" {
		return fmt.Errorf("gateways.s3.listen_addr is required when S3 gateway is enabled")
	}
	if c.Gateways.WebDAV.Enabled && c.Gateways.WebDAV.ListenAddr == "" {
		return fmt.Errorf("gateways.webdav.listen_addr is required when WebDAV gateway is enabled")
	}
	if c.Gateways.HTTP.Enabled && c.Gateways.HTTP.ListenAddr == "" {
		return fmt.Errorf("gateways.http.listen_addr is required when HTTP gateway is enabled")
	}
	if c.Gateways.SFTP.Enabled && c.Gateways.SFTP.ListenAddr == "" {
		return fmt.Errorf("gateways.sftp.listen_addr is required when SFTP gateway is enabled")
	}
	m := c.Multipart
	if m.MinPartBytes < 0 || m.MaxPartBytes < 0 {
		return fmt.Errorf("multipart part size limits must not be negative")
	}
	// A minimum above the maximum would reject every multi-part upload with a
	// contradictory pair of errors, so refuse it at startup instead.
	if m.MinPartBytes > 0 && m.MaxPartBytes > 0 && m.MinPartBytes > m.MaxPartBytes {
		return fmt.Errorf("multipart.min_part_bytes must not exceed multipart.max_part_bytes")
	}
	if m.MaxParts < 0 {
		return fmt.Errorf("multipart.max_parts must not be negative")
	}
	if m.MaxActiveUploads < 0 || m.MaxConcurrentPartUploads < 0 {
		return fmt.Errorf("multipart upload count limits must not be negative")
	}
	if m.UploadTTL < 0 || m.CleanupInterval < 0 || m.TempFileMaxAge < 0 {
		return fmt.Errorf("multipart durations must not be negative")
	}
	return nil
}

// checkPeerURL accepts an absolute http(s) URL naming a host. Anything else
// starts a node that polls nothing and reports a peer that never recovers.
func checkPeerURL(peer string) error {
	u, err := url.Parse(peer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("peer %q must be an http:// or https:// URL with a host, e.g. https://node-2:9100", peer)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("peer %q must not carry credentials, a query or a fragment", peer)
	}
	return nil
}

// credentialPair is one gateway's authentication settings, named for errors.
type credentialPair struct {
	gateway, first, second string
	a, b                   string
	enabled, anonymous     bool
}

func (c *Config) credentialPairs() []credentialPair {
	g := c.Gateways
	return []credentialPair{
		{"gateways.s3", "access_key", "secret_key", g.S3.AccessKey, g.S3.SecretKey, g.S3.Enabled, g.S3.AllowAnonymous},
		{"gateways.webdav", "username", "password", g.WebDAV.Username, g.WebDAV.Password, g.WebDAV.Enabled, g.WebDAV.AllowAnonymous},
		{"gateways.http", "username", "password", g.HTTP.Username, g.HTTP.Password, g.HTTP.Enabled, g.HTTP.AllowAnonymous},
		{"gateways.sftp", "username", "password", g.SFTP.Username, g.SFTP.Password, g.SFTP.Enabled, g.SFTP.AllowAnonymous},
	}
}

// checkCredentials fails closed. An enabled gateway needs both credentials,
// unless it explicitly allows anonymous access with neither. One credential
// alone used to be worse than none: an empty secret key or password is a
// value anyone can sign or log in with.
func (c *Config) checkCredentials() error {
	for _, p := range c.credentialPairs() {
		if !p.enabled {
			continue
		}
		switch {
		case p.a == "" && p.b == "" && !p.anonymous:
			return fmt.Errorf("%s is enabled without %s/%s; set both, or set %s.allow_anonymous: true to serve without authentication", p.gateway, p.first, p.second, p.gateway)
		case (p.a == "") != (p.b == ""):
			return fmt.Errorf("%s needs both %s and %s; one alone lets anyone in with an empty value", p.gateway, p.first, p.second)
		case isPlaceholder(p.a) || isPlaceholder(p.b):
			return fmt.Errorf("%s credentials still contain an example placeholder (changeme); set real values", p.gateway)
		}
	}
	for name, value := range map[string]string{"cluster_secret": c.ClusterSecret, "metrics_token": c.MetricsToken} {
		if isPlaceholder(value) {
			return fmt.Errorf("%s still contains an example placeholder (changeme); set a real value", name)
		}
	}
	return nil
}

// isPlaceholder recognizes the example values shipped in .env.example, which
// are public and therefore not credentials.
func isPlaceholder(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "changeme")
}

// SecurityWarnings returns messages for settings that are allowed but expose
// data: an explicitly anonymous gateway, an open peer API, and peer traffic in
// clear text over a network that may not be private.
func SecurityWarnings(c Config) []string {
	var warnings []string
	if c.ClusterSecret == "" && !loopbackListener(c.ListenAddr) {
		warnings = append(warnings, "cluster_secret is empty; the peer API on "+c.ListenAddr+" serves every file to anyone who can reach it")
	}
	for _, peer := range c.Peers {
		if u, err := url.Parse(peer); err == nil && u.Scheme == "http" && !privateHost(u.Hostname()) {
			warnings = append(warnings, "peer "+peer+" uses plain HTTP over a network that may not be private; the cluster secret and file bytes travel unencrypted — use https:// (tls_cert_file on that node) or a VPN")
		}
	}
	if c.Sync.ReconcileInterval == 0 {
		warnings = append(warnings, "sync.reconcile_interval is 0: full reconciliation is disabled, so anything the change stream misses will never be repaired")
	}
	for _, p := range c.credentialPairs() {
		if p.enabled && p.anonymous && p.a == "" && p.b == "" {
			warnings = append(warnings, p.gateway+" allows anonymous access; anyone who reaches it can read, write and delete")
		}
	}
	return warnings
}

// loopbackListener reports whether addr only accepts local connections.
func loopbackListener(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// privateHost reports whether a peer host is on a network an operator
// controls: a loopback or private address, or a name that public DNS cannot
// resolve, such as a Docker service name.
func privateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".internal", ".lan", ".home.arpa", ".localhost"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
