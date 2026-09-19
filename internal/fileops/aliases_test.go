package fileops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootAliasesShareStorageGuard(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "volume")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("storage unavailable")
	SetHooks(alias, Hooks{Validate: func() error { return unavailable }})
	called := false
	if err := Do(real, func() error { called = true; return nil }); !errors.Is(err, unavailable) || called {
		t.Fatalf("canonical spelling bypassed guard: called=%v error=%v", called, err)
	}
	// Looking up the alias after it is rebound must keep the original guard.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	if err := Do(alias, func() error { called = true; return nil }); !errors.Is(err, unavailable) {
		t.Fatalf("rebound alias lost guard: %v", err)
	}
}
func TestRootAliasesShareWriterReservation(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "volume")
	os.Symlink(real, alias)
	f, _, err := OpenWriter(alias, filepath.Join(alias, "file"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer AbortWriter(alias, f)
	if err = Remove(real, filepath.Join(real, "file"), true); !errors.Is(err, ErrBusy) {
		t.Fatalf("different root spelling bypassed writer: %v", err)
	}
}
