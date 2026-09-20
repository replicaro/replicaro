//go:build !windows

package appdata

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func atomicReplace(source, destination string) error {
	if err := validateManagedAncestors(destination); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("atomic replacement source must be a regular file")
	}
	mode := info.Mode().Perm()
	if mode != 0o600 && mode != 0o700 {
		mode = 0o600
	}
	if err := os.Chmod(source, mode); err != nil {
		return err
	}
	if sourceStat, ok := info.Sys().(*syscall.Stat_t); ok {
		if directoryInfo, statErr := os.Stat(filepath.Dir(destination)); statErr == nil {
			if destinationStat, ok := directoryInfo.Sys().(*syscall.Stat_t); ok && sourceStat.Dev != destinationStat.Dev {
				return fmt.Errorf("atomic replacement crosses filesystems")
			}
		}
	}
	if destinationInfo, statErr := os.Lstat(destination); statErr == nil && (destinationInfo.Mode()&os.ModeSymlink != 0 || !destinationInfo.Mode().IsRegular()) {
		return fmt.Errorf("atomic replacement destination must be a regular file")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	if err := os.Chmod(destination, mode); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
