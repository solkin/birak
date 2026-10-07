package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuorumConfigurationFencesFilesystemGateways(t *testing.T) {
	base := `storage_mode: quorum
node_id: n1
max_upload_bytes: 1073741824
quorum:
  cluster_id: test
  state_dir: /private/quorum
  advertise_addr: 127.0.0.1:9100
  ca_file: ca.crt
  cert_file: n1.crt
  key_file: n1.key
`
	for _, tc := range []struct {
		name, extra string
		ok          bool
	}{
		{"valid", "gateways:\n  s3:\n    enabled: true\n    listen_addr: 127.0.0.1:9200\n    access_key: a\n    secret_key: b\n", true},
		{"missing credentials", "", false},
		{"public plaintext", "gateways:\n  s3:\n    enabled: true\n    listen_addr: ':9200'\n    access_key: a\n    secret_key: b\n", false},
		{"webdav bypass", "gateways:\n  s3:\n    access_key: a\n    secret_key: b\n  webdav:\n    enabled: true\n", false},
		{"sftp bypass", "gateways:\n  s3:\n    access_key: a\n    secret_key: b\n  sftp:\n    enabled: true\n", false},
		{"legacy peers", "peers: ['http://127.0.0.1:8000']\ngateways:\n  s3:\n    access_key: a\n    secret_key: b\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(base+tc.extra), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if (err == nil) != tc.ok {
				t.Fatalf("expected valid=%v, got %v", tc.ok, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(strings.ReplaceAll(base, "storage_mode: quorum", "storage_mode: typo")), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("accepted unknown storage mode")
	}
}
