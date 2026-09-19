package sftp

import (
	"golang.org/x/crypto/ssh"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readStatusCode(t *testing.T, ch io.Reader) uint32 {
	t.Helper()
	pktType, payload, err := readPacket(ch)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if pktType != sshFxpStatus {
		t.Fatalf("expected STATUS, got %d", pktType)
	}
	_, rest, _ := unmarshalUint32(payload)
	code, _, _ := unmarshalUint32(rest)
	return code
}

func TestSetstatAppliesAttrs(t *testing.T) {
	syncDir := t.TempDir()
	path := filepath.Join(syncDir, "f.txt")
	if err := os.WriteFile(path, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "/f.txt")
	req = marshalUint32(req, sshFileXferAttrSize|sshFileXferAttrPermissions|sshFileXferAttrACModTime)
	req = marshalUint64(req, 3)
	req = marshalUint32(req, 0o600)
	req = marshalUint32(req, 123)
	req = marshalUint32(req, 456)
	writePacket(ch, sshFxpSetstat, req)

	if code := readStatusCode(t, ch); code != sshFxOk {
		t.Fatalf("expected OK for setstat, got %d", code)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 3 || info.Mode().Perm() != 0o600 || info.ModTime().Unix() != 456 {
		t.Fatalf("attrs not applied: size=%d mode=%o mtime=%d", info.Size(), info.Mode().Perm(), info.ModTime().Unix())
	}
}

func TestSetstatTraversal(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "../../../etc/passwd")
	req = marshalUint32(req, 0)
	writePacket(ch, sshFxpSetstat, req)

	if code := readStatusCode(t, ch); code != sshFxPermissionDenied {
		t.Fatalf("expected permission denied for traversal setstat, got %d", code)
	}
}

func TestSetstatBadAttrs(t *testing.T) {
	syncDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(syncDir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "/f.txt")
	req = append(req, 0, 0) // too short for attrs flags
	writePacket(ch, sshFxpSetstat, req)

	if code := readStatusCode(t, ch); code != sshFxBadMessage {
		t.Fatalf("expected bad message for malformed setstat attrs, got %d", code)
	}
}

func TestFsetstatInvalidHandle(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "missing-handle")
	req = marshalUint32(req, 0)
	writePacket(ch, sshFxpFsetstat, req)

	if code := readStatusCode(t, ch); code != sshFxFailure {
		t.Fatalf("expected failure for invalid fsetstat handle, got %d", code)
	}
}

func TestFsetstatAppliesAttrs(t *testing.T) {
	syncDir := t.TempDir()
	path := filepath.Join(syncDir, "f.txt")
	if err := os.WriteFile(path, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	handle := sftpOpenFile(t, ch, 1, "/f.txt", sshFxfRead|sshFxfWrite)

	var req []byte
	req = marshalUint32(req, 2)
	req = marshalString(req, handle)
	req = marshalUint32(req, sshFileXferAttrSize|sshFileXferAttrPermissions)
	req = marshalUint64(req, 2)
	req = marshalUint32(req, 0o640)
	writePacket(ch, sshFxpFsetstat, req)

	if code := readStatusCode(t, ch); code != sshFxOk {
		t.Fatalf("expected OK for fsetstat, got %d", code)
	}
	sftpClose(t, ch, 3, handle)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2 || info.Mode().Perm() != 0o640 {
		t.Fatalf("attrs not applied: size=%d mode=%o", info.Size(), info.Mode().Perm())
	}
}

func TestExtendedPosixRename(t *testing.T) {
	syncDir := t.TempDir()
	os.WriteFile(filepath.Join(syncDir, "old.txt"), []byte("data"), 0o644)

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "posix-rename@openssh.com")
	req = marshalString(req, "/old.txt")
	req = marshalString(req, "/new.txt")
	writePacket(ch, sshFxpExtended, req)

	pktType, payload, _ := readPacket(ch)
	if pktType != sshFxpStatus {
		t.Fatalf("expected STATUS, got %d", pktType)
	}
	_, rest, _ := unmarshalUint32(payload)
	code, _, _ := unmarshalUint32(rest)
	if code != sshFxOk {
		t.Fatalf("posix-rename failed: %d", code)
	}

	if _, err := os.Stat(filepath.Join(syncDir, "old.txt")); !os.IsNotExist(err) {
		t.Error("old file should not exist")
	}
	data, err := os.ReadFile(filepath.Join(syncDir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data" {
		t.Errorf("expected data, got %q", data)
	}
}

func TestExtendedPosixRenameTraversal(t *testing.T) {
	syncDir := t.TempDir()
	os.WriteFile(filepath.Join(syncDir, "f.txt"), []byte("x"), 0o644)

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "posix-rename@openssh.com")
	req = marshalString(req, "/f.txt")
	req = marshalString(req, "../../../etc/evil")
	writePacket(ch, sshFxpExtended, req)

	pktType, payload, _ := readPacket(ch)
	if pktType != sshFxpStatus {
		t.Fatalf("expected STATUS, got %d", pktType)
	}
	_, rest, _ := unmarshalUint32(payload)
	code, _, _ := unmarshalUint32(rest)
	if code != sshFxPermissionDenied {
		t.Errorf("expected PERMISSION_DENIED, got %d", code)
	}
}

func TestExtendedUnknown(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "some-unknown-extension@example.com")
	writePacket(ch, sshFxpExtended, req)

	pktType, payload, _ := readPacket(ch)
	if pktType != sshFxpStatus {
		t.Fatalf("expected STATUS, got %d", pktType)
	}
	_, rest, _ := unmarshalUint32(payload)
	code, _, _ := unmarshalUint32(rest)
	if code != sshFxOpUnsupported {
		t.Errorf("expected OP_UNSUPPORTED, got %d", code)
	}
}

func TestOpenReadWrite(t *testing.T) {
	syncDir := t.TempDir()
	os.WriteFile(filepath.Join(syncDir, "rw.txt"), []byte("AAAA"), 0o644)

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	handle := sftpOpenFile(t, ch, 1, "/rw.txt", sshFxfRead|sshFxfWrite)
	sftpWriteFile(t, ch, 2, handle, 2, []byte("ZZ"))
	data, _ := sftpReadFile(t, ch, 3, handle, 0, 10)
	sftpClose(t, ch, 4, handle)

	if string(data) != "AAZZ" {
		t.Errorf("expected AAZZ, got %q", data)
	}
}

func TestOpenExclusive(t *testing.T) {
	syncDir := t.TempDir()
	os.WriteFile(filepath.Join(syncDir, "exists.txt"), []byte("x"), 0o644)

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "/exists.txt")
	req = marshalUint32(req, sshFxfWrite|sshFxfCreat|sshFxfExcl)
	req = marshalUint32(req, 0)
	writePacket(ch, sshFxpOpen, req)

	pktType, _, _ := readPacket(ch)
	if pktType != sshFxpStatus {
		t.Fatalf("expected STATUS (failure), got %d", pktType)
	}
}

func TestOpenCreatOnly(t *testing.T) {
	syncDir := t.TempDir()

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	handle := sftpOpenFile(t, ch, 1, "/new.txt", sshFxfWrite|sshFxfCreat)
	sftpWriteFile(t, ch, 2, handle, 0, []byte("created"))
	sftpClose(t, ch, 3, handle)

	data, err := os.ReadFile(filepath.Join(syncDir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "created" {
		t.Fatalf("expected created, got %q", data)
	}
}

func TestRenameTraversalSource(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	code := sftpRename(t, ch, 1, "../../../etc/passwd", "/stolen.txt")
	if code == sshFxOk {
		t.Error("expected failure for traversal in source")
	}
}

func TestRenameTraversalDest(t *testing.T) {
	syncDir := t.TempDir()
	os.WriteFile(filepath.Join(syncDir, "f.txt"), []byte("x"), 0o644)

	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	code := sftpRename(t, ch, 1, "/f.txt", "../../../etc/evil")
	if code == sshFxOk {
		t.Error("expected failure for traversal in destination")
	}
}

func TestOpenTraversal(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "../../../etc/passwd")
	req = marshalUint32(req, sshFxfRead)
	req = marshalUint32(req, 0)
	writePacket(ch, sshFxpOpen, req)

	pktType, payload, _ := readPacket(ch)
	if pktType != sshFxpStatus {
		t.Fatalf("expected STATUS, got %d", pktType)
	}
	_, rest, _ := unmarshalUint32(payload)
	code, _, _ := unmarshalUint32(rest)
	if code != sshFxPermissionDenied {
		t.Errorf("expected PERMISSION_DENIED, got %d", code)
	}
}

func TestMkdirTraversal(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	code := sftpMkdir(t, ch, 1, "../../../evil")
	if code == sshFxOk {
		t.Error("expected failure for traversal in mkdir")
	}
}

func TestRemoveTraversal(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	code := sftpRemove(t, ch, 1, "../../../etc/passwd")
	if code == sshFxOk {
		t.Error("expected failure for traversal in remove")
	}
}

func TestRmdirTraversal(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	code := sftpRmdir(t, ch, 1, "../../../etc")
	if code == sshFxOk {
		t.Error("expected failure for traversal in rmdir")
	}
}

func TestStatTraversal(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	pktType, payload := sftpStat(t, ch, 1, "../../../etc/passwd")
	if pktType != sshFxpStatus {
		t.Fatalf("expected STATUS, got %d", pktType)
	}
	_, rest, _ := unmarshalUint32(payload)
	code, _, _ := unmarshalUint32(rest)
	if code == sshFxOk {
		t.Error("should not succeed for traversal path")
	}
}

func TestOpendirTraversal(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")
	ch := sftpClient(t, addr, "user", "pass")
	sftpInit(t, ch)

	var req []byte
	req = marshalUint32(req, 1)
	req = marshalString(req, "../../../etc")
	writePacket(ch, sshFxpOpendir, req)

	pktType, payload, _ := readPacket(ch)
	if pktType != sshFxpStatus {
		t.Fatalf("expected STATUS, got %d", pktType)
	}
	_, rest, _ := unmarshalUint32(payload)
	code, _, _ := unmarshalUint32(rest)
	if code != sshFxPermissionDenied {
		t.Errorf("expected PERMISSION_DENIED, got %d", code)
	}
}

func TestSubsystemMalformedPayload_NoPanic(t *testing.T) {
	syncDir := t.TempDir()
	addr := testGateway(t, syncDir, nil, "user", "pass")

	config := &ssh.ClientConfig{
		User:            "user",
		Auth:            []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer conn.Close()

	ch, reqs, err := conn.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	go ssh.DiscardRequests(reqs)

	// 2-byte payload cannot hold the 4-byte length prefix — previously panicked.
	ok, err := ch.SendRequest("subsystem", true, []byte{0x00, 0x00})
	if err != nil {
		t.Fatalf("send malformed subsystem: %v", err)
	}
	if ok {
		t.Fatalf("malformed subsystem request should be rejected")
	}

	// The daemon must still be alive and able to serve a valid subsystem request
	// on the same channel (handleSession loops with `continue` after rejection).
	ok, err = ch.SendRequest("subsystem", true, marshalString(nil, "sftp"))
	if err != nil {
		t.Fatalf("send valid subsystem: %v", err)
	}
	if !ok {
		t.Fatalf("valid subsystem request should be accepted")
	}

	sftpInit(t, ch)

	var rp []byte
	rp = marshalUint32(rp, 1)
	rp = marshalString(rp, ".")
	if err := writePacket(ch, sshFxpRealpath, rp); err != nil {
		t.Fatalf("write realpath: %v", err)
	}
	pktType, _, err := readPacket(ch)
	if err != nil {
		t.Fatalf("read realpath reply: %v", err)
	}
	if pktType != sshFxpName {
		t.Fatalf("expected NAME reply after malformed request, got %d", pktType)
	}
}
