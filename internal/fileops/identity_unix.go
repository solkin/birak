//go:build !windows

package fileops

import (
	"fmt"
	"os"
	"syscall"
)

func objectIdentity(path string, info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("filesystem object identity unavailable: %s", path)
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

// Device returns the filesystem device holding the entry info describes.
func Device(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Dev), true
}
