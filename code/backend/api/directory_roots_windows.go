//go:build windows

package api

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// getLogicalDrives and getDriveType are variables only so tests can supply a
// drive table.
var getLogicalDrives = windows.GetLogicalDrives
var getDriveType = windows.GetDriveType

// directoryRoots lists drive letters without touching any drive. Calling Stat
// on every letter would wait on each disconnected mapped drive or empty card
// reader on every picker request, and that delay would count against
// whichever folder was being opened, making a healthy folder look
// unresponsive. GetLogicalDrives and GetDriveType read the letter table and
// the drive type without accessing the media or the network share, so a dead
// drive is still listed but only its own listing can be slow (and that one is
// bounded by directoryListingTimeout). Letters with no root directory or an
// unknown type are skipped. An empty card reader or optical drive is listed;
// opening it reports its own error.
func directoryRoots() []directoryEntry {
	mask, err := getLogicalDrives()
	if err != nil {
		return []directoryEntry{}
	}
	roots := make([]directoryEntry, 0, 4)
	for index := 0; index < 26; index++ {
		if mask&(1<<uint(index)) == 0 {
			continue
		}
		path := fmt.Sprintf("%c:\\", 'A'+index)
		root, err := windows.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		switch getDriveType(root) {
		case windows.DRIVE_UNKNOWN, windows.DRIVE_NO_ROOT_DIR:
			continue
		}
		roots = append(roots, directoryEntry{Name: path, Path: path})
	}
	return roots
}
