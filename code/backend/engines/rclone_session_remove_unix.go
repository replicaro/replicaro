//go:build !windows

package engines

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openUnixDirectory(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(string(filepath.Separator),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), string(filepath.Separator))
	components := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		if err := rejectUnixOpenedCaseAlias(current, component); err != nil {
			_ = current.Close()
			return nil, err
		}
		nextFD, openErr := unix.Openat(int(current.Fd()), component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = current.Close()
			if errors.Is(openErr, unix.ELOOP) {
				return nil, invalidRcloneLocalBinding(fmt.Errorf("private rclone path contains a symbolic link"))
			}
			if errors.Is(openErr, unix.ENOTDIR) {
				return nil, invalidRcloneLocalBinding(fmt.Errorf("private rclone path contains a symbolic link or non-directory component"))
			}
			return nil, openErr
		}
		next := os.NewFile(uintptr(nextFD), filepath.Join(current.Name(), component))
		if closeErr := current.Close(); closeErr != nil {
			_ = next.Close()
			return nil, closeErr
		}
		current = next
	}
	return current, nil
}

func rejectUnixOpenedCaseAlias(parent *os.File, exact string) error {
	fd, err := unix.Openat(
		int(parent.Fd()), ".",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	copy := os.NewFile(uintptr(fd), parent.Name())
	entries, readErr := copy.ReadDir(-1)
	closeErr := copy.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	matches := 0
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), exact) {
			matches++
			if entry.Name() != exact {
				return invalidRcloneLocalBinding(fmt.Errorf(
					"private rclone path has a case-conflicting alias: %s",
					filepath.Join(parent.Name(), entry.Name()),
				))
			}
		}
	}
	if matches > 1 {
		return invalidRcloneLocalBinding(fmt.Errorf(
			"private rclone path is case-ambiguous below %s", parent.Name(),
		))
	}
	return nil
}

func unixSameEntry(parent *os.File, name string, opened *os.File) (bool, error) {
	var actual, expected unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &actual, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, err
	}
	if err := unix.Fstat(int(opened.Fd()), &expected); err != nil {
		return false, err
	}
	return actual.Dev == expected.Dev && actual.Ino == expected.Ino, nil
}

func unixSameFile(left, right *os.File) (bool, error) {
	var leftStat, rightStat unix.Stat_t
	if err := unix.Fstat(int(left.Fd()), &leftStat); err != nil {
		return false, err
	}
	if err := unix.Fstat(int(right.Fd()), &rightStat); err != nil {
		return false, err
	}
	return leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino, nil
}

func openUnixChild(parent *os.File, name string) (*os.File, fs.FileMode, error) {
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, 0, invalidRcloneLocalBinding(fmt.Errorf("private rclone path contains a symbolic link"))
		}
		return nil, 0, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		_ = file.Close()
		return nil, 0, invalidRcloneLocalBinding(fmt.Errorf("rclone cleanup child is linked or special: %s", file.Name()))
	}
	if info.Mode().IsRegular() {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = file.Close()
			return nil, 0, err
		}
		if stat.Nlink != 1 {
			_ = file.Close()
			return nil, 0, invalidRcloneLocalBinding(fmt.Errorf("rclone cleanup child is multiply linked: %s", file.Name()))
		}
	}
	return file, info.Mode(), nil
}

func removeUnixOpenedTreeAt(parent *os.File, name string, child *os.File, mode fs.FileMode) error {
	if mode.IsDir() {
		entries, err := child.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			opened, childMode, err := openUnixChild(child, entry.Name())
			if err != nil {
				return err
			}
			if hook := rcloneCleanupChildOpenForTest; hook != nil {
				hook(opened.Name())
			}
			removeErr := removeUnixOpenedTreeAt(child, entry.Name(), opened, childMode)
			closeErr := opened.Close()
			if err := errors.Join(removeErr, closeErr); err != nil {
				return err
			}
		}
	}
	same, err := unixSameEntry(parent, name, child)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("rclone cleanup entry identity changed: %s", child.Name())
	}
	flags := 0
	if mode.IsDir() {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, flags); err != nil {
		return err
	}
	return nil
}

func removeRcloneSessionTreeSecure(root string) error {
	parentPath, name, err := recognizedRcloneSessionPath(root)
	if err != nil {
		return err
	}
	parent, err := openUnixDirectory(parentPath)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	opened, mode, err := openUnixChild(parent, name)
	if err != nil {
		closeErr := parent.Close()
		if errors.Is(err, unix.ENOENT) {
			return closeErr
		}
		return errors.Join(err, closeErr)
	}
	if hook := rcloneCleanupAfterOpenForTest; hook != nil {
		hook(root)
	}
	currentParent, err := openUnixDirectory(parentPath)
	if err != nil {
		return errors.Join(
			fmt.Errorf("revalidate rclone cleanup ancestors: %w", err),
			opened.Close(),
			parent.Close(),
		)
	}
	sameParent, compareErr := unixSameFile(parent, currentParent)
	closeErr := currentParent.Close()
	if err := errors.Join(compareErr, closeErr); err != nil {
		return errors.Join(err, opened.Close(), parent.Close())
	}
	if !sameParent {
		return errors.Join(
			fmt.Errorf("rclone cleanup ancestor identity changed"),
			opened.Close(),
			parent.Close(),
		)
	}
	removeErr := removeUnixOpenedTreeAt(parent, name, opened, mode)
	return errors.Join(removeErr, opened.Close(), parent.Close())
}

func cleanupRcloneSessionsSecure(parentPath string) error {
	parent, err := openUnixDirectory(parentPath)
	if err != nil {
		return err
	}
	entries, err := parent.ReadDir(-1)
	if err != nil {
		return errors.Join(err, parent.Close())
	}
	for _, entry := range entries {
		name := entry.Name()
		if !validRcloneAuthorizationDirectoryName(name) &&
			!validRcloneOperationDirectoryName(name) &&
			!validRcloneConfigDirectoryName(name) {
			continue
		}
		opened, mode, err := openUnixChild(parent, name)
		if err != nil {
			return errors.Join(
				fmt.Errorf("inspect stale rclone session: %w", err),
				parent.Close(),
			)
		}
		if !mode.IsDir() {
			_ = opened.Close()
			return errors.Join(
				fmt.Errorf("rclone session candidate is not a directory: %s", opened.Name()),
				parent.Close(),
			)
		}
		currentParent, err := openUnixDirectory(parentPath)
		if err != nil {
			return errors.Join(
				fmt.Errorf("revalidate stale rclone session ancestors: %w", err),
				opened.Close(),
				parent.Close(),
			)
		}
		sameParent, compareErr := unixSameFile(parent, currentParent)
		currentCloseErr := currentParent.Close()
		if err := errors.Join(compareErr, currentCloseErr); err != nil {
			return errors.Join(err, opened.Close(), parent.Close())
		}
		if !sameParent {
			return errors.Join(
				fmt.Errorf("stale rclone session ancestor identity changed"),
				opened.Close(),
				parent.Close(),
			)
		}
		removeErr := removeUnixOpenedTreeAt(parent, name, opened, mode)
		closeErr := opened.Close()
		if err := errors.Join(removeErr, closeErr); err != nil {
			return errors.Join(fmt.Errorf("remove stale rclone session: %w", err), parent.Close())
		}
	}
	return parent.Close()
}

func wipeRcloneConfigSecure(filename string) error {
	parent, err := openUnixDirectory(filepath.Dir(filename))
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("open private rclone config parent: %w", err)
	}
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(filename),
		unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return parent.Close()
		}
		return errors.Join(fmt.Errorf("open private rclone config: %w", err), parent.Close())
	}
	file := os.NewFile(uintptr(fd), filename)
	info, err := file.Stat()
	if err != nil {
		return errors.Join(err, file.Close(), parent.Close())
	}
	if !info.Mode().IsRegular() {
		return errors.Join(
			fmt.Errorf("private rclone config is linked or special"),
			file.Close(),
			parent.Close(),
		)
	}
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return errors.Join(err, file.Close(), parent.Close())
	}
	if before.Nlink != 1 {
		return errors.Join(
			fmt.Errorf("private rclone config is multiply linked"),
			file.Close(),
			parent.Close(),
		)
	}
	if hook := rcloneWipeAfterOpenForTest; hook != nil {
		hook(filename)
	}
	same, err := unixSameEntry(parent, filepath.Base(filename), file)
	if err != nil {
		return errors.Join(err, file.Close(), parent.Close())
	}
	if !same {
		return errors.Join(
			fmt.Errorf("private rclone config identity changed"),
			file.Close(),
			parent.Close(),
		)
	}
	if err := unix.Unlinkat(int(parent.Fd()), filepath.Base(filename), 0); err != nil {
		return errors.Join(
			fmt.Errorf("remove private rclone config name: %w", err),
			file.Close(),
			parent.Close(),
		)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return errors.Join(err, file.Close(), parent.Close())
	}
	if stat.Nlink != 0 {
		return errors.Join(
			fmt.Errorf("private rclone config is multiply linked after controlled-name removal"),
			file.Close(),
			parent.Close(),
		)
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		return errors.Join(
			fmt.Errorf("wipe private rclone config: %w", err),
			file.Close(),
			parent.Close(),
		)
	}
	return errors.Join(file.Sync(), file.Close(), parent.Close())
}
