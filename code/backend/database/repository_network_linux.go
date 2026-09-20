//go:build linux

package database

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func isNetworkFilesystemLocation(connector, location string) bool {
	if !strings.EqualFold(strings.TrimSpace(connector), "fs") {
		return false
	}
	path := filepath.Clean(location)
	if path == "." || path == "" {
		return false
	}
	if isUNCFilesystemLocation(path) {
		return true
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err == nil {
		switch uint64(stat.Type) {
		case 0x6969, 0xff534d42, 0xfe534d42, 0x65735546: // NFS, CIFS/SMB, FUSE
			return true
		}
	}
	// Statfs may be unavailable for an inaccessible mount.  Consult the
	// kernel mount table as a best-effort informational fallback.
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer file.Close()
	for scanner := bufio.NewScanner(file); scanner.Scan(); {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 5 || len(fields) <= separator+1 {
			continue
		}
		mountpoint := strings.ReplaceAll(fields[4], `\040`, " ")
		mountpoint = strings.ReplaceAll(mountpoint, `\011`, "\t")
		if path == mountpoint || strings.HasPrefix(path, mountpoint+string(filepath.Separator)) {
			switch fields[separator+1] {
			case "nfs", "nfs4", "cifs", "smbfs", "fuse", "fuseblk", "sshfs", "davfs":
				return true
			}
		}
	}
	return false
}
