//go:build windows

package storagehelper

import (
	"fmt"
	"os"
	"syscall"

	"github.com/local/replicaro/appdata"
	"golang.org/x/sys/windows"
)

func validateManagedDirectory(path string, _ bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || hasReparsePoint(info) {
		return fmt.Errorf("managed component path is not a real directory")
	}
	return nil
}

func inspectRegularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || hasReparsePoint(info) {
		return nil, fmt.Errorf("component executable is not a regular file")
	}
	return info, nil
}

func hasReparsePoint(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func protectExecutable(path string) error {
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return appdata.SecurePath(path, false)
}
