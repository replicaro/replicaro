//go:build windows

package vaultprofile

import "golang.org/x/sys/windows"

func passwordRotationAvailableBytes(path string) (uint64, error) {
	value, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(value, &available, nil, nil); err != nil {
		return 0, err
	}
	return available, nil
}
