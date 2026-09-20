//go:build windows

package vaultprofile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/local/replicaro/appdata"
	"golang.org/x/sys/windows"
)

func windowsRotationSafe(info os.FileInfo, directory bool) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 && info.IsDir() == directory && (directory || info.Mode().IsRegular())
}

func validatePasswordRotationAncestors(target string) error {
	absolute, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	if volume == "" {
		return fmt.Errorf("password-change staging root has no volume")
	}
	current := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(absolute[len(volume):], string(filepath.Separator))
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !windowsRotationSafe(info, true) {
			return fmt.Errorf("password-change staging ancestor is reparse or has the wrong type")
		}
	}
	return nil
}

func preparePasswordRotationDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	created := false
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return false, err
		}
		created = true
		info, err = os.Lstat(path)
	}
	if err != nil {
		return false, err
	}
	if !windowsRotationSafe(info, true) {
		return false, fmt.Errorf("password-change staging component is reparse or has the wrong type")
	}
	if err := appdata.SecurePath(path, true); err != nil {
		return false, err
	}
	return created, validatePasswordRotationDirectory(path)
}

func validatePasswordRotationDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !windowsRotationSafe(info, true) {
		return fmt.Errorf("password-change staging directory is reparse or has the wrong type")
	}
	return nil
}

func validatePasswordRotationFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !windowsRotationSafe(info, false) {
		return fmt.Errorf("password-change staging file is reparse or has the wrong type")
	}
	return nil
}

func readPasswordRotationFileBounded(path string, limit int64) ([]byte, error) {
	if err := validatePasswordRotationFile(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || info.Size() > limit {
		_ = file.Close()
		return nil, fmt.Errorf("password-change staging file exceeds its size limit")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("password-change staging file exceeds its size limit")
	}
	return data, nil
}

func syncPasswordRotationDirectory(path string) error {
	// Windows has no supported directory equivalent of fsync. Every staged
	// file is flushed before its directory publication reaches this boundary;
	// retain the exact non-reparse directory check without treating the native
	// ERROR_ACCESS_DENIED from FlushFileBuffers on a directory as data loss.
	if err := validatePasswordRotationDirectory(path); err != nil {
		return err
	}
	return nil
}
