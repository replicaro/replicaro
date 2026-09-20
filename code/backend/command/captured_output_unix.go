//go:build !windows

package command

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/local/replicaro/appdata"
)

func capturedOutputDirectory() (string, error) { return appdata.Directory("command-output") }

func createCapturedOutputFile(directory, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return nil, err
	}
	if err := validateCapturedOutput(file); err != nil {
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

func openCapturedOutput(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open captured command output")
	}
	if err := validateCapturedOutput(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func validateCapturedOutput(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("captured command output must be an owner-only single-link regular file")
	}
	return nil
}

func removeCapturedOutput(path string) error { return os.Remove(path) }

func cleanupCapturedOutputDirectory(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var cleanupErrors []error
	for _, entry := range entries {
		name := entry.Name()
		if (!strings.HasPrefix(name, ".stdout-") && !strings.HasPrefix(name, ".stderr-")) ||
			!strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			cleanupErrors = append(cleanupErrors, infoErr)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if removeErr := removeCapturedOutput(filepath.Join(directory, name)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, removeErr)
		}
	}
	return errors.Join(cleanupErrors...)
}
