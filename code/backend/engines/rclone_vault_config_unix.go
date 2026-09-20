//go:build !windows

package engines

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/local/replicaro/appdata"
	"golang.org/x/sys/unix"
)

func rcloneVaultConfigSingleLink(_ string, info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func syncRcloneVaultDirectory(path string) error {
	directory, err := openUnixDirectory(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func inspectUnixRcloneConfigFile(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	// POSIX admission is based only on the opened file's final safety state.
	// Native same-directory replacements do not need a particular ACL origin,
	// inheritance form, or prior inode identity.
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return invalidRcloneLocalBinding(fmt.Errorf("rclone vault config is linked or special"))
	}
	if stat.Mode&0o077 != 0 || stat.Mode&0o700 != 0o600 {
		return fmt.Errorf("rclone vault config permissions are unsafe")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("rclone vault config owner is unsafe")
	}
	return nil
}

func openRcloneConfigFileReadOnly(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if absolute != filepath.Clean(path) {
		return nil, invalidRcloneLocalBinding(fmt.Errorf("rclone vault config path is not canonical"))
	}
	parent, err := openUnixDirectory(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	file, mode, err := openUnixChild(parent, filepath.Base(absolute))
	if err != nil {
		return nil, err
	}
	if !mode.IsRegular() {
		_ = file.Close()
		return nil, invalidRcloneLocalBinding(fmt.Errorf("rclone vault config is linked or special"))
	}
	if err := inspectUnixRcloneConfigFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	same, err := unixSameEntry(parent, filepath.Base(absolute), file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !same {
		_ = file.Close()
		return nil, fmt.Errorf("rclone vault config identity changed")
	}
	return file, nil
}

func inspectUnixOpenedStagedRcloneConfig(
	directory *os.File,
	name string,
) error {
	if err := rejectUnixOpenedCaseAlias(directory, name); err != nil {
		return err
	}
	config, mode, err := openUnixChild(directory, name)
	if err != nil {
		return err
	}
	defer config.Close()
	if !mode.IsRegular() {
		return invalidRcloneLocalBinding(fmt.Errorf("rclone vault config is linked or special"))
	}
	if err := inspectUnixRcloneConfigFile(config); err != nil {
		return err
	}
	if err := inspectBoundedRcloneVaultConfig(config); err != nil {
		return err
	}
	same, err := unixSameEntry(directory, name, config)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("rclone vault config identity changed")
	}
	return nil
}

func openStagedRcloneConfigGuard(path string) (*rcloneVaultConfigGuard, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if absolute != filepath.Clean(path) {
		return nil, invalidRcloneLocalBinding(fmt.Errorf("staged rclone config path is not canonical"))
	}
	guard := &rcloneVaultConfigGuard{
		path: absolute,
	}
	guard.inspect = func() error {
		directory, err := openUnixDirectory(filepath.Dir(absolute))
		if err != nil {
			return err
		}
		inspectErr := inspectUnixOpenedStagedRcloneConfig(
			directory, filepath.Base(absolute),
		)
		return errors.Join(inspectErr, directory.Close())
	}
	guard.close = func() error { return nil }
	if err := guard.inspect(); err != nil {
		_ = guard.close()
		return nil, err
	}
	return guard, nil
}

func hardenNewRcloneConfigFile(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent, err := openUnixDirectory(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	defer parent.Close()
	fd, err := unix.Openat(
		int(parent.Fd()), filepath.Base(absolute),
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), absolute)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return fmt.Errorf("new rclone vault config is linked or special")
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	same, err := unixSameEntry(parent, filepath.Base(absolute), file)
	if err != nil || !same {
		return fmt.Errorf("new rclone vault config identity changed")
	}
	return inspectUnixRcloneConfigFile(file)
}

func touchRcloneVaultSidecarConfig(
	ctx context.Context,
	binary, repositoryID, config, cache, temporary string,
) (*RcloneVaultConfigBinding, error) {
	root, directory, err := openUnixRcloneVault(repositoryID)
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		return nil, err
	}
	if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
		_ = directory.Close()
		_ = root.Close()
		return nil, err
	}
	if err := errors.Join(directory.Close(), root.Close()); err != nil {
		return nil, err
	}
	if _, err := runRcloneVaultConfigCommand(
		ctx, binary,
		[]string{"config", "touch", "--config", config,
			"--cache-dir", cache, "--temp-dir", temporary,
			"--log-level", "ERROR"},
		nil, "", time.Minute, RcloneComponentID,
	); err != nil {
		return nil, err
	}
	if err := hardenNewRcloneConfigFile(config); err != nil {
		return nil, err
	}
	binding := nonResticRcloneSidecarBinding(repositoryID, config)
	if err := binding.Revalidate(ctx); err != nil {
		return nil, err
	}
	return binding, nil
}

func inspectUnixRcloneVaultDirectory(directory *os.File) error {
	if directory == nil {
		return fmt.Errorf("rclone vault owned directory is unavailable")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o700 {
		return fmt.Errorf("rclone vault owned directory final state is unsafe")
	}
	return nil
}

func openUnixRcloneVaultRoot(rootPath string, create bool) (*os.File, error) {
	applicationRoot := filepath.Dir(filepath.Dir(rootPath))
	if create {
		if err := os.MkdirAll(filepath.Dir(applicationRoot), 0o700); err != nil {
			return nil, err
		}
	}
	anchor, err := openUnixDirectory(filepath.Dir(applicationRoot))
	if err != nil {
		return nil, err
	}
	opened := []*os.File{anchor}
	closeOpened := func() error {
		var result error
		for index := len(opened) - 1; index >= 0; index-- {
			result = errors.Join(result, opened[index].Close())
		}
		return result
	}
	current := anchor
	for _, name := range []string{
		filepath.Base(applicationRoot),
		filepath.Base(filepath.Dir(rootPath)),
		filepath.Base(rootPath),
	} {
		if err := rejectUnixOpenedCaseAlias(current, name); err != nil {
			_ = closeOpened()
			return nil, err
		}
		child, mode, err := openUnixChild(current, name)
		created := false
		if create && errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(int(current.Fd()), name, 0o700); err != nil {
				_ = closeOpened()
				return nil, err
			}
			created = true
			child, mode, err = openUnixChild(current, name)
		}
		if err != nil {
			_ = closeOpened()
			return nil, err
		}
		if !mode.IsDir() {
			_ = child.Close()
			_ = closeOpened()
			return nil, invalidRcloneLocalBinding(fmt.Errorf("rclone vault owned path component is not a directory"))
		}
		if created {
			if err := child.Chmod(0o700); err != nil {
				_ = child.Close()
				_ = closeOpened()
				return nil, err
			}
		}
		if err := inspectUnixRcloneVaultDirectory(child); err != nil {
			_ = child.Close()
			_ = closeOpened()
			return nil, err
		}
		same, err := unixSameEntry(current, name, child)
		if err != nil {
			_ = child.Close()
			_ = closeOpened()
			return nil, err
		}
		if !same {
			_ = child.Close()
			_ = closeOpened()
			return nil, fmt.Errorf("rclone vault owned directory identity changed")
		}
		opened = append(opened, child)
		current = child
	}
	root := opened[len(opened)-1]
	opened = opened[:len(opened)-1]
	if err := closeOpened(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func openUnixRcloneVault(repositoryID string) (root, directory *os.File, err error) {
	rootPath, err := appdata.RcloneVaultConfigRoot()
	if err != nil {
		return nil, nil, err
	}
	root, err = openUnixRcloneVaultRoot(rootPath, false)
	if err != nil {
		return nil, nil, err
	}
	if err := rejectUnixOpenedCaseAlias(root, repositoryID); err != nil {
		return root, nil, err
	}
	directory, mode, err := openUnixChild(root, repositoryID)
	if err != nil {
		return root, nil, err
	}
	if !mode.IsDir() {
		_ = directory.Close()
		return root, nil, invalidRcloneLocalBinding(fmt.Errorf("rclone vault config owner is not a directory"))
	}
	if err := inspectUnixRcloneVaultDirectory(directory); err != nil {
		_ = directory.Close()
		return root, nil, err
	}
	return root, directory, nil
}

func inspectUnixOpenedRcloneVaultConfig(
	root, directory *os.File,
	repositoryID string,
) error {
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	if _, err := directory.Seek(0, 0); err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.ErrNotExist
	}
	if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
		return invalidRcloneLocalBinding(fmt.Errorf(
			"rclone vault directory contains ambiguous native residue; reconnect or recover this vault",
		))
	}
	config, mode, err := openUnixChild(directory, "rclone.conf")
	if err != nil {
		return err
	}
	defer config.Close()
	if !mode.IsRegular() {
		return invalidRcloneLocalBinding(fmt.Errorf("rclone vault config is linked or special"))
	}
	if err := inspectUnixRcloneConfigFile(config); err != nil {
		return err
	}
	if err := inspectBoundedRcloneVaultConfig(config); err != nil {
		return err
	}
	sameConfig, err := unixSameEntry(directory, "rclone.conf", config)
	if err != nil {
		return err
	}
	if !sameConfig {
		return fmt.Errorf("rclone vault config identity changed")
	}
	if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
		return err
	}
	return nil
}

func openRcloneVaultConfigGuard(repositoryID string) (*rcloneVaultConfigGuard, error) {
	path, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return nil, err
	}
	guard := &rcloneVaultConfigGuard{
		path: path,
	}
	guard.inspect = func() error {
		return inspectRcloneVaultConfigSecure(repositoryID)
	}
	guard.close = func() error { return nil }
	if err := guard.inspect(); err != nil {
		_ = guard.close()
		return nil, err
	}
	return guard, nil
}

func newRcloneVaultPublication(repositoryID string) (*rcloneVaultPublication, error) {
	target, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return nil, err
	}
	rootPath := filepath.Dir(filepath.Dir(target))
	root, err := openUnixRcloneVaultRoot(rootPath, true)
	if err != nil {
		return nil, fmt.Errorf("prepare rclone vault config root: %w", err)
	}
	if err := rejectUnixOpenedCaseAlias(root, repositoryID); err != nil {
		_ = root.Close()
		return nil, err
	}
	directory, mode, openErr := openUnixChild(root, repositoryID)
	created := false
	if errors.Is(openErr, unix.ENOENT) {
		if err := unix.Mkdirat(int(root.Fd()), repositoryID, 0o700); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("create rclone vault directory: %w", err)
		}
		created = true
		directory, mode, openErr = openUnixChild(root, repositoryID)
	}
	err = openErr
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !mode.IsDir() {
		_ = directory.Close()
		_ = root.Close()
		return nil, fmt.Errorf("rclone vault directory is linked, special, or unavailable")
	}
	if created {
		err = directory.Chmod(0o700)
	}
	if err != nil {
		_ = directory.Close()
		_ = root.Close()
		return nil, fmt.Errorf("protect rclone vault directory")
	}
	if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
		_ = directory.Close()
		_ = root.Close()
		return nil, err
	}
	cleanupHandles := func() {
		if directory != nil {
			_ = directory.Close()
			directory = nil
		}
		if root != nil {
			_ = root.Close()
			root = nil
		}
	}
	if hook := rcloneVaultPublicationBeforeCreateForTest; hook != nil {
		if err := hook(filepath.Dir(target)); err != nil {
			cleanupHandles()
			return nil, err
		}
	}
	if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
		cleanupHandles()
		return nil, err
	}
	publication := &rcloneVaultPublication{target: target}
	publication.activate = func(staged string) (RcloneConfigDisposition, error) {
		if hook := rcloneVaultPublicationAfterOpenForTest; hook != nil {
			if err := hook(filepath.Dir(target)); err != nil {
				return RcloneConfigRetained, err
			}
		}
		if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
			return RcloneConfigRetained, err
		}
		return activateRcloneConfigCandidate(staged, directory, target)
	}
	publication.syncDirectory = func() error {
		if err := directory.Sync(); err != nil {
			return err
		}
		return revalidateUnixRcloneVault(root, directory, repositoryID)
	}
	publication.inspectTarget = func() error {
		if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
			return err
		}
		return validateRcloneVaultFile(target)
	}
	publication.close = func() error {
		var result error
		if directory != nil {
			result = directory.Close()
			directory = nil
		}
		if root != nil {
			result = errors.Join(result, root.Close())
			root = nil
		}
		return result
	}
	return publication, nil
}

func revalidateUnixRcloneVault(root, directory *os.File, repositoryID string) error {
	rootPath, err := appdata.RcloneVaultConfigRoot()
	if err != nil {
		return err
	}
	currentRoot, err := openUnixRcloneVaultRoot(rootPath, false)
	if err != nil {
		return err
	}
	sameRoot, compareErr := unixSameFile(root, currentRoot)
	closeErr := currentRoot.Close()
	if err := errors.Join(compareErr, closeErr); err != nil {
		return err
	}
	if !sameRoot {
		return fmt.Errorf("rclone vault config root identity changed")
	}
	if err := errors.Join(
		inspectUnixRcloneVaultDirectory(root),
		inspectUnixRcloneVaultDirectory(directory),
	); err != nil {
		return err
	}
	sameDirectory, err := unixSameEntry(root, repositoryID, directory)
	if err != nil {
		return err
	}
	if !sameDirectory {
		return fmt.Errorf("rclone vault directory identity changed")
	}
	return nil
}

func prepareRcloneVaultDirectorySecure(repositoryID string) (string, error) {
	config, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return "", err
	}
	rootPath := filepath.Dir(filepath.Dir(config))
	root, err := openUnixRcloneVaultRoot(rootPath, true)
	if err != nil {
		return "", fmt.Errorf("prepare rclone vault config root: %w", err)
	}
	defer root.Close()
	if err := rejectRcloneCaseAlias(rootPath, repositoryID); err != nil {
		return "", err
	}
	directory, mode, openErr := openUnixChild(root, repositoryID)
	created := false
	if errors.Is(openErr, unix.ENOENT) {
		if err := unix.Mkdirat(int(root.Fd()), repositoryID, 0o700); err != nil {
			return "", fmt.Errorf("create rclone vault directory: %w", err)
		}
		created = true
		directory, mode, openErr = openUnixChild(root, repositoryID)
	}
	err = openErr
	if err != nil {
		return "", err
	}
	defer directory.Close()
	if !mode.IsDir() {
		return "", fmt.Errorf("rclone vault directory is linked, special, or unavailable")
	}
	if created {
		err = directory.Chmod(0o700)
	}
	if err != nil {
		return "", fmt.Errorf("protect rclone vault directory")
	}
	if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
		return "", err
	}
	return config, nil
}

func inspectRcloneVaultConfigSecure(repositoryID string) error {
	root, directory, err := openUnixRcloneVault(repositoryID)
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		return err
	}
	inspectErr := inspectUnixOpenedRcloneVaultConfig(root, directory, repositoryID)
	return errors.Join(inspectErr, directory.Close(), root.Close())
}

func inspectRcloneVaultDirectorySecure(repositoryID string) error {
	root, directory, err := openUnixRcloneVault(repositoryID)
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		return err
	}
	defer root.Close()
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
		return fmt.Errorf("rclone vault directory contains ambiguous native residue")
	}
	config, mode, err := openUnixChild(directory, "rclone.conf")
	if err != nil {
		return err
	}
	defer config.Close()
	if !mode.IsRegular() {
		return fmt.Errorf("rclone vault config is linked or special")
	}
	if err := inspectUnixRcloneConfigFile(config); err != nil {
		return err
	}
	same, err := unixSameEntry(directory, "rclone.conf", config)
	if err != nil || !same {
		return fmt.Errorf("rclone vault config identity changed")
	}
	return revalidateUnixRcloneVault(root, directory, repositoryID)
}

func removeRcloneVaultDirectorySecure(repositoryID string) error {
	root, directory, err := openUnixRcloneVault(repositoryID)
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer root.Close()
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("rclone vault directory contains ambiguous native state")
	}
	if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
		return err
	}
	return unix.Unlinkat(int(root.Fd()), repositoryID, unix.AT_REMOVEDIR)
}

func removeRcloneVaultConfigSecure(repositoryID string) error {
	root, directory, err := openUnixRcloneVault(repositoryID)
	if err != nil {
		if root != nil {
			_ = root.Close()
		}
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer root.Close()
	defer directory.Close()
	return removeUnixOpenedRcloneVault(root, directory, repositoryID)
}

func removeUnixOpenedRcloneVault(
	root, directory *os.File,
	repositoryID string,
) error {
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return unix.Unlinkat(int(root.Fd()), repositoryID, unix.AT_REMOVEDIR)
	}
	if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
		return fmt.Errorf("rclone vault directory contains ambiguous native state")
	}
	fd, err := unix.Openat(
		int(directory.Fd()), "rclone.conf",
		unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
	)
	if err != nil {
		return err
	}
	config := os.NewFile(uintptr(fd), filepath.Join(directory.Name(), "rclone.conf"))
	defer config.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("rclone vault config is multiply linked")
	}
	if hook := rcloneWipeAfterOpenForTest; hook != nil {
		hook(config.Name())
	}
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("rclone vault config is multiply linked")
	}
	same, err := unixSameEntry(directory, "rclone.conf", config)
	if err != nil || !same {
		return fmt.Errorf("rclone vault config identity changed")
	}
	if err := revalidateUnixRcloneVault(root, directory, repositoryID); err != nil {
		return err
	}
	if hook := rcloneVaultRemovalBeforeMutationForTest; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(int(directory.Fd()), "rclone.conf", 0); err != nil {
		return fmt.Errorf("remove rclone vault config name: %w", err)
	}
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Nlink != 0 {
		return fmt.Errorf("rclone vault config is multiply linked after controlled-name removal")
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		return fmt.Errorf("wipe rclone vault config: %w", err)
	}
	if err := config.Sync(); err != nil {
		return err
	}
	return unix.Unlinkat(int(root.Fd()), repositoryID, unix.AT_REMOVEDIR)
}

func reconcileRcloneVaultConfigsSecure(ownerIDs map[string]bool) error {
	rootPath, err := appdata.RcloneVaultConfigRoot()
	if err != nil {
		return err
	}
	root, err := openUnixRcloneVaultRoot(rootPath, false)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := root.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := entry.Name()
		if !validRepositoryUUID(id) {
			continue
		}
		directory, mode, openErr := openUnixChild(root, id)
		if openErr != nil || !mode.IsDir() {
			if directory != nil {
				_ = directory.Close()
			}
			continue
		}
		if ownerIDs[id] {
			_ = directory.Close()
			continue
		}
		removable, classifyErr := unixRcloneVaultRemovable(directory)
		if err := directory.Close(); err != nil && classifyErr == nil {
			classifyErr = err
		}
		if classifyErr != nil || !removable {
			continue
		}
		directory, mode, openErr = openUnixChild(root, id)
		if openErr != nil || !mode.IsDir() {
			if directory != nil {
				_ = directory.Close()
			}
			continue
		}
		removeErr := removeUnixOpenedRcloneVault(root, directory, id)
		closeErr := directory.Close()
		if err := errors.Join(removeErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func unixRcloneVaultRemovable(directory *os.File) (bool, error) {
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return true, nil
	}
	if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
		return false, nil
	}
	fd, err := unix.Openat(
		int(directory.Fd()), "rclone.conf",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
	)
	if err != nil {
		return false, nil
	}
	config := os.NewFile(uintptr(fd), filepath.Join(directory.Name(), "rclone.conf"))
	defer config.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, err
	}
	return stat.Mode&unix.S_IFMT == unix.S_IFREG &&
		stat.Nlink == 1 &&
		stat.Size >= 0 &&
		stat.Size <= maximumRcloneVaultConfigBytes, nil
}
