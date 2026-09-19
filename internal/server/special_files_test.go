package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/birak/birak/internal/store"
)

func TestFileEndpointRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	handler := New(st, root, "source", nil, Config{}, logger).Handler()
	completed := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(completed)
		handler.ServeHTTP(w, r)
	}))
	defer peer.Close()
	// Release a broken handler before httptest.Close waits for its connection.
	defer func() {
		f, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err == nil {
			defer f.Close()
		}
		select {
		case <-completed:
		case <-time.After(2 * time.Second):
			t.Error("FIFO handler remained blocked")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.URL+"/files/pipe", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := peer.Client().Do(req)
	if err != nil {
		t.Fatalf("FIFO request blocked instead of rejecting unsupported file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("FIFO response: %d", resp.StatusCode)
	}
}
