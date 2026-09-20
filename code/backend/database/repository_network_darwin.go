//go:build darwin

package database

import (
	"path/filepath"
	"strings"
	"syscall"
)

func isNetworkFilesystemLocation(connector, location string) bool {
	if !strings.EqualFold(strings.TrimSpace(connector), "fs") {
		return false
	}
	path := filepath.Clean(location)
	if isUNCFilesystemLocation(path) {
		return true
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false
	}
	nameBytes := make([]byte, 0, len(stat.Fstypename))
	for _, value := range stat.Fstypename {
		if value == 0 {
			break
		}
		nameBytes = append(nameBytes, byte(value))
	}
	name := string(nameBytes)
	switch strings.ToLower(name) {
	case "nfs", "smbfs", "webdav", "fuse", "sshfs":
		return true
	default:
		return false
	}
}
