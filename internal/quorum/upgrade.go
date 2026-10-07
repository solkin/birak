package quorum

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/generation"
)

// UpgradeV2 must run with every cluster process stopped. It preserves Raft
// history and data, and changes only the paired format markers. Repeating after
// a crash between replacements completes the same upgrade. Never downgrade.
func UpgradeV2(dir string, expected Identity) error {
	if dir == "" || expected.Cluster == "" || expected.Node == "" || expected.Format != Format {
		return errors.New("explicit directory, cluster and node required")
	}
	release, err := fileops.AcquireLease(dir)
	if err != nil {
		return err
	}
	defer release()
	paths := []string{filepath.Join(dir, "identity.json"), filepath.Join(dir, "generations", "identity.json")}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("identity must be a regular file")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var saved Identity
		if json.Unmarshal(b, &saved) != nil || saved.Cluster != expected.Cluster || saved.Node != expected.Node || (saved.Format != legacyFormat && saved.Format != Format) {
			return fmt.Errorf("identity mismatch: %s", path)
		}
	}
	for _, path := range []string{"raft.db", "snapshots", "generations", "generations/objects", "generations/staging"} {
		info, err := os.Lstat(filepath.Join(dir, path))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("upgrade refuses linked state")
		}
	}
	b, _ := json.Marshal(expected)
	for _, path := range paths {
		if err := replaceStateFile(path, b); err != nil {
			return err
		}
	}
	return nil
}
func replaceStateFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".upgrade-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return generation.SyncDir(filepath.Dir(path))
}
