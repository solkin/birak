package fileops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type replacement struct {
	Destination    string `json:"destination"`
	Source         string `json:"source,omitempty"` // nonempty for MOVE; restore it on rollback
	Backup         string `json:"backup"`
	HadDestination bool   `json:"had_destination"`
	Committed      bool   `json:"committed"`
}

// ReplaceLocked journals an overwrite before moving the previous destination.
// The caller holds the root lock, including while op builds the new destination.
func ReplaceLocked(root, src, dst string, op func() error) error {
	return replaceLocked(root, src, dst, op, func(string) {})
}
func replaceLocked(root, src, dst string, op func() error, checkpoint func(string)) error {
	root, _ = filepath.Abs(root)
	dst, _ = filepath.Abs(dst)
	if src != "" {
		src, _ = filepath.Abs(src)
	}
	state := replacement{Destination: dst, Source: src}
	_, err := os.Lstat(dst)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	state.HadDestination = err == nil
	backup, err := os.CreateTemp(filepath.Dir(dst), ".birak-bak-*")
	if err != nil {
		return err
	}
	state.Backup = backup.Name()
	if err = errors.Join(backup.Close(), os.Remove(backup.Name())); err != nil {
		return err
	}
	journalDir := filepath.Join(root, ".birak", "transactions")
	if err = os.MkdirAll(journalDir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(journalDir, ".birak-tmp-journal-*")
	if err != nil {
		return err
	}
	journal := filepath.Join(journalDir, "replace-"+strings.TrimPrefix(filepath.Base(file.Name()), ".birak-tmp-journal-")+".json")
	defer os.Remove(file.Name())
	if err = file.Close(); err != nil {
		return err
	}
	if err = writeReplacement(journal, state); err != nil {
		os.Remove(journal)
		return err
	}
	checkpoint("prepared")
	if state.HadDestination {
		if err = os.Rename(dst, state.Backup); err != nil {
			return errors.Join(err, os.Remove(journal))
		}
		if err = SyncDir(filepath.Dir(dst)); err != nil {
			return errors.Join(err, recoverReplacement(journal, state))
		}
	}
	checkpoint("backed-up")
	if err = op(); err != nil {
		return errors.Join(err, recoverReplacement(journal, state))
	}
	if err = errors.Join(SyncSurvivingParent(dst, root), syncMoveSource(src, root)); err != nil {
		return errors.Join(err, recoverReplacement(journal, state))
	}
	checkpoint("published")
	state.Committed = true
	if err = writeReplacement(journal, state); err != nil {
		// The disk record is authoritative if replacing it succeeded but fsync failed.
		return err
	}
	checkpoint("committed")
	if err := recoverReplacement(journal, state); err != nil {
		return err
	}
	checkpoint("cleaned")
	return nil
}
func syncMoveSource(src, root string) error {
	if src == "" {
		return nil
	}
	return SyncSurvivingParent(src, root)
}
func writeReplacement(path string, state replacement) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".birak-tmp-journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}
func recoverReplacement(journal string, state replacement) error {
	if !state.Committed {
		_, err := os.Lstat(state.Backup)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil || !state.HadDestination {
			if state.Source != "" {
				if _, err := os.Lstat(state.Source); os.IsNotExist(err) {
					if _, err := os.Lstat(state.Destination); err == nil {
						if err = os.Rename(state.Destination, state.Source); err != nil {
							return err
						}
					} else if !os.IsNotExist(err) {
						return err
					}
				} else if err != nil {
					return err
				}
			}
			if err = os.RemoveAll(state.Destination); err != nil {
				return err
			}
			if state.HadDestination {
				if err = os.Rename(state.Backup, state.Destination); err != nil {
					return err
				}
			}
		}
	}
	if err := os.RemoveAll(state.Backup); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(state.Destination)); err != nil {
		return err
	}
	if state.Source != "" {
		if err := SyncDir(filepath.Dir(state.Source)); err != nil {
			return err
		}
	}
	if err := os.Remove(journal); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(journal))
}

// RecoverLocked runs before indexing or accepting mutations on startup. Recovery
// is idempotent: a second process termination during rollback can be retried.
func RecoverLocked(root string) error {
	dir := filepath.Join(root, ".birak", "transactions")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "replace-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var state replacement
		if err = json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("read replacement journal %s: %w", path, err)
		}
		for _, p := range []string{state.Destination, state.Backup, state.Source} {
			if p == "" {
				continue
			}
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("invalid replacement journal path %q", p)
			}
		}
		if err = recoverReplacement(path, state); err != nil {
			return fmt.Errorf("recover replacement %s: %w", path, err)
		}
	}
	return nil
}

// Old releases did not journal backup destinations. Refuse to index deletions
// while an unmapped backup exists: guessing or sweeping it can destroy data.
func CheckOrphanedBackups(root string, ignore func(string) bool) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root && strings.HasPrefix(d.Name(), ".birak-bak-") {
			return fmt.Errorf("unmapped recovery backup %s; restore it to its original path before restarting", path)
		}
		rel, _ := filepath.Rel(root, path)
		if path != root && d.IsDir() && (d.Name() == ".birak" || (ignore != nil && ignore(filepath.ToSlash(rel)))) {
			return filepath.SkipDir
		}
		return nil
	})
}
