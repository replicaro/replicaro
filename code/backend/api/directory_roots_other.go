//go:build !windows

package api

import (
	"os"
	"runtime"
)

func directoryRoots() []directoryEntry {
	roots := []directoryEntry{{Name: "/", Path: "/"}}
	if runtime.GOOS == "darwin" {
		if info, err := os.Stat("/Volumes"); err == nil && info.IsDir() {
			roots = append(roots, directoryEntry{Name: "/Volumes", Path: "/Volumes"})
		}
	}
	return roots
}
