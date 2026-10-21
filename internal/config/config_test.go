package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	t.Setenv("BIRAK_S3_SECRET_KEY", "env-secret")

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

func TestLoad_S3Buckets(t *testing.T) {
	path := writeYAML(t, `
gateways:
  s3:
    enabled: true
    allow_anonymous: true
    buckets: [files, backups]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := strings.Join(cfg.Gateways.S3.Buckets, ","); got != "files,backups" {
		t.Fatalf("expected files,backups from YAML, got %q", got)
	}

	t.Setenv("BIRAK_S3_BUCKETS", " media, ,backups ,")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := strings.Join(cfg.Gateways.S3.Buckets, ","); got != "media,backups" {
		t.Fatalf("expected media,backups from env, got %q", got)
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
	open := Config{
		ListenAddr: ":9100",
		Peers:      []string{"http://203.0.113.7:9100", "http://node-2:9100", "http://10.0.0.3:9100", "https://peer.example.com:9100"},
		Sync:       SyncConfig{ReconcileInterval: time.Hour},
		Gateways: GatewaysConfig{
			S3:     S3GatewayConfig{Enabled: true, AllowAnonymous: true},
			WebDAV: WebDAVGatewayConfig{Enabled: true, AllowAnonymous: true},
			HTTP:   HTTPGatewayConfig{Enabled: true, Username: "u", Password: "p", AllowAnonymous: true},
			SFTP:   SFTPGatewayConfig{Enabled: false, AllowAnonymous: true},
		},
	}
	got := strings.Join(SecurityWarnings(open), "\n")
	for _, want := range []string{
		"cluster_secret is empty",
		"peer http://203.0.113.7:9100 uses plain HTTP",
		"gateways.s3 allows anonymous access",
		"gateways.webdav allows anonymous access",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing warning %q in:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"node-2", "10.0.0.3", "peer.example.com", "gateways.http", "gateways.sftp"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected warning about %q in:\n%s", unwanted, got)
		}
	}

	secured := Config{
		ListenAddr:    "127.0.0.1:9100",
		ClusterSecret: "s",
		Sync:          SyncConfig{ReconcileInterval: time.Hour},
		Gateways: GatewaysConfig{
			S3:     S3GatewayConfig{Enabled: true, AccessKey: "a", SecretKey: "b"},
			WebDAV: WebDAVGatewayConfig{Enabled: true, Username: "u", Password: "p"},
			HTTP:   HTTPGatewayConfig{Enabled: false},
			SFTP:   SFTPGatewayConfig{Enabled: true, Username: "u", Password: "p"},
		},
	}
	if got := SecurityWarnings(secured); len(got) != 0 {
		t.Fatalf("expected no warnings for secured/disabled gateways, got %v", got)
	}
	if got := SecurityWarnings(Config{ListenAddr: "127.0.0.1:9100", Sync: SyncConfig{ReconcileInterval: time.Hour}}); len(got) != 0 {
		t.Fatalf("a loopback-only peer API without a secret is not exposed, got %v", got)
	}
}

// A gateway that is enabled must authenticate unless an operator explicitly
// opts out. A missing compose variable, or one credential of a pair, used to
// open the volume to anonymous writes behind the public reverse proxy.
func TestGatewayCredentialsFailClosed(t *testing.T) {
	gateways := []struct{ name, enabled, first, second, anonymous string }{
		{"s3", "BIRAK_S3_ENABLED", "BIRAK_S3_ACCESS_KEY", "BIRAK_S3_SECRET_KEY", "BIRAK_S3_ALLOW_ANONYMOUS"},
		{"webdav", "BIRAK_WEBDAV_ENABLED", "BIRAK_WEBDAV_USERNAME", "BIRAK_WEBDAV_PASSWORD", "BIRAK_WEBDAV_ALLOW_ANONYMOUS"},
		{"http", "BIRAK_HTTP_ENABLED", "BIRAK_HTTP_USERNAME", "BIRAK_HTTP_PASSWORD", "BIRAK_HTTP_ALLOW_ANONYMOUS"},
		{"sftp", "BIRAK_SFTP_ENABLED", "BIRAK_SFTP_USERNAME", "BIRAK_SFTP_PASSWORD", "BIRAK_SFTP_ALLOW_ANONYMOUS"},
	}
	cases := []struct {
		name, first, second, anonymous, want string
	}{
		{name: "both set", first: "user", second: "pass"},
		{name: "none", want: "allow_anonymous"},
		{name: "none but explicitly anonymous", anonymous: "true"},
		{name: "first only", first: "admin", want: "needs both"},
		{name: "second only", second: "pass", want: "needs both"},
		{name: "first only even if anonymous", first: "admin", anonymous: "true", want: "needs both"},
		{name: "example placeholder", first: "admin", second: "changeme-password", want: "placeholder"},
	}
	for _, g := range gateways {
		for _, tc := range cases {
			t.Run(g.name+"/"+tc.name, func(t *testing.T) {
				t.Setenv(g.enabled, "true")
				t.Setenv(g.first, tc.first)
				t.Setenv(g.second, tc.second)
				t.Setenv(g.anonymous, tc.anonymous)
				_, err := Load("")
				switch {
				case tc.want == "" && err != nil:
					t.Fatalf("rejected: %v", err)
				case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
					t.Fatalf("expected error containing %q, got %v", tc.want, err)
				}
			})
		}
	}
	// Disabled gateways keep whatever they have; nothing is served.
	t.Setenv("BIRAK_S3_ACCESS_KEY", "only-key")
	if _, err := Load(""); err != nil {
		t.Fatalf("a disabled gateway's credentials are not checked: %v", err)
	}
}

func TestPeersRequireIdentityAndSecret(t *testing.T) {
	t.Setenv("BIRAK_PEERS", "http://node-2:9100")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "node_id is required") {
		t.Fatalf("a node with peers fell back to the shared default node_id: %v", err)
	}
	t.Setenv("BIRAK_NODE_ID", "node-1")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "cluster_secret is required") {
		t.Fatalf("peers accepted without a cluster secret: %v", err)
	}
	t.Setenv("BIRAK_CLUSTER_SECRET", "changeme-cluster-secret")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("example cluster secret accepted: %v", err)
	}
	t.Setenv("BIRAK_CLUSTER_SECRET", "a-real-secret")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeID != "node-1" {
		t.Fatalf("explicit node_id lost: %q", cfg.NodeID)
	}
	t.Setenv("BIRAK_NODE_ID", "node 1")
	if _, err := Load(""); err == nil {
		t.Fatal("node_id with a space accepted")
	}
}

func TestPeerListIsValidated(t *testing.T) {
	t.Setenv("BIRAK_NODE_ID", "node-1")
	t.Setenv("BIRAK_CLUSTER_SECRET", "secret")
	t.Setenv("BIRAK_PEERS", "http://node-2:9100, ,https://node-3:9100/,")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Peers, ","); got != "http://node-2:9100,https://node-3:9100" {
		t.Fatalf("peers = %q; empty items must be dropped and the rest normalized", got)
	}
	for _, bad := range []string{"node-2:9100", "ftp://node-2", "http://", "http://user:pw@node-2:9100", "http://node-2:9100/?x=1"} {
		t.Setenv("BIRAK_PEERS", bad)
		if _, err := Load(""); err == nil {
			t.Errorf("peer %q accepted", bad)
		}
	}
	// A standalone node keeps the documented default identity.
	t.Setenv("BIRAK_PEERS", "")
	t.Setenv("BIRAK_NODE_ID", "")
	if cfg, err := Load(""); err != nil || cfg.NodeID != "node-1" {
		t.Fatalf("standalone default node_id = %q, %v", cfg.NodeID, err)
	}
}

func TestPeerTLSAndMetricsSettings(t *testing.T) {
	path := writeYAML(t, "tls_cert_file: peer.pem\ntls_key_file: peer.key\npeer_ca_file: ca.pem\nmetrics_token: scrape\naccess_log: false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSCertFile != "peer.pem" || cfg.TLSKeyFile != "peer.key" || cfg.PeerCAFile != "ca.pem" || cfg.MetricsToken != "scrape" || cfg.AccessLog {
		t.Fatalf("settings not loaded: %+v", cfg)
	}
	if def, _ := Load(""); !def.AccessLog {
		t.Fatal("access log must stay on by default")
	}
	if _, err := Load(writeYAML(t, "tls_cert_file: peer.pem\n")); err == nil {
		t.Fatal("certificate without key accepted")
	}
	t.Setenv("BIRAK_IGNORE", " *.swp , .DS_Store")
	if cfg, err := Load(""); err != nil || strings.Join(cfg.Ignore, "|") != "*.swp|.DS_Store" {
		t.Fatalf("ignore patterns not trimmed: %q, %v", cfg.Ignore, err)
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		input   string
		want    bool
		invalid bool
	}{
		{"true", true, false},
		{"TRUE", true, false},
		{"1", true, false},
		{"yes", true, false},
		{"false", false, false},
		{"0", false, false},
		{"no", false, false},
		{" true ", true, false},
		{"", false, true},
		{"anything", false, true},
	}
	for _, tt := range tests {
		if got, err := parseBool(tt.input); got != tt.want || (err != nil) != tt.invalid {
			t.Errorf("parseBool(%q) = %v, %v; want %v, invalid=%v", tt.input, got, err, tt.want, tt.invalid)
		}
	}
}

func TestLoadMissingExplicitFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := Load(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing explicit config must fail: %v", err)
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	for _, key := range []string{
		"BIRAK_MAX_UPLOAD_BYTES", "BIRAK_SYNC_MAX_REPAIR_QUEUE", "BIRAK_SYNC_SCRUB_BYTES_PER_SECOND",
		"BIRAK_SYNC_BATCH_LIMIT", "BIRAK_SYNC_MAX_CONCURRENT_DOWNLOADS", "BIRAK_SYNC_RECONCILE_PAGE_BUDGET",
		"BIRAK_SYNC_POLL_INTERVAL", "BIRAK_SYNC_TOMBSTONE_TTL", "BIRAK_SYNC_SCAN_INTERVAL",
		"BIRAK_SYNC_DEBOUNCE_WINDOW", "BIRAK_SYNC_REPAIR_INTERVAL", "BIRAK_SYNC_RECONCILE_INTERVAL",
		"BIRAK_MULTIPART_MIN_PART_BYTES", "BIRAK_MULTIPART_MAX_PART_BYTES", "BIRAK_MULTIPART_MAX_PARTS",
		"BIRAK_MULTIPART_MAX_ACTIVE_UPLOADS", "BIRAK_MULTIPART_MAX_CONCURRENT_PART_UPLOADS",
		"BIRAK_MULTIPART_UPLOAD_TTL", "BIRAK_MULTIPART_CLEANUP_INTERVAL", "BIRAK_MULTIPART_TEMP_FILE_MAX_AGE",
		"BIRAK_S3_ENABLED", "BIRAK_WEBDAV_ENABLED", "BIRAK_HTTP_ENABLED", "BIRAK_SFTP_ENABLED",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "invalid")
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("invalid %s must fail with its name: %v", key, err)
			}
		})
	}
	for key, value := range map[string]string{
		"BIRAK_MAX_UPLOAD_BYTES": "-1", "BIRAK_SYNC_MAX_REPAIR_QUEUE": "-1",
		"BIRAK_SYNC_RECONCILE_PAGE_BUDGET": "-1", "BIRAK_SYNC_DEBOUNCE_WINDOW": "-1s",
		"BIRAK_MULTIPART_MAX_PARTS": "-1", "BIRAK_MULTIPART_UPLOAD_TTL": "-1s",
	} {
		t.Run(key+"/negative", func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := Load(""); err == nil {
				t.Fatal("negative limit accepted")
			}
		})
	}
}

func TestLoad_MultipartDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Zero means unlimited for MaxActiveUploads, so its finite default must be
	// applied while loading config. The store resolves the remaining zero fields.
	if cfg.Multipart.MaxActiveUploads != 10000 {
		t.Fatalf("max_active_uploads = %d, want default 10000", cfg.Multipart.MaxActiveUploads)
	}
	cfg.Multipart.MaxActiveUploads = 0
	if cfg.Multipart != (MultipartConfig{}) {
		t.Fatalf("unexpected non-zero multipart defaults: %+v", cfg.Multipart)
	}
}

func TestLoad_MultipartExplicitUnlimitedActiveUploads(t *testing.T) {
	path := writeYAML(t, "multipart:\n  max_active_uploads: 0\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Multipart.MaxActiveUploads != 0 {
		t.Fatalf("max_active_uploads = %d, want explicit unlimited value 0", cfg.Multipart.MaxActiveUploads)
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
