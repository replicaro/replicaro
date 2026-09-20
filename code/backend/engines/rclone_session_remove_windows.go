//go:build windows

package engines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsFileDispositionInfo struct {
	DeleteFile byte
}

func openWindowsLocked(path string, access uint32, directory bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0) ||
		(!directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) ||
		(!directory && info.NumberOfLinks != 1) {
		_ = file.Close()
		return nil, invalidRcloneLocalBinding(fmt.Errorf("private rclone path is reparse, multiply linked, or has the wrong type: %s", path))
	}
	return file, nil
}

func openWindowsAncestorChain(path string) ([]*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(absolute)
	if volume == "" {
		return nil, fmt.Errorf("private rclone path has no volume")
	}
	current := volume + string(filepath.Separator)
	chain := []*os.File{}
	components := strings.Split(strings.TrimPrefix(absolute[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		if err := rejectRcloneCaseAlias(current, component); err != nil {
			for index := len(chain) - 1; index >= 0; index-- {
				_ = chain[index].Close()
			}
			return nil, err
		}
		current = filepath.Join(current, component)
		file, err := openWindowsLocked(current, windows.GENERIC_READ|windows.READ_CONTROL, true)
		if err != nil {
			for index := len(chain) - 1; index >= 0; index-- {
				_ = chain[index].Close()
			}
			return nil, err
		}
		chain = append(chain, file)
	}
	return chain, nil
}

func closeWindowsChain(chain []*os.File) error {
	var result error
	for index := len(chain) - 1; index >= 0; index-- {
		result = errors.Join(result, chain[index].Close())
	}
	return result
}

func deleteWindowsHandle(file *os.File) error {
	info := windowsFileDispositionInfo{DeleteFile: 1}
	return windows.SetFileInformationByHandle(
		windows.Handle(file.Fd()),
		windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
}

func openWindowsCleanupEntry(path string) (*os.File, bool, error) {
	directory, err := openWindowsLocked(path,
		windows.GENERIC_READ|windows.DELETE|windows.FILE_WRITE_ATTRIBUTES, true)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, false, err
		}
		file, fileErr := openWindowsLocked(path,
			windows.GENERIC_READ|windows.DELETE|windows.FILE_WRITE_ATTRIBUTES, false)
		if fileErr != nil {
			return nil, false, err
		}
		return file, false, nil
	}
	return directory, true, nil
}

func removeWindowsOpenedTree(path string, entry *os.File, directory bool) error {
	if !directory {
		return deleteWindowsHandle(entry)
	}
	entries, err := entry.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, childEntry := range entries {
		childPath := filepath.Join(path, childEntry.Name())
		child, childDirectory, err := openWindowsCleanupEntry(childPath)
		if err != nil {
			return err
		}
		if hook := rcloneCleanupChildOpenForTest; hook != nil {
			hook(childPath)
		}
		removeErr := removeWindowsOpenedTree(childPath, child, childDirectory)
		closeErr := child.Close()
		if err := errors.Join(removeErr, closeErr); err != nil {
			return err
		}
	}
	return deleteWindowsHandle(entry)
}

func removeRcloneSessionTreeSecure(root string) error {
	parent, _, err := recognizedRcloneSessionPath(root)
	if err != nil {
		return err
	}
	chain, err := openWindowsAncestorChain(parent)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	rootEntry, directory, err := openWindowsCleanupEntry(root)
	if err != nil {
		closeErr := closeWindowsChain(chain)
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return closeErr
		}
		return errors.Join(err, closeErr)
	}
	if !directory {
		return errors.Join(
			fmt.Errorf("rclone cleanup target is not a directory"),
			rootEntry.Close(),
			closeWindowsChain(chain),
		)
	}
	if hook := rcloneCleanupAfterOpenForTest; hook != nil {
		hook(root)
	}
	removeErr := removeWindowsOpenedTree(root, rootEntry, true)
	return errors.Join(removeErr, rootEntry.Close(), closeWindowsChain(chain))
}

func cleanupRcloneSessionsSecure(parentPath string) error {
	chain, err := openWindowsAncestorChain(filepath.Dir(parentPath))
	if err != nil {
		return err
	}
	parent, err := openWindowsLocked(parentPath,
		windows.GENERIC_READ|windows.READ_CONTROL, true)
	if err != nil {
		return errors.Join(err, closeWindowsChain(chain))
	}
	entries, err := parent.ReadDir(-1)
	if err != nil {
		return errors.Join(err, parent.Close(), closeWindowsChain(chain))
	}
	for _, entry := range entries {
		name := entry.Name()
		if !validRcloneAuthorizationDirectoryName(name) &&
			!validRcloneOperationDirectoryName(name) &&
			!validRcloneConfigDirectoryName(name) {
			continue
		}
		childPath := filepath.Join(parentPath, name)
		child, directory, err := openWindowsCleanupEntry(childPath)
		if err != nil {
			return errors.Join(
				fmt.Errorf("inspect stale rclone session: %w", err),
				parent.Close(),
				closeWindowsChain(chain),
			)
		}
		if !directory {
			_ = child.Close()
			return errors.Join(
				fmt.Errorf("rclone session candidate is not a directory: %s", childPath),
				parent.Close(),
				closeWindowsChain(chain),
			)
		}
		removeErr := removeWindowsOpenedTree(childPath, child, true)
		closeErr := child.Close()
		if err := errors.Join(removeErr, closeErr); err != nil {
			return errors.Join(
				fmt.Errorf("remove stale rclone session: %w", err),
				parent.Close(),
				closeWindowsChain(chain),
			)
		}
	}
	return errors.Join(parent.Close(), closeWindowsChain(chain))
}

func wipeRcloneConfigSecure(filename string) error {
	chain, err := openWindowsAncestorChain(filepath.Dir(filename))
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("open private rclone config parent: %w", err)
	}
	file, err := openWindowsLocked(filename,
		windows.GENERIC_READ|windows.GENERIC_WRITE, false)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return closeWindowsChain(chain)
		}
		return errors.Join(
			fmt.Errorf("open private rclone config: %w", err),
			closeWindowsChain(chain),
		)
	}
	if hook := rcloneWipeAfterOpenForTest; hook != nil {
		hook(filename)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return errors.Join(err, file.Close(), closeWindowsChain(chain))
	}
	if err := windows.SetEndOfFile(windows.Handle(file.Fd())); err != nil {
		return errors.Join(
			fmt.Errorf("wipe private rclone config: %w", err),
			file.Close(),
			closeWindowsChain(chain),
		)
	}
	return errors.Join(
		windows.FlushFileBuffers(windows.Handle(file.Fd())),
		file.Close(),
		closeWindowsChain(chain),
	)
}
