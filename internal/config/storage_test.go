package config

import (
	"strings"
	"testing"
)

func TestLoad_FilesystemStorageOnly(t *testing.T) {
	for _, mode := range []string{"filesystem", "quorum", "typo"} {
		t.Run(mode, func(t *testing.T) {
			_, err := Load(writeYAML(t, "storage_mode: "+mode+"\n"))
			if mode == "filesystem" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "unsupported storage_mode") {
				t.Fatalf("expected unsupported storage error, got %v", err)
			}
		})
	}
	t.Setenv("BIRAK_STORAGE_MODE", "quorum")
	if _, err := Load(""); err == nil {
		t.Fatal("removed mode from environment must not silently start filesystem storage")
	}
}

func TestLoad_S3TLS(t *testing.T) {
	path := writeYAML(t, "gateways:\n  s3:\n    tls_cert_file: cert.pem\n    tls_key_file: key.pem\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateways.S3.TLSCertFile != "cert.pem" || cfg.Gateways.S3.TLSKeyFile != "key.pem" {
		t.Fatalf("TLS configuration not loaded: %+v", cfg.Gateways.S3)
	}
	for _, field := range []string{"tls_cert_file", "tls_key_file"} {
		if _, err := Load(writeYAML(t, "gateways:\n  s3:\n    "+field+": lone.pem\n")); err == nil {
			t.Fatalf("accepted incomplete TLS pair: %s", field)
		}
	}
	t.Setenv("BIRAK_S3_TLS_CERT_FILE", "env-cert.pem")
	t.Setenv("BIRAK_S3_TLS_KEY_FILE", "env-key.pem")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateways.S3.TLSCertFile != "env-cert.pem" || cfg.Gateways.S3.TLSKeyFile != "env-key.pem" {
		t.Fatalf("TLS environment overrides not applied: %+v", cfg.Gateways.S3)
	}
}
