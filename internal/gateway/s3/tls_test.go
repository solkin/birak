package s3

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGateway_TLSStartStop(t *testing.T) {
	// Reuse httptest's certificate and verifying client, but serve the request
	// through Gateway.Start so the configured certificate files are exercised.
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := fixture.TLS.Certificates[0]
	client := fixture.Client()
	client.Timeout = time.Second
	address := fixture.Listener.Addr().String()
	fixture.Close()
	defer client.CloseIdleConnections()

	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	var chain []byte
	for _, der := range certificate.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, chain, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "bucket"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bucket", "object"), []byte("verified HTTPS object"), 0600); err != nil {
		t.Fatal(err)
	}
	g := New(dir, nil, Config{ListenAddr: address, TLSCertFile: certPath, TLSKeyFile: keyPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("TLS server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("TLS server did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("https://" + address + "/bucket/object")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "verified HTTPS object" {
				t.Fatalf("HTTPS GET: status=%d body=%q error=%v", response.StatusCode, body, readErr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTPS listener did not become available: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
