package sftp

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/watcher"
)

func TestSFTPRefusesQuarantinedFile(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("file"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("CORRUPT!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := w.Refresh("file"); !errors.Is(err, watcher.ErrIntegrity) {
		t.Fatalf("not quarantined: %v", err)
	}
	addr := testGateway(t, root, nil, "review", "synthetic-password")
	ch := sftpClient(t, addr, "review", "synthetic-password")
	sftpInit(t, ch)
	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "/file")
	req = marshalUint32(req, sshFxfRead)
	req = marshalUint32(req, 0)
	if err := writePacket(ch, sshFxpOpen, req); err != nil {
		t.Fatal(err)
	}
	kind, payload, err := readPacket(ch)
	if err != nil {
		t.Fatal(err)
	}
	if kind == sshFxpStatus {
		return
	}
	if kind != sshFxpHandle {
		t.Fatalf("unexpected response: %d", kind)
	}
	_, rest, err := unmarshalUint32(payload)
	if err != nil {
		t.Fatal(err)
	}
	handle, _, err := unmarshalString(rest)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := sftpReadFile(t, ch, 2, handle, 0, 100)
	t.Fatalf("SFTP served quarantined content: body=%q quarantine=%d", body, w.Status().Quarantined)
}
