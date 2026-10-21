//go:build windows

package server

import "golang.org/x/sys/windows"

// diskSpace reports the bytes available to this process and the volume size.
func diskSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	err = windows.GetDiskFreeSpaceEx(p, &free, &total, nil)
	return free, total, err
}
