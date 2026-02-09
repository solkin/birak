package config

import (
	"fmt"
	"os"
	"path/filepath"
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
		Ignore: []string{
			".DS_Store",
			"Thumbs.db",
			"desktop.ini",
			".birak-tmp-*",
		},
		Gateways: GatewaysConfig{
			S3: S3GatewayConfig{
				Enabled:    false,
				ListenAddr: ":9200",
			},
			WebDAV: WebDAVGatewayConfig{
				Enabled:    false,
				ListenAddr: ":9300",
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

// Load reads a YAML config file and returns a Config.
func Load(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
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
	return nil
}
