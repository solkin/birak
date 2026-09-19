package webdav

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/server"
	"github.com/birak/birak/internal/store"
	"github.com/birak/birak/internal/syncer"
	"github.com/birak/birak/internal/watcher"
)

func reviewState(t *testing.T, root string) (*store.Store, *watcher.Watcher, *slog.Logger) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(filepath.Join(t.TempDir(), "node.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := watcher.New(root, st, logger, time.Millisecond, time.Hour, nil)
	return st, w, logger
}

func reviewWrite(t *testing.T, root, name, body string, stamp time.Time) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestReviewSuccessfulOverwriteMustSurviveReplication(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		t.Run(method, func(t *testing.T) {
			root := t.TempDir()
			st, w, logger := reviewState(t, root)
			now := time.Now()
			reviewWrite(t, root, "src", "desired copied bytes", now.Add(-2*time.Hour))
			reviewWrite(t, root, "dst", "previous destination", now.Add(-time.Hour))
			for _, name := range []string{"src", "dst"} {
				if err := w.Refresh(name); err != nil {
					t.Fatal(err)
				}
			}
			old, err := st.GetFile("dst")
			if err != nil || old == nil {
				t.Fatalf("old metadata: %+v %v", old, err)
			}
			peerRoot := t.TempDir()
			peerStore, _, _ := reviewState(t, peerRoot)
			reviewWrite(t, peerRoot, "dst", "previous destination", time.Unix(0, old.ModTime))
			if _, err := peerStore.PutFile("dst", old.ModTime, old.Size, old.Hash, false); err != nil {
				t.Fatal(err)
			}
			peer := httptest.NewServer(server.New(peerStore, peerRoot, "remote-review", nil, server.Config{}, logger).Handler())
			defer peer.Close()
			g := New(root, nil, Config{}, logger)
			req := httptest.NewRequest(method, "http://node/src", nil)
			req.Header.Set("Destination", "http://node/dst")
			out := httptest.NewRecorder()
			g.server.Handler.ServeHTTP(out, req)
			if out.Code != http.StatusNoContent {
				t.Fatalf("%s failed: %d %s", method, out.Code, out.Body)
			}
			if err := w.Refresh("dst"); err != nil {
				t.Fatal(err)
			}
			copied, err := os.ReadFile(filepath.Join(root, "dst"))
			if err != nil || string(copied) != "desired copied bytes" {
				t.Fatalf("overwrite did not initially succeed: %q %v", copied, err)
			}
			syn := syncer.New(st, w, root, "local-review", []string{peer.URL}, nil, logger, syncer.Options{
				PollInterval: 5 * time.Millisecond, BatchLimit: 10, MaxConcurrentDownloads: 1,
				RepairInterval: 10 * time.Millisecond,
			})
			ctx, cancel := context.WithCancel(context.Background())
			watchDone, syncDone := make(chan error, 1), make(chan struct{})
			go func() { watchDone <- w.Run(ctx) }()
			go func() { syn.Run(ctx); close(syncDone) }()
			defer func() {
				cancel()
				<-syncDone
				if err := <-watchDone; err != nil {
					t.Error(err)
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				stats := syn.PeerStats()
				if len(stats) == 1 && stats[0].Healthy && stats[0].Cursor > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("peer did not finish a successful poll")
				}
				time.Sleep(5 * time.Millisecond)
			}
			got, err := os.ReadFile(filepath.Join(root, "dst"))
			if err != nil || string(got) != "desired copied bytes" {
				t.Fatalf("successful %s (HTTP %d) was reverted by an unchanged peer: dst=%q err=%v", method, out.Code, got, err)
			}
		})
	}
}

func TestReviewReplacementCrashMustPreserveOldFile(t *testing.T) {
	if root := os.Getenv("BIRAK_REVIEW_REPLACE_CHILD"); root != "" {
		_ = stageReplace(filepath.Join(root, "valuable"), func() error {
			// Kill at the real cut between moving the original and publishing
			// the replacement. No defers or rollback execute in this process.
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			os.Exit(73)
			return nil
		})
		os.Exit(74)
	}
	root := t.TempDir()
	st, w, logger := reviewState(t, root)
	reviewWrite(t, root, "valuable", "last acknowledged bytes", time.Now().Add(-time.Hour))
	if err := w.Refresh("valuable"); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestReviewReplacementCrashMustPreserveOldFile$")
	cmd.Env = append(os.Environ(), "BIRAK_REVIEW_REPLACE_CHILD="+root)
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("child did not reach crash point: %v %s", err, output)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("unexpected child termination: %v %s", err, output)
	}
	backups, err := filepath.Glob(filepath.Join(root, ".birak-bak-*"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recovery starts with %d backup(s)", len(backups))
	// Startup recovers replacement journals before sweeping scratch files.
	if err := fileops.RecoverLocked(root); err != nil {
		t.Fatal(err)
	}
	gateway.SweepTempFiles(root, 0, logger)
	if err := w.Refresh("valuable"); err != nil {
		t.Fatal(err)
	}
	meta, err := st.GetFile("valuable")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := os.ReadFile(filepath.Join(root, "valuable"))
	if readErr != nil || string(got) != "last acknowledged bytes" || meta.Deleted {
		t.Fatalf("restart discarded original and advertised deletion: bytes=%q read=%v metadata=%+v", got, readErr, meta)
	}
}

func TestReviewGatewayMustRejectReplacementStorageRoot(t *testing.T) {
	root := t.TempDir()
	_, w, logger := reviewState(t, root)
	if err := w.CheckStorage(); err != nil {
		t.Fatal(err)
	}
	away := root + "-real-volume"
	if err := os.Rename(root, away); err != nil {
		t.Fatal(err)
	}
	defer func() { os.RemoveAll(root); os.Rename(away, root) }()
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.CheckStorage(); err == nil {
		t.Fatal("fixture failed to lose the bound storage volume")
	}
	g := New(root, nil, Config{}, logger)
	out := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(out, httptest.NewRequest(http.MethodPut, "/new", strings.NewReader("acknowledged on wrong volume")))
	if out.Code >= 200 && out.Code < 300 {
		body, _ := os.ReadFile(filepath.Join(root, "new"))
		t.Fatalf("gateway acknowledged write while bound storage is missing: HTTP %d, wrong-volume bytes=%q", out.Code, body)
	}
}

func TestTreeReplacementReplicatesBothNamespaceDirections(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		for _, sourceDirectory := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/directory-source=%v", method, sourceDirectory), func(t *testing.T) {
				root, peerRoot := t.TempDir(), t.TempDir()
				st, w, logger := reviewState(t, root)
				remoteStore, remoteWatcher, _ := reviewState(t, peerRoot)
				oldStamp := time.Now().Add(-time.Hour)
				newStamp := oldStamp.Add(-time.Hour)
				sourceName, targetName := "src", "dst/deep/sub/old"
				if sourceDirectory {
					sourceName = "src/new"
					targetName = "dst"
				}
				for _, dir := range []string{root, peerRoot} {
					os.MkdirAll(filepath.Dir(filepath.Join(dir, targetName)), 0o755)
					reviewWrite(t, dir, targetName, "previous", oldStamp)
				}
				os.MkdirAll(filepath.Dir(filepath.Join(root, sourceName)), 0o755)
				reviewWrite(t, root, sourceName, "desired", newStamp)
				for _, name := range []string{sourceName, targetName} {
					if err := w.Refresh(name); err != nil {
						t.Fatal(err)
					}
				}
				if err := remoteWatcher.Refresh(targetName); err != nil {
					t.Fatal(err)
				}
				g := New(root, nil, Config{}, logger)
				req := httptest.NewRequest(method, "http://node/src", nil)
				req.Header.Set("Destination", "http://node/dst")
				out := httptest.NewRecorder()
				g.server.Handler.ServeHTTP(out, req)
				if out.Code != http.StatusNoContent {
					t.Fatalf("overwrite %d: %s", out.Code, out.Body)
				}
				// Verify gateway success itself, without giving fsnotify time to catch up.
				old, err := st.GetFile(targetName)
				if err != nil || old == nil || !old.Deleted {
					t.Fatalf("obsolete destination not indexed at acknowledgement: %+v %v", old, err)
				}
				newName := "dst"
				if sourceDirectory {
					newName = "dst/new"
				}
				created, err := st.GetFile(newName)
				if err != nil {
					t.Fatal(err)
				}
				observed, err := remoteStore.GetFile(targetName)
				if err != nil || created == nil || observed == nil || created.StateClock() <= observed.StateClock() {
					t.Fatalf("old-mtime replacement did not supersede the observed namespace: created=%+v previous=%+v error=%v", created, observed, err)
				}
				peer := httptest.NewServer(server.New(st, root, "source", nil, server.Config{}, logger).Handler())
				defer peer.Close()
				syn := syncer.New(remoteStore, remoteWatcher, peerRoot, "target", []string{peer.URL}, nil, logger, syncer.Options{PollInterval: 5 * time.Millisecond, RepairInterval: 10 * time.Millisecond, BatchLimit: 100, MaxConcurrentDownloads: 3})
				ctx, cancel := context.WithCancel(context.Background())
				wdone, sdone := make(chan error, 1), make(chan struct{})
				go func() { wdone <- remoteWatcher.Run(ctx) }()
				go func() { syn.Run(ctx); close(sdone) }()
				defer func() {
					cancel()
					<-sdone
					if err := <-wdone; err != nil {
						t.Error(err)
					}
				}()
				wanted := "dst"
				if sourceDirectory {
					wanted = "dst/new"
				}
				deadline := time.Now().Add(12 * time.Second)
				for time.Now().Before(deadline) {
					body, _ := os.ReadFile(filepath.Join(peerRoot, wanted))
					if string(body) == "desired" {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				entries, _ := remoteStore.ListManifest("", 100)
				t.Fatalf("namespace did not converge: manifest=%+v stats=%+v", entries, syn.PeerStats())
			})
		}
	}
}
