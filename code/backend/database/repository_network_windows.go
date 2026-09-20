//go:build windows

package database

import (
	"strings"

	"golang.org/x/sys/windows"
)

func isNetworkFilesystemLocation(connector, location string) bool {
	if !strings.EqualFold(strings.TrimSpace(connector), "fs") {
		return false
	}
	if isUNCFilesystemLocation(location) {
		return true
	}

	normalized := strings.TrimSpace(strings.ReplaceAll(location, "/", "\\"))
	if len(normalized) < 2 || normalized[1] != ':' {
		return false
	}
	root, err := windows.UTF16PtrFromString(strings.ToUpper(normalized[:1]) + ":\\")
	if err != nil {
		return false
	}
	return windows.GetDriveType(root) == windows.DRIVE_REMOTE
}
