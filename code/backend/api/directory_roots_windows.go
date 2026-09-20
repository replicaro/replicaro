//go:build windows

package api

import (
	"fmt"
	"os"
)

func directoryRoots() []directoryEntry {
	roots := make([]directoryEntry, 0, 4)
	for drive := 'A'; drive <= 'Z'; drive++ {
		path := fmt.Sprintf("%c:\\", drive)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			roots = append(roots, directoryEntry{Name: path, Path: path})
		}
	}
	return roots
}
