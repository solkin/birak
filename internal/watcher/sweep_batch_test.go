package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/birak/birak/internal/fileops"
)

func TestSweepPageDoesNotOverwriteNewerGatewayState(t *testing.T) {
	w := auditWatcher(t)
	name := "file"
	path := filepath.Join(w.dir, name)
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, err := w.store.GetFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, deleted := range []bool{false, true} {
		if deleted {
			if err := fileops.Remove(w.dir, path, false); err != nil {
				t.Fatal(err)
			}
		} else {
			stage, err := fileops.CreateTemp(w.dir, ".birak-tmp-sweep-*")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stage.WriteString("newer body"); err != nil {
				t.Fatal(err)
			}
			if err := stage.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fileops.Publish(w.dir, stage.Name(), path); err != nil {
				t.Fatal(err)
			}
			fileops.ReleaseTemp(stage)
		}
		current, err := w.store.GetFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.sweepKnownFile(name, old); err != nil {
			t.Fatal(err)
		}
		got, err := w.store.GetFile(name)
		if err != nil || got == nil || *got != *current {
			t.Fatalf("stale page changed current state: %+v -> %+v %v", current, got, err)
		}
	}
}
