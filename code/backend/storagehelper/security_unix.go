//go:build !windows

package storagehelper

import (
	"fmt"
	"os"
	"syscall"
)

func validateManagedDirectory(path string, requireOwner bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("managed component path is not a real directory")
	}
	if requireOwner {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return fmt.Errorf("managed component directory has unexpected owner")
		}
	}
	return nil
}

func inspectRegularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("component executable is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("component executable has unexpected owner")
	}
	return info, nil
}

func protectExecutable(path string) error {
	return os.Chmod(path, 0o700)
}
