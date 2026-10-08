package fileops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReaderLoadTracksReadsRevocationAndRepeatedClose(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file")
	if err := os.WriteFile(p, []byte("abcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := ReaderStats(dir); got.Active != 1 {
		t.Fatalf("open: %+v", got)
	}
	if n, err := r.Read(make([]byte, 3)); n != 3 || err != nil {
		t.Fatalf("read: %d %v", n, err)
	}
	if n, err := r.ReadAt(make([]byte, 2), 5); n != 2 || err != nil {
		t.Fatalf("readat: %d %v", n, err)
	}
	unlock := Lock(dir)
	RevokeReadersLocked(dir, p)
	RevokeReadersLocked(dir, p)
	unlock()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("revoked: %v", err)
	}
	if got := ReaderStats(dir); got.Active != 1 || got.BytesRead != 5 || got.Revocations != 1 {
		t.Fatalf("revocation: %+v", got)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if got := ReaderStats(dir); got.Active != 0 || got.BytesRead != 5 || got.Revocations != 1 {
		t.Fatalf("close: %+v", got)
	}
}
