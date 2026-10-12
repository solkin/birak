package fileops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestReadersDoNotTakeTheCommitLock(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file")
	if err := os.WriteFile(p, []byte("abcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	fenced := errors.New("fenced")
	var readFence error
	SetHooks(dir, Hooks{
		Validate:     func() error { return fenced },
		ValidateRead: func() error { return readFence },
	})
	unlock := Lock(dir)
	done := make(chan error, 1)
	go func() {
		r, err := OpenReader(dir, p)
		if err == nil {
			_, err = r.Read(make([]byte, 4))
			err = errors.Join(err, r.Close())
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("open/read/close waited for the commit lock")
	}
	unlock()

	// The read-side fence is still applied.
	readFence = fenced
	if r, err := OpenReader(dir, p); !errors.Is(err, fenced) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("read-side fence ignored: %v", err)
	}
	// Without a read-side fence the commit fence applies, under the lock.
	SetHooks(dir, Hooks{Validate: func() error { return fenced }})
	if r, err := OpenReader(dir, p); !errors.Is(err, fenced) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("storage fence bypassed by a read: %v", err)
	}
}

// A reader whose quarantine check fails is not left registered.
func TestRejectedReaderIsUnregistered(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file")
	if err := os.WriteFile(p, []byte("abcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetHooks(dir, Hooks{CheckRead: func(string, os.FileInfo) error { return ErrUnreadable }})
	if _, err := OpenReader(dir, p); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("check ignored: %v", err)
	}
	if got := ReaderStats(dir); got.Active != 0 {
		t.Fatalf("rejected reader still registered: %+v", got)
	}
}
