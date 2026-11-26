package fileops

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncParentsRejectsOutsideRoot(t *testing.T) {
	rootDir := t.TempDir()
	outside := t.TempDir()
	if err := SyncParents(outside, rootDir); err == nil {
		t.Fatalf("SyncParents(%q, %q) succeeded, want boundary error", outside, rootDir)
	}
}

func TestCheckSpellingRelativeDir(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sub, "file.txt")
	if err := os.WriteFile(target, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	if err := CheckSpelling("sub", target); err != nil {
		t.Fatalf("CheckSpelling with relative dir failed: %v", err)
	}
}

func TestSameModTimeExtremeOverflow(t *testing.T) {
	dir := t.TempDir()
	SetModTimeGranularity(dir, time.Second)

	farPast := time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)
	farFuture := time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC)
	if SameModTime(dir, farPast, time.Now().UnixNano()) {
		t.Fatal("far past matched now")
	}
	if SameModTime(dir, farFuture, math.MinInt64) {
		t.Fatal("far future matched MinInt64")
	}
	now := time.Now().Truncate(time.Second)
	if !SameModTime(dir, now, now.Add(500*time.Millisecond).UnixNano()) {
		t.Fatal("timestamps within granularity did not match")
	}
}

func TestRecoverIfNeededLockedTracksDirtyState(t *testing.T) {
	dir := t.TempDir()
	unlock := Lock(dir)
	defer unlock()

	if err := RecoverIfNeededLocked(dir); err != nil {
		t.Fatalf("first RecoverIfNeededLocked: %v", err)
	}
	if !root(dir).recoveryClean.Load() {
		t.Fatal("expected recoveryClean=true after clean recovery")
	}

	// Stage and publish a replacement via ReplaceStagedLocked.
	stage, err := os.MkdirTemp(dir, ".birak-tmp-replace-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "payload"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceStagedLocked(dir, stage, dst); err != nil {
		t.Fatalf("ReplaceStagedLocked: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil || string(body) != "new" {
		t.Fatalf("dst after ReplaceStagedLocked = %q, %v", body, err)
	}
	if err := RecoverIfNeededLocked(dir); err != nil {
		t.Fatalf("post-replace RecoverIfNeededLocked: %v", err)
	}
	if !root(dir).recoveryClean.Load() {
		t.Fatal("expected recoveryClean=true after post-replace recovery")
	}
}
