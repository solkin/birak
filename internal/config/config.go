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
	NodeID     string          `yaml:"node_id"`
	SyncDir    string          `yaml:"sync_dir"`
	MetaDir    string          `yaml:"meta_dir"`
	ListenAddr string          `yaml:"listen_addr"`
	Peers      []string        `yaml:"peers"`
	Ignore     []string        `yaml:"ignore"`
	Sync       SyncConfig      `yaml:"sync"`
	Gateways   GatewaysConfig  `yaml:"gateways"`
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
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
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
		Ignore: []string{},
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
			TombstoneTTL:          168 * time.Hour, // 7 days
			ScanInterval:          5 * time.Minute,
			DebounceWindow:        300 * time.Millisecond,
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
	return nil
}
