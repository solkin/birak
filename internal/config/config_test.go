package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.NodeID != "node-1" {
		t.Fatalf("expected node-1, got %s", cfg.NodeID)
	}
	if cfg.Gateways.S3.ListenAddr != ":9200" {
		t.Fatalf("expected default S3 addr :9200, got %s", cfg.Gateways.S3.ListenAddr)
	}
	if cfg.MaxUploadBytes != 0 {
		t.Fatalf("expected default MaxUploadBytes 0 (unlimited), got %d", cfg.MaxUploadBytes)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	t.Setenv("BIRAK_NODE_ID", "from-env")
	t.Setenv("BIRAK_S3_ENABLED", "true")
	t.Setenv("BIRAK_S3_ACCESS_KEY", "env-key")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.NodeID != "from-env" {
		t.Fatalf("expected from-env, got %s", cfg.NodeID)
	}
	if !cfg.Gateways.S3.Enabled {
		t.Fatal("S3 should be enabled by env")
	}
	if cfg.Gateways.S3.AccessKey != "env-key" {
		t.Fatalf("expected env-key, got %s", cfg.Gateways.S3.AccessKey)
	}
}

func TestLoad_MaxUploadBytes_YAML(t *testing.T) {
	path := writeYAML(t, "max_upload_bytes: 1048576\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxUploadBytes != 1048576 {
		t.Fatalf("expected 1048576, got %d", cfg.MaxUploadBytes)
	}
}

func TestLoad_MaxUploadBytes_Env(t *testing.T) {
	path := writeYAML(t, "max_upload_bytes: 1048576\n")
	t.Setenv("BIRAK_MAX_UPLOAD_BYTES", "2097152")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxUploadBytes != 2097152 {
		t.Fatalf("env should override yaml: expected 2097152, got %d", cfg.MaxUploadBytes)
	}
}

func TestLoad_MaxUploadBytes_NegativeRejected(t *testing.T) {
	path := writeYAML(t, "max_upload_bytes: -1\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for negative max_upload_bytes")
	}
}

func TestSecurityWarnings(t *testing.T) {
	open := Config{Gateways: GatewaysConfig{
		S3:     S3GatewayConfig{Enabled: true},
		WebDAV: WebDAVGatewayConfig{Enabled: true},
		HTTP:   HTTPGatewayConfig{Enabled: true},
		SFTP:   SFTPGatewayConfig{Enabled: true},
	}}
	if got := SecurityWarnings(open); len(got) != 4 {
		t.Fatalf("expected 4 warnings for open gateways, got %d: %v", len(got), got)
	}

	secured := Config{Gateways: GatewaysConfig{
		S3:     S3GatewayConfig{Enabled: true, AccessKey: "a", SecretKey: "b"},
		WebDAV: WebDAVGatewayConfig{Enabled: true, Username: "u", Password: "p"},
		HTTP:   HTTPGatewayConfig{Enabled: false},
		SFTP:   SFTPGatewayConfig{Enabled: true, Username: "u", Password: "p"},
	}}
	if got := SecurityWarnings(secured); len(got) != 0 {
		t.Fatalf("expected no warnings for secured/disabled gateways, got %v", got)
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"anything", false},
	}
	for _, tt := range tests {
		if got := parseBool(tt.input); got != tt.want {
			t.Errorf("parseBool(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
