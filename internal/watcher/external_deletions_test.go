package watcher

// A file missing from disk becomes a tombstone on every peer. These tests pin
// the two guards against a disk that merely stopped showing files: a second
// filesystem below the root is refused outright, and deletions inferred from
// absence are published only up to a budget, a larger group being held whole.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/birak/birak/internal/fileops"
)

// fakeDevices presents every path for which device returns nonzero as being on
// that device, and everything else as on device 1.
func fakeDevices(t *testing.T, device func(path string) uint64) {
	t.Helper()
	previous := deviceOf
	deviceOf = func(path string, _ os.FileInfo) (uint64, bool) {
		if d := device(path); d != 0 {
			return d, true
		}
		return 1, true
	}
	t.Cleanup(func() { deviceOf = previous })
}

func under(root, dir string) func(string) uint64 {
	prefix := filepath.Join(root, dir)
	return func(path string) uint64 {
		if path == prefix || strings.HasPrefix(path, prefix+string(filepath.Separator)) {
			return 2
		}
		return 0
	}
}

func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func countDeleted(t *testing.T, w *Watcher, names ...string) int {
	t.Helper()
	deleted := 0
	for _, name := range names {
		meta, err := w.store.GetFile(name)
		if err != nil || meta == nil {
			t.Fatalf("%s lost from the index: %+v %v", name, meta, err)
		}
		if meta.Deleted {
			deleted++
		}
	}
	return deleted
}

// --- a second filesystem below the root --------------------------------------

func TestAnotherFilesystemBelowTheRootFailsTheScan(t *testing.T) {
	w := auditWatcher(t)
	writeFiles(t, w.dir, "apks/a.apk", "apks/b.apk", "top.txt")
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	fakeDevices(t, under(w.dir, "apks"))
	// Whatever happens below it is not this node's to publish.
	if err := os.Remove(filepath.Join(w.dir, "apks", "a.apk")); err != nil {
		t.Fatal(err)
	}

	err := w.periodicScan(context.Background())
	want := `sync_dir contains another filesystem at "apks"; Birak requires a single filesystem under sync_dir`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a nested filesystem was not refused: %v", err)
	}
	if status := w.Status(); status.Ready || !strings.Contains(status.LastError, want) {
		t.Fatalf("a nested filesystem left the node ready: %+v", status)
	}
	if n := countDeleted(t, w, "apks/a.apk", "apks/b.apk", "top.txt"); n != 0 {
		t.Fatalf("%d files below a nested filesystem were published as deleted", n)
	}
}

// An empty mount point is where a filesystem comes back. Removing it as an
// empty parent would make the next mount fail, or land in the wrong place.
func TestCleanupNeverRemovesAMountPoint(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "mnt/sub/file")
	fakeDevices(t, under(root, "mnt"))
	file := filepath.Join(root, "mnt", "sub", "file")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	CleanEmptyParents(file, root, nil, auditWatcher(t).logger)
	if _, err := os.Stat(filepath.Join(root, "mnt", "sub")); !os.IsNotExist(err) {
		t.Fatalf("an empty directory on the mounted filesystem was kept: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "mnt")); err != nil || !info.IsDir() {
		t.Fatalf("the mount point was removed: %v", err)
	}
}

// --- the deletion budget ------------------------------------------------------

var five = []string{"sub/1", "sub/2", "sub/3", "sub/4", "sub/5"}

func TestVanishedSubtreeIsHeldNotPublished(t *testing.T) {
	w := auditWatcher(t)
	w.SetMaxExternalDeletions(3)
	writeFiles(t, w.dir, append([]string{"keep.txt"}, five...)...)
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(w.dir, "sub")); err != nil {
		t.Fatal(err)
	}

	err := w.periodicScan(context.Background())
	if !errors.Is(err, errHeldDeletions) {
		t.Fatalf("five missing files with a budget of three: %v", err)
	}
	status := w.Status()
	want := "holding 5 external deletions (limit 3 per 10 minutes): restore the files, or restart with sync.max_external_deletions raised to accept them"
	if status.Ready || status.LastError != want || status.HeldExternalDeletions != 5 {
		t.Fatalf("a held group must be a visible fault: %+v", status)
	}
	if n := countDeleted(t, w, append([]string{"keep.txt"}, five...)...); n != 0 {
		t.Fatalf("%d of a held group were published", n)
	}

	// Three come back: the two still missing fit the budget, are published,
	// and the fault clears by itself.
	writeFiles(t, w.dir, five[:3]...)
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = w.Status()
	if !status.Ready || status.LastError != "" || status.HeldExternalDeletions != 0 {
		t.Fatalf("fault did not clear once the group fit: %+v", status)
	}
	if n := countDeleted(t, w, five[3:]...); n != 2 {
		t.Fatalf("published %d of the 2 files still missing", n)
	}
	if n := countDeleted(t, w, five[:3]...); n != 0 {
		t.Fatalf("%d restored files were published as deleted", n)
	}
}

// A node that starts with part of its tree missing does not start replicating,
// and an operator who means it raises the budget and restarts.
func TestHeldGroupAtStartupWaitsForTheOperator(t *testing.T) {
	previous := auditWatcher(t)
	writeFiles(t, previous.dir, five...)
	if err := previous.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(previous.dir, "sub")); err != nil {
		t.Fatal(err)
	}

	w := New(previous.dir, previous.store, previous.logger, time.Millisecond, time.Hour, nil)
	w.SetMaxExternalDeletions(3)
	if err := w.periodicScan(context.Background()); !errors.Is(err, errHeldDeletions) {
		t.Fatalf("startup scan: %v", err)
	}
	select {
	case <-w.Ready():
		t.Fatal("a node holding deletions released the syncer")
	default:
	}

	raised := New(previous.dir, previous.store, previous.logger, time.Millisecond, time.Hour, nil)
	raised.SetMaxExternalDeletions(10)
	if err := raised.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-raised.Ready():
	default:
		t.Fatal("accepted deletions did not release the syncer")
	}
	if n := countDeleted(t, raised, five...); n != 5 {
		t.Fatalf("published %d of 5 accepted deletions", n)
	}
}

func TestEventBurstIsDecidedAsOneGroup(t *testing.T) {
	w := auditWatcher(t)
	w.SetMaxExternalDeletions(3)
	writeFiles(t, w.dir, append([]string{"a", "b"}, five...)...)
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Two deletions fit and are published when the burst ends.
	for _, name := range []string{"a", "b"} {
		if err := os.Remove(filepath.Join(w.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	w.processBatch(context.Background(), []string{"a", "b"})
	if n := countDeleted(t, w, "a", "b"); n != 2 {
		t.Fatalf("published %d of 2 deletions within budget", n)
	}

	// Five more do not fit what is left, and none of them is published.
	if err := os.RemoveAll(filepath.Join(w.dir, "sub")); err != nil {
		t.Fatal(err)
	}
	w.processBatch(context.Background(), five)
	status := w.Status()
	if status.Ready || status.HeldExternalDeletions != 5 || !strings.Contains(status.LastError, "holding 5 external deletions") {
		t.Fatalf("an oversized burst was not held: %+v", status)
	}
	if n := countDeleted(t, w, five...); n != 0 {
		t.Fatalf("%d of a held burst were published", n)
	}
}

// The budget is per window, not per group: a node never publishes more than
// the limit within ten minutes, across sweeps and across restarts.
func TestBudgetCoversTheWindowAcrossRestarts(t *testing.T) {
	w := auditWatcher(t)
	w.SetMaxExternalDeletions(3)
	writeFiles(t, w.dir, "a", "b", "c", "d")
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.Remove(filepath.Join(w.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}

	restarted := New(w.dir, w.store, w.logger, time.Millisecond, time.Hour, nil)
	restarted.SetMaxExternalDeletions(3)
	clock := time.Now()
	restarted.now = func() time.Time { return clock }
	for _, name := range []string{"c", "d"} {
		if err := os.Remove(filepath.Join(w.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := restarted.periodicScan(context.Background()); !errors.Is(err, errHeldDeletions) {
		t.Fatalf("two more within the window of two already published: %v", err)
	}
	if status := restarted.Status(); !strings.Contains(status.LastError, "2 already published") {
		t.Fatalf("message does not explain the window: %q", status.LastError)
	}
	if n := countDeleted(t, restarted, "c", "d"); n != 0 {
		t.Fatalf("published %d past the budget", n)
	}

	clock = clock.Add(externalDeletionWindow + time.Second)
	if err := restarted.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := restarted.Status(); !status.Ready || status.HeldExternalDeletions != 0 {
		t.Fatalf("a new window did not publish the group: %+v", status)
	}
	if n := countDeleted(t, restarted, "c", "d"); n != 2 {
		t.Fatalf("published %d of 2", n)
	}
}

// Deletions a client asked for are explicit and never count.
func TestGatewayDeletionsAreNotBudgeted(t *testing.T) {
	w := auditWatcher(t)
	w.SetMaxExternalDeletions(1)
	writeFiles(t, w.dir, append([]string{"loose"}, five...)...)
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fileops.Remove(w.dir, filepath.Join(w.dir, "sub"), true); err != nil {
		t.Fatal(err)
	}
	if err := fileops.Remove(w.dir, filepath.Join(w.dir, "loose"), false); err != nil {
		t.Fatal(err)
	}
	if n := countDeleted(t, w, append([]string{"loose"}, five...)...); n != 6 {
		t.Fatalf("published %d of 6 gateway deletions", n)
	}
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := w.Status(); !status.Ready || status.HeldExternalDeletions != 0 {
		t.Fatalf("gateway deletions tripped the breaker: %+v", status)
	}
}

// What was found missing is checked again under the commit lock when it is
// published: a file that came back in between is indexed, not deleted.
func TestPublishingRechecksEachName(t *testing.T) {
	w := auditWatcher(t)
	writeFiles(t, w.dir, "back", "gone")
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(w.dir, "gone")); err != nil {
		t.Fatal(err)
	}
	group := newDeletionGroup(10)
	group.add("back")
	group.add("gone")
	if err := w.settleDeletions(context.Background(), group, true); err != nil {
		t.Fatal(err)
	}
	if n := countDeleted(t, w, "back"); n != 0 {
		t.Fatal("a file present at publication was published as deleted")
	}
	if n := countDeleted(t, w, "gone"); n != 1 {
		t.Fatal("a file still missing was not published")
	}
}

// The scrub reads bytes; it does not decide what is gone.
func TestScrubDoesNotInferDeletions(t *testing.T) {
	w := auditWatcher(t)
	w.SetScrubRate(1 << 30)
	writeFiles(t, w.dir, "victim")
	if err := w.periodicScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(w.dir, "victim")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.scrubLoop(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for w.Status().LastScrubAgoMS < 0 {
		if time.Now().After(deadline) {
			t.Fatal("scrub did not complete a cycle")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := countDeleted(t, w, "victim"); n != 0 {
		t.Fatal("the scrub published a deletion")
	}
}

func TestDeletionGroupKeepsOnlyWhatItCanPublish(t *testing.T) {
	g := newDeletionGroup(2)
	for i := range 5 {
		g.add(fmt.Sprint(i))
	}
	g.add("0")
	if g.count != 5 || len(g.names) != 2 {
		t.Fatalf("count=%d names=%v", g.count, g.names)
	}
	other := newDeletionGroup(2)
	other.add("1")
	other.add("x")
	other.add("y")
	g.merge(other)
	if g.count != 7 {
		t.Fatalf("merged count=%d", g.count)
	}
}
