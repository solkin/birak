package fileops

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// CheckSpelling rejects a physical case alias before publication or deletion.
// The metadata reservation handles indexed names; this also protects existing
// files that the watcher has not indexed yet. Caller holds the volume lock.
func CheckSpelling(dir, path string) error {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	parentPath, err := ResolvePath(filepath.Dir(path))
	if err != nil {
		return err
	}
	path = filepath.Join(parentPath, filepath.Base(path))
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path outside storage")
	}
	parent := dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		full := filepath.Join(parent, part)
		info, err := os.Lstat(full)
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		if err != nil {
			return err
		}
		// Case-sensitive filesystems need no directory walk for ordinary ASCII
		// names. On Windows also check aliases such as short (8.3) names.
		alternate := strings.ToUpper(part)
		if alternate == part {
			alternate = strings.ToLower(part)
		}
		check := runtime.GOOS == "windows" || strings.IndexFunc(part, func(r rune) bool { return r > 127 }) >= 0
		if alternate != part {
			other, statErr := os.Lstat(filepath.Join(parent, alternate))
			check = check || statErr == nil && os.SameFile(info, other)
		}
		if check {
			f, err := os.Open(parent)
			if err != nil {
				return err
			}
			found := false
			for !found {
				entries, readErr := f.ReadDir(256)
				for _, entry := range entries {
					if entry.Name() == part {
						found = true
						break
					}
				}
				if readErr != nil {
					if readErr != io.EOF {
						f.Close()
						return readErr
					}
					break
				}
			}
			f.Close()
			if !found {
				return fmt.Errorf("physical namespace alias: requested %q is not its stored spelling", full)
			}
		}
		parent = full
	}
	return nil
}
