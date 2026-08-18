package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all daemon configuration.
type Config struct {
	NodeID     string   `yaml:"node_id"`
	SyncDir    string   `yaml:"sync_dir"`
	MetaDir    string   `yaml:"meta_dir"`
	ListenAddr string   `yaml:"listen_addr"`
	Peers      []string `yaml:"peers"`
	Ignore     []string `yaml:"ignore"`
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

// SFTPGatewayConfig holds configuration for the SFTP gateway.
type SFTPGatewayConfig struct {
	Enabled     bool   `yaml:"enabled"`
	ListenAddr  string `yaml:"listen_addr"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	HostKeyPath string `yaml:"host_key_path"`
}

// HTTPGatewayConfig holds configuration for the HTTP file server gateway.
type HTTPGatewayConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
}

// WebDAVGatewayConfig holds configuration for the WebDAV gateway.
type WebDAVGatewayConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
}

// S3GatewayConfig holds configuration for the S3 gateway.
type S3GatewayConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
	AccessKey  string `yaml:"access_key"`
	SecretKey  string `yaml:"secret_key"`
	Domain     string `yaml:"domain"`
}

// SyncConfig holds sync-specific tuning parameters.
type SyncConfig struct {
	PollInterval           time.Duration `yaml:"poll_interval"`
	BatchLimit             int           `yaml:"batch_limit"`
	MaxConcurrentDownloads int           `yaml:"max_concurrent_downloads"`
	TombstoneTTL           time.Duration `yaml:"tombstone_ttl"`
	ScanInterval           time.Duration `yaml:"scan_interval"`
	DebounceWindow         time.Duration `yaml:"debounce_window"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		NodeID:     "node-1",
		SyncDir:    "./sync",
		MetaDir:    "./meta",
		ListenAddr: ":9100",
		Ignore:     []string{},
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
			TombstoneTTL:           168 * time.Hour, // 7 days
			ScanInterval:           5 * time.Minute,
			DebounceWindow:         300 * time.Millisecond,
		},
	}
}

// Load reads a YAML config file (if it exists) and applies environment
// variable overrides. The config file is optional — Birak can be configured
// entirely through environment variables.
func Load(path string) (Config, error) {
	cfg := DefaultConfig()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("read config %s: %w", path, err)
			}
			// Config file not found — continue with defaults + env vars.
		} else {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}

	applyEnv(&cfg)

	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// applyEnv overrides config values with environment variables.
// Env vars take precedence over the config file.
func applyEnv(c *Config) {
	if v := os.Getenv("BIRAK_NODE_ID"); v != "" {
		c.NodeID = v
	}
	if v := os.Getenv("BIRAK_SYNC_DIR"); v != "" {
		c.SyncDir = v
	}
	if v := os.Getenv("BIRAK_META_DIR"); v != "" {
		c.MetaDir = v
	}
	if v := os.Getenv("BIRAK_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("BIRAK_PEERS"); v != "" {
		c.Peers = strings.Split(v, ",")
	}
	if v := os.Getenv("BIRAK_IGNORE"); v != "" {
		c.Ignore = strings.Split(v, ",")
	}
	if v := os.Getenv("BIRAK_MAX_UPLOAD_BYTES"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
			c.MaxUploadBytes = n
		}
	}

	// Sync settings.
	if v := os.Getenv("BIRAK_SYNC_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Sync.PollInterval = d
		}
	}
	if v := os.Getenv("BIRAK_SYNC_BATCH_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Sync.BatchLimit = n
		}
	}
	if v := os.Getenv("BIRAK_SYNC_MAX_CONCURRENT_DOWNLOADS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Sync.MaxConcurrentDownloads = n
		}
	}
	if v := os.Getenv("BIRAK_SYNC_TOMBSTONE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Sync.TombstoneTTL = d
		}
	}
	if v := os.Getenv("BIRAK_SYNC_SCAN_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Sync.ScanInterval = d
		}
	}
	if v := os.Getenv("BIRAK_SYNC_DEBOUNCE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Sync.DebounceWindow = d
		}
	}

	// S3 gateway.
	if v := os.Getenv("BIRAK_S3_ENABLED"); v != "" {
		c.Gateways.S3.Enabled = parseBool(v)
	}
	if v := os.Getenv("BIRAK_S3_LISTEN_ADDR"); v != "" {
		c.Gateways.S3.ListenAddr = v
	}
	if v := os.Getenv("BIRAK_S3_ACCESS_KEY"); v != "" {
		c.Gateways.S3.AccessKey = v
	}
	if v := os.Getenv("BIRAK_S3_SECRET_KEY"); v != "" {
		c.Gateways.S3.SecretKey = v
	}
	if v := os.Getenv("BIRAK_S3_DOMAIN"); v != "" {
		c.Gateways.S3.Domain = v
	}

	// WebDAV gateway.
	if v := os.Getenv("BIRAK_WEBDAV_ENABLED"); v != "" {
		c.Gateways.WebDAV.Enabled = parseBool(v)
	}
	if v := os.Getenv("BIRAK_WEBDAV_LISTEN_ADDR"); v != "" {
		c.Gateways.WebDAV.ListenAddr = v
	}
	if v := os.Getenv("BIRAK_WEBDAV_USERNAME"); v != "" {
		c.Gateways.WebDAV.Username = v
	}
	if v := os.Getenv("BIRAK_WEBDAV_PASSWORD"); v != "" {
		c.Gateways.WebDAV.Password = v
	}

	// HTTP file browser gateway.
	if v := os.Getenv("BIRAK_HTTP_ENABLED"); v != "" {
		c.Gateways.HTTP.Enabled = parseBool(v)
	}
	if v := os.Getenv("BIRAK_HTTP_LISTEN_ADDR"); v != "" {
		c.Gateways.HTTP.ListenAddr = v
	}
	if v := os.Getenv("BIRAK_HTTP_USERNAME"); v != "" {
		c.Gateways.HTTP.Username = v
	}
	if v := os.Getenv("BIRAK_HTTP_PASSWORD"); v != "" {
		c.Gateways.HTTP.Password = v
	}

	// SFTP gateway.
	if v := os.Getenv("BIRAK_SFTP_ENABLED"); v != "" {
		c.Gateways.SFTP.Enabled = parseBool(v)
	}
	if v := os.Getenv("BIRAK_SFTP_LISTEN_ADDR"); v != "" {
		c.Gateways.SFTP.ListenAddr = v
	}
	if v := os.Getenv("BIRAK_SFTP_USERNAME"); v != "" {
		c.Gateways.SFTP.Username = v
	}
	if v := os.Getenv("BIRAK_SFTP_PASSWORD"); v != "" {
		c.Gateways.SFTP.Password = v
	}
	if v := os.Getenv("BIRAK_SFTP_HOST_KEY_PATH"); v != "" {
		c.Gateways.SFTP.HostKeyPath = v
	}

	envInt64Map := map[string]*int64{
		"BIRAK_MULTIPART_MIN_PART_BYTES": &c.Multipart.MinPartBytes,
		"BIRAK_MULTIPART_MAX_PART_BYTES": &c.Multipart.MaxPartBytes,
	}
	for env, ptr := range envInt64Map {
		if v := os.Getenv(env); v != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
				*ptr = n
			}
		}
	}

	envIntMap := map[string]*int{
		"BIRAK_MULTIPART_MAX_PARTS":                   &c.Multipart.MaxParts,
		"BIRAK_MULTIPART_MAX_ACTIVE_UPLOADS":          &c.Multipart.MaxActiveUploads,
		"BIRAK_MULTIPART_MAX_CONCURRENT_PART_UPLOADS": &c.Multipart.MaxConcurrentPartUploads,
	}
	for env, ptr := range envIntMap {
		if v := os.Getenv(env); v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
				*ptr = n
			}
		}
	}

	envDurationMap := map[string]*time.Duration{
		"BIRAK_MULTIPART_UPLOAD_TTL":        &c.Multipart.UploadTTL,
		"BIRAK_MULTIPART_CLEANUP_INTERVAL":  &c.Multipart.CleanupInterval,
		"BIRAK_MULTIPART_TEMP_FILE_MAX_AGE": &c.Multipart.TempFileMaxAge,
	}
	for env, ptr := range envDurationMap {
		if v := os.Getenv(env); v != "" {
			if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
				*ptr = d
			}
		}
	}
}

func parseBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes"
}

func (c *Config) validate() error {
	if c.NodeID == "" {
		return fmt.Errorf("node_id is required")
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

// SecurityWarnings returns messages for enabled gateways that run without any
// credentials configured. Such gateways accept unauthenticated access, which is
// easy to enable by accident — e.g. a default container run — so the operator is
// warned at startup rather than silently exposing the filesystem.
func SecurityWarnings(c Config) []string {
	var warnings []string
	g := c.Gateways
	if g.S3.Enabled && g.S3.AccessKey == "" && g.S3.SecretKey == "" {
		warnings = append(warnings, "S3 gateway is enabled without access_key/secret_key; it accepts unauthenticated requests")
	}
	if g.WebDAV.Enabled && g.WebDAV.Username == "" && g.WebDAV.Password == "" {
		warnings = append(warnings, "WebDAV gateway is enabled without username/password; it accepts unauthenticated requests")
	}
	if g.HTTP.Enabled && g.HTTP.Username == "" && g.HTTP.Password == "" {
		warnings = append(warnings, "HTTP UI gateway is enabled without username/password; it accepts unauthenticated requests")
	}
	if g.SFTP.Enabled && g.SFTP.Username == "" && g.SFTP.Password == "" {
		warnings = append(warnings, "SFTP gateway is enabled without username/password; it accepts unauthenticated connections")
	}
	return warnings
}
