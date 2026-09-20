//go:build !windows

package vaultprofile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/local/replicaro/appdata"
)

func validatePasswordRotationAncestors(target string) error {
	absolute, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	for _, component := range splitRotationPath(absolute) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("password-change staging ancestor is linked or has the wrong type")
		}
	}
	return nil
}

func splitRotationPath(path string) []string {
	var result []string
	for clean := filepath.Clean(path); clean != string(filepath.Separator) && clean != "."; clean = filepath.Dir(clean) {
		result = append([]string{filepath.Base(clean)}, result...)
	}
	return result
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
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("password-change staging component is linked or has the wrong type")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return false, fmt.Errorf("password-change staging component has the wrong owner")
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
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("password-change staging directory is linked, special, or not private")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("password-change staging directory has the wrong owner")
	}
	return nil
}

func validatePasswordRotationFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("password-change staging file is linked, special, or not private")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 {
		return fmt.Errorf("password-change staging file has the wrong owner or link count")
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
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errorsJoin(directory.Sync(), directory.Close())
}

func errorsJoin(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
