//go:build unix

package fileops

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFIFOReaderDoesNotBlockNamespace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// Unblock a broken implementation before cleanup so failure cannot hang tests.
	defer func() {
		f, _ := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
		if f != nil {
			f.Close()
		}
	}()
	done := make(chan error, 1)
	go func() {
		f, err := OpenReader(root, path)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO opened as a regular reader")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO open blocked")
	}
	go func() { done <- Mkdir(root, filepath.Join(root, "ordinary"), 0700, false) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO reader retained namespace lock")
	}
}
