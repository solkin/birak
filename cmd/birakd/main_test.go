package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRemovedStorageModeFailsBeforeOpeningDirectories(t *testing.T) {
	dir := t.TempDir()
	syncDir, metaDir := filepath.Join(dir, "sync"), filepath.Join(dir, "meta")
	path := filepath.Join(dir, "config.yaml")
	data := fmt.Sprintf("storage_mode: quorum\nsync_dir: %q\nmeta_dir: %q\n", syncDir, metaDir)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(path); err == nil || !strings.Contains(err.Error(), "unsupported storage_mode") {
		t.Fatalf("expected removed mode to fail startup, got %v", err)
	}
	for _, path := range []string{syncDir, metaDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("startup touched filesystem storage %s: %v", path, err)
		}
	}
}

func TestComponentListenFailureIsReturned(t *testing.T) {
	for _, gateway := range []string{"peer", "s3", "webdav", "http", "sftp"} {
		t.Run(gateway, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			dir := t.TempDir()
			listen := "127.0.0.1:0"
			if gateway == "peer" {
				listen = listener.Addr().String()
			}
			data := fmt.Sprintf("sync_dir: %q\nmeta_dir: %q\nlisten_addr: %q\n", filepath.Join(dir, "sync"), filepath.Join(dir, "meta"), listen)
			data += "gateways:\n"
			for _, name := range []string{"s3", "webdav", "http", "sftp"} {
				addr := "127.0.0.1:0"
				if name == gateway {
					addr = listener.Addr().String()
				}
				data += fmt.Sprintf("  %s:\n    enabled: true\n    listen_addr: %q\n", name, addr)
			}
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if err := run(path); !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("listen failure must reach main: %v", err)
			}
		})
	}
}
