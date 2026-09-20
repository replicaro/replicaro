//go:build !windows

package operationlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/local/replicaro/appdata"
	"golang.org/x/sys/unix"
)

func operationLogDirectory() (string, error) {
	return appdata.Directory("logs")
}

func createStageFile(directory, operationID, kind, stream string) (*os.File, error) {
	safeKind := strings.NewReplacer("/", "_", "\\", "_", "\x00", "_").Replace(kind)
	file, err := os.CreateTemp(directory, "."+operationID+"."+safeKind+"."+stream+"-*.tmp")
	if err != nil {
		return nil, err
	}
	if filepath.Dir(file.Name()) != filepath.Clean(directory) {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
		return nil, fmt.Errorf("operation-log stage escaped its directory")
	}
	if err := validateFile(file); err != nil {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
		return nil, err
	}
	return file, nil
}

func openAppend(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := validateFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openRead(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := validateFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func removeOperationLogFile(path string) error { return os.Remove(path) }

func cleanupStagedOutputFiles(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var cleanupErrors []error
	for _, entry := range entries {
		name := entry.Name()
		if !stageFilename(name) {
			continue
		}
		path := filepath.Join(directory, name)
		file, openErr := openRead(path)
		if openErr != nil {
			cleanupErrors = append(cleanupErrors, openErr)
			continue
		}
		closeErr := file.Close()
		if closeErr != nil {
			cleanupErrors = append(cleanupErrors, closeErr)
			continue
		}
		if removeErr := removeOperationLogFile(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, removeErr)
		}
	}
	return errors.Join(cleanupErrors...)
}

func validateFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("operation log must be a user-owned single-link regular file")
	}
	return nil
}
