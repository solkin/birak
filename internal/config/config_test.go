package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestLoad_MultipartDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Unset multipart fields stay zero; the store resolves them to the
	// S3-compatible defaults so there is one source of truth.
	if cfg.Multipart != (MultipartConfig{}) {
		t.Fatalf("expected zero multipart config, got %+v", cfg.Multipart)
	}
}

func TestLoad_MultipartConfig(t *testing.T) {
	path := writeYAML(t, `
multipart:
  min_part_bytes: 1048576
  max_part_bytes: 104857600
  max_parts: 500
  max_active_uploads: 20
  max_concurrent_part_uploads: 4
  upload_ttl: 48h
  cleanup_interval: 15m
  temp_file_max_age: 6h
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := cfg.Multipart
	if m.MinPartBytes != 1048576 || m.MaxPartBytes != 104857600 {
		t.Fatalf("part size limits = %d/%d", m.MinPartBytes, m.MaxPartBytes)
	}
	if m.MaxParts != 500 || m.MaxActiveUploads != 20 || m.MaxConcurrentPartUploads != 4 {
		t.Fatalf("count limits = %+v", m)
	}
	if m.UploadTTL != 48*time.Hour || m.CleanupInterval != 15*time.Minute || m.TempFileMaxAge != 6*time.Hour {
		t.Fatalf("durations = %v/%v/%v", m.UploadTTL, m.CleanupInterval, m.TempFileMaxAge)
	}
}

func TestLoad_MultipartEnvOverride(t *testing.T) {
	path := writeYAML(t, `
multipart:
  max_parts: 500
  upload_ttl: 48h
`)
	t.Setenv("BIRAK_MULTIPART_MAX_PARTS", "77")
	t.Setenv("BIRAK_MULTIPART_MIN_PART_BYTES", "2048")
	t.Setenv("BIRAK_MULTIPART_MAX_CONCURRENT_PART_UPLOADS", "8")
	t.Setenv("BIRAK_MULTIPART_UPLOAD_TTL", "90m")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Multipart.MaxParts != 77 {
		t.Fatalf("max_parts = %d, want 77", cfg.Multipart.MaxParts)
	}
	if cfg.Multipart.MinPartBytes != 2048 {
		t.Fatalf("min_part_bytes = %d, want 2048", cfg.Multipart.MinPartBytes)
	}
	if cfg.Multipart.MaxConcurrentPartUploads != 8 {
		t.Fatalf("max_concurrent_part_uploads = %d, want 8", cfg.Multipart.MaxConcurrentPartUploads)
	}
	if cfg.Multipart.UploadTTL != 90*time.Minute {
		t.Fatalf("upload_ttl = %v, want 90m", cfg.Multipart.UploadTTL)
	}
}

func TestLoad_MultipartInvalidValues(t *testing.T) {
	cases := map[string]string{
		"min above max": `
multipart:
  min_part_bytes: 100
  max_part_bytes: 10
`,
		"negative max parts": `
multipart:
  max_parts: -1
`,
		"negative ttl": `
multipart:
  upload_ttl: -1h
`,
	}
	for name, fragment := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeYAML(t, fragment)
			if _, err := Load(path); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
