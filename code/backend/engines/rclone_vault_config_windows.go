//go:build windows

package engines

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/local/replicaro/appdata"
	"golang.org/x/sys/windows"
)

// FILE_ALL_ACCESS is the file-object mapping Windows stores after resolving
// GENERIC_ALL in an access-control entry.
const windowsRcloneFileAllAccess windows.ACCESS_MASK = 0x001f01ff

var rcloneVaultRemovalAfterWipeForTest func() error

func windowsRcloneFullControl(mask windows.ACCESS_MASK) bool {
	return mask == windows.GENERIC_ALL || mask == windowsRcloneFileAllAccess
}

func windowsRcloneTrustedOwner(
	owner, user, system, admin *windows.SID,
) bool {
	return owner != nil &&
		(owner.Equals(user) || owner.Equals(system) || owner.Equals(admin))
}

func windowsRcloneACEFormAllowed(
	protectedDACL bool,
	aceType, aceFlags uint8,
	mask windows.ACCESS_MASK,
) bool {
	return aceType == windows.ACCESS_ALLOWED_ACE_TYPE &&
		windowsRcloneFullControl(mask) &&
		(protectedDACL || aceFlags&windows.INHERITED_ACE != 0)
}

func rcloneVaultConfigSingleLink(path string, _ fs.FileInfo) bool {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	return windows.GetFileInformationByHandle(handle, &info) == nil &&
		info.NumberOfLinks == 1 &&
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func syncRcloneVaultDirectory(string) error { return nil }

func windowsRcloneConfigPrincipals() (*windows.SID, *windows.SID, *windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, nil, nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, nil, err
	}
	userSID, err := user.User.Sid.Copy()
	if err != nil {
		return nil, nil, nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, nil, nil, err
	}
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, nil, nil, err
	}
	return userSID, system, admin, nil
}

func secureWindowsRcloneHandle(file *os.File, directory bool) error {
	user, system, admin, err := windowsRcloneConfigPrincipals()
	if err != nil {
		return err
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, 3)
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	for _, principal := range []struct {
		sid  *windows.SID
		kind windows.TRUSTEE_TYPE
	}{{user, windows.TRUSTEE_IS_USER}, {system, windows.TRUSTEE_IS_USER}, {admin, windows.TRUSTEE_IS_GROUP}} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windowsRcloneFileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  principal.kind,
				TrusteeValue: windows.TrusteeValueFromSID(principal.sid),
			},
		})
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user, nil, dacl, nil,
	)
}

func secureWindowsRcloneConfigHandle(file *os.File) error {
	return secureWindowsRcloneHandle(file, false)
}

func inspectWindowsRcloneConfigFile(file *os.File) error {
	return inspectWindowsRcloneObject(file, false)
}

func sameRcloneVaultOpenedPath(path string, opened *os.File) error {
	openedInfo, err := opened.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(openedInfo, pathInfo) {
		return fmt.Errorf("rclone vault owned path identity changed")
	}
	return nil
}

func inspectRcloneVaultWindowsOwnedChain(chain []*os.File) error {
	if len(chain) < 3 {
		return fmt.Errorf("rclone vault owned application chain is incomplete")
	}
	for _, directory := range chain[len(chain)-3:] {
		if err := errors.Join(
			inspectWindowsRcloneObject(directory, true),
			sameRcloneVaultOpenedPath(directory.Name(), directory),
		); err != nil {
			return err
		}
	}
	return nil
}

func openRcloneVaultWindowsOwnedChain(rootPath string) ([]*os.File, error) {
	chain, err := openWindowsAncestorChain(rootPath)
	if err != nil {
		return nil, err
	}
	if err := inspectRcloneVaultWindowsOwnedChain(chain); err != nil {
		_ = closeWindowsChain(chain)
		return nil, err
	}
	return chain, nil
}

func inspectWindowsRcloneObject(file *os.File, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(file.Fd()), &info,
	); err != nil {
		return err
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || isDirectory != directory ||
		info.NumberOfLinks != 1 {
		return invalidRcloneLocalBinding(fmt.Errorf("rclone vault config is reparse, linked, or special"))
	}
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil || descriptor == nil {
		return fmt.Errorf("inspect rclone vault config ACL")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("inspect rclone vault config ACL control")
	}
	protectedDACL := control&windows.SE_DACL_PROTECTED != 0
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("inspect rclone vault config owner")
	}
	user, system, admin, err := windowsRcloneConfigPrincipals()
	if err != nil {
		return err
	}
	if !windowsRcloneTrustedOwner(owner, user, system, admin) {
		return fmt.Errorf("rclone vault config owner is unsafe")
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil || defaulted || dacl.AceCount != 3 {
		return fmt.Errorf("rclone vault config ACL is unsafe")
	}
	expected := []*windows.SID{user, system, admin}
	seen := make([]bool, len(expected))
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace == nil ||
			!windowsRcloneACEFormAllowed(
				protectedDACL,
				ace.Header.AceType,
				ace.Header.AceFlags,
				ace.Mask,
			) {
			return fmt.Errorf("rclone vault config ACL is unsafe")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		match := -1
		for principalIndex, principal := range expected {
			if sid.Equals(principal) {
				match = principalIndex
				break
			}
		}
		if match < 0 || seen[match] {
			return fmt.Errorf("rclone vault config ACL is unsafe")
		}
		seen[match] = true
	}
	for _, found := range seen {
		if !found {
			return fmt.Errorf("rclone vault config ACL is unsafe")
		}
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
	chain, err := openWindowsAncestorChain(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer closeWindowsChain(chain)
	file, err := openWindowsLocked(
		absolute, windows.GENERIC_READ|windows.READ_CONTROL, false,
	)
	if err != nil {
		return nil, err
	}
	if err := inspectWindowsRcloneConfigFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func hardenNewRcloneConfigFile(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	chain, err := openWindowsAncestorChain(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	defer closeWindowsChain(chain)
	file, err := openWindowsLocked(
		absolute,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|
			windows.WRITE_DAC|windows.WRITE_OWNER,
		false,
	)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := secureWindowsRcloneConfigHandle(file); err != nil {
		return err
	}
	return inspectWindowsRcloneConfigFile(file)
}

func touchRcloneVaultSidecarConfig(
	ctx context.Context,
	binary, repositoryID string,
	config, cache, temporary string,
) (*RcloneVaultConfigBinding, error) {
	if _, err := runRcloneVaultConfigCommand(
		ctx, binary,
		[]string{"config", "touch", "--config", config, "--cache-dir", cache,
			"--temp-dir", temporary, "--log-level", "ERROR"},
		nil, "", time.Minute, RcloneComponentID,
	); err != nil {
		return nil, err
	}
	if err := hardenNewRcloneConfigFile(config); err != nil {
		return nil, err
	}
	guard, err := openRcloneVaultConfigGuard(repositoryID)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(guard.inspect(), guard.close()); err != nil {
		return nil, err
	}
	binding := nonResticRcloneSidecarBinding(repositoryID, config)
	if err := binding.Revalidate(ctx); err != nil {
		return nil, err
	}
	return binding, nil
}

func inspectWindowsGuardedRcloneConfigWithEntries(
	path string,
	directory *os.File,
	requireOnlyConfig bool,
) error {
	openedDirectory, err := openWindowsLocked(
		filepath.Dir(path), windows.GENERIC_READ|windows.READ_CONTROL, true,
	)
	if err != nil {
		return err
	}
	currentInfo, currentErr := openedDirectory.Stat()
	expectedInfo, expectedErr := directory.Stat()
	closeErr := openedDirectory.Close()
	if err := errors.Join(currentErr, expectedErr, closeErr); err != nil {
		return err
	}
	if !os.SameFile(currentInfo, expectedInfo) {
		return fmt.Errorf("rclone vault directory identity changed")
	}
	if requireOnlyConfig {
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return os.ErrNotExist
		}
		if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
			return invalidRcloneLocalBinding(fmt.Errorf("rclone vault directory contains ambiguous native residue"))
		}
	} else if err := rejectRcloneCaseAlias(
		filepath.Dir(path), filepath.Base(path),
	); err != nil {
		return err
	}
	file, err := openWindowsLocked(
		path, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, false,
	)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := inspectWindowsRcloneConfigFile(file); err != nil {
		return err
	}
	if err := inspectBoundedRcloneVaultConfig(file); err != nil {
		return err
	}
	return sameRcloneVaultOpenedPath(path, file)
}

func inspectWindowsGuardedRcloneConfig(
	path string,
	directory *os.File,
) error {
	return inspectWindowsGuardedRcloneConfigWithEntries(path, directory, true)
}

func openStagedRcloneConfigGuard(path string) (*rcloneVaultConfigGuard, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if absolute != filepath.Clean(path) {
		return nil, invalidRcloneLocalBinding(fmt.Errorf("staged rclone config path is not canonical"))
	}
	guard := &rcloneVaultConfigGuard{path: absolute}
	guard.inspect = func() error {
		chain, err := openWindowsAncestorChain(filepath.Dir(absolute))
		if err != nil {
			return err
		}
		if len(chain) == 0 {
			_ = closeWindowsChain(chain)
			return fmt.Errorf("staged rclone config parent is unavailable")
		}
		inspectErr := inspectWindowsGuardedRcloneConfigWithEntries(
			absolute, chain[len(chain)-1], false,
		)
		return errors.Join(inspectErr, closeWindowsChain(chain))
	}
	guard.close = func() error { return nil }
	if err := guard.inspect(); err != nil {
		_ = guard.close()
		return nil, err
	}
	return guard, nil
}

func openRcloneVaultConfigGuard(repositoryID string) (*rcloneVaultConfigGuard, error) {
	path, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return nil, err
	}
	guard := &rcloneVaultConfigGuard{path: path}
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

type windowsRcloneRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func renameWindowsRcloneHandle(
	file *os.File,
	directory *os.File,
	name string,
) error {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	nameBytes := len(encoded)*2 - 2
	var layout windowsRcloneRenameInformation
	size := int(unsafe.Offsetof(layout.FileName)) + nameBytes
	buffer := make([]byte, size)
	info := (*windowsRcloneRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.ReplaceIfExists = windows.FILE_RENAME_REPLACE_IF_EXISTS
	info.RootDirectory = windows.Handle(directory.Fd())
	info.FileNameLength = uint32(nameBytes)
	copy(
		(*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.FileName[0]))[:nameBytes/2:nameBytes/2],
		encoded,
	)
	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		windows.Handle(file.Fd()), &status, &buffer[0], uint32(size),
		windows.FileRenameInformation,
	)
}

func newRcloneVaultPublication(repositoryID string) (*rcloneVaultPublication, error) {
	target, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return nil, err
	}
	rootPath := filepath.Dir(filepath.Dir(target))
	if err := prepareRcloneSessionRoot(rootPath); err != nil {
		return nil, fmt.Errorf("prepare rclone vault config root: %w", err)
	}
	chain, err := openRcloneVaultWindowsOwnedChain(rootPath)
	if err != nil {
		return nil, err
	}
	root := chain[len(chain)-1]
	if err := rejectRcloneCaseAlias(rootPath, repositoryID); err != nil {
		_ = closeWindowsChain(chain)
		return nil, err
	}
	objectName, err := windows.NewNTUnicodeString(repositoryID)
	if err != nil {
		_ = closeWindowsChain(chain)
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(root.Fd()),
		ObjectName:    objectName,
	}
	var (
		directoryHandle windows.Handle
		status          windows.IO_STATUS_BLOCK
		size            int64
	)
	err = windows.NtCreateFile(
		&directoryHandle,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|
			windows.WRITE_DAC|windows.WRITE_OWNER,
		attributes, &status, &size, windows.FILE_ATTRIBUTE_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_OPEN_IF,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT, 0, 0,
	)
	if err != nil {
		_ = closeWindowsChain(chain)
		return nil, err
	}
	directory := os.NewFile(uintptr(directoryHandle), filepath.Dir(target))
	var directoryInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		directoryHandle, &directoryInfo,
	); err != nil ||
		directoryInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		directoryInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = directory.Close()
		_ = closeWindowsChain(chain)
		return nil, fmt.Errorf("rclone vault directory is reparse or special")
	}
	// NtCreateFile reports FILE_CREATED as 2 in IO_STATUS_BLOCK.Information.
	// Existing native-owned directories are inspected without DACL repair.
	if status.Information == 2 {
		if err := secureWindowsRcloneHandle(directory, true); err != nil {
			_ = directory.Close()
			_ = closeWindowsChain(chain)
			return nil, err
		}
	} else if err := inspectWindowsRcloneObject(directory, true); err != nil {
		_ = directory.Close()
		_ = closeWindowsChain(chain)
		return nil, err
	}
	chain = append(chain, directory)
	if hook := rcloneVaultPublicationBeforeCreateForTest; hook != nil {
		if err := hook(filepath.Dir(target)); err != nil {
			_ = closeWindowsChain(chain)
			return nil, err
		}
	}
	publication := &rcloneVaultPublication{target: target}
	publication.activate = func(staged string) (RcloneConfigDisposition, error) {
		if hook := rcloneVaultPublicationAfterOpenForTest; hook != nil {
			if err := hook(filepath.Dir(target)); err != nil {
				return RcloneConfigRetained, err
			}
		}
		if err := errors.Join(
			inspectRcloneVaultWindowsOwnedChain(chain),
			inspectWindowsRcloneObject(directory, true),
			sameRcloneVaultOpenedPath(filepath.Dir(target), directory),
		); err != nil {
			return RcloneConfigRetained, err
		}
		return activateRcloneConfigCandidate(staged, directory, target)
	}
	publication.syncDirectory = func() error { return nil }
	publication.inspectTarget = func() error {
		if err := errors.Join(
			inspectRcloneVaultWindowsOwnedChain(chain),
			inspectWindowsRcloneObject(directory, true),
			sameRcloneVaultOpenedPath(filepath.Dir(target), directory),
		); err != nil {
			return err
		}
		opened, err := openWindowsLocked(
			target, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, false,
		)
		if err != nil {
			return err
		}
		defer opened.Close()
		if err := inspectWindowsRcloneConfigFile(opened); err != nil {
			return err
		}
		return inspectBoundedRcloneVaultConfig(opened)
	}
	publication.close = func() error {
		result := closeWindowsChain(chain)
		chain = nil
		directory = nil
		return result
	}
	return publication, nil
}

func prepareRcloneVaultDirectorySecure(repositoryID string) (string, error) {
	config, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return "", err
	}
	root := filepath.Dir(filepath.Dir(config))
	if err := prepareRcloneSessionRoot(root); err != nil {
		return "", fmt.Errorf("prepare rclone vault config root: %w", err)
	}
	chain, err := openRcloneVaultWindowsOwnedChain(root)
	if err != nil {
		return "", err
	}
	defer closeWindowsChain(chain)
	if err := rejectRcloneCaseAlias(root, repositoryID); err != nil {
		return "", err
	}
	directory := filepath.Dir(config)
	mkdirErr := os.Mkdir(directory, 0o700)
	if mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
		return "", fmt.Errorf("create rclone vault directory: %w", mkdirErr)
	}
	opened, err := openWindowsLocked(
		directory, windows.GENERIC_READ|windows.READ_CONTROL, true,
	)
	if err != nil {
		return "", fmt.Errorf("rclone vault directory is linked, special, or unavailable")
	}
	defer opened.Close()
	if mkdirErr == nil {
		if err := appdata.SecurePath(directory, true); err != nil {
			return "", fmt.Errorf("protect new rclone vault directory: %w", err)
		}
	}
	if err := inspectWindowsRcloneObject(opened, true); err != nil {
		return "", fmt.Errorf("existing rclone vault directory is unsafe: %w", err)
	}
	return config, nil
}

func inspectRcloneVaultConfigSecure(repositoryID string) error {
	config, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(config))
	if err := rejectRcloneCaseAlias(root, repositoryID); err != nil {
		return err
	}
	chain, err := openRcloneVaultWindowsOwnedChain(root)
	if err != nil {
		return err
	}
	defer closeWindowsChain(chain)
	directory, err := openWindowsLocked(
		filepath.Dir(config), windows.GENERIC_READ|windows.READ_CONTROL, true,
	)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := errors.Join(
		inspectWindowsRcloneObject(directory, true),
		sameRcloneVaultOpenedPath(filepath.Dir(config), directory),
	); err != nil {
		return err
	}
	return inspectWindowsGuardedRcloneConfig(config, directory)
}

func inspectRcloneVaultDirectorySecure(repositoryID string) error {
	config, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(config))
	chain, err := openRcloneVaultWindowsOwnedChain(root)
	if err != nil {
		return err
	}
	defer closeWindowsChain(chain)
	directory, err := openWindowsLocked(
		filepath.Dir(config), windows.GENERIC_READ|windows.READ_CONTROL, true,
	)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := errors.Join(
		inspectWindowsRcloneObject(directory, true),
		sameRcloneVaultOpenedPath(filepath.Dir(config), directory),
	); err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
		return fmt.Errorf("rclone vault directory contains ambiguous native residue")
	}
	file, err := openWindowsLocked(config, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, false)
	if err != nil {
		return err
	}
	return file.Close()
}

func removeRcloneVaultDirectorySecure(repositoryID string) error {
	config, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(config))
	if err := rejectRcloneCaseAlias(root, repositoryID); err != nil {
		if errors.Is(err, os.ErrNotExist) ||
			errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	chain, err := openRcloneVaultWindowsOwnedChain(root)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	defer closeWindowsChain(chain)
	directory, err := openWindowsLocked(
		filepath.Dir(config),
		windows.GENERIC_READ|windows.READ_CONTROL|windows.DELETE|windows.FILE_WRITE_ATTRIBUTES,
		true,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	defer directory.Close()
	if err := inspectWindowsRcloneObject(directory, true); err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("rclone vault directory contains ambiguous native state")
	}
	return deleteWindowsHandle(directory)
}

func removeRcloneVaultConfigSecure(repositoryID string) error {
	configPath, err := RcloneVaultConfigPath(repositoryID)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(configPath))
	if err := rejectRcloneCaseAlias(root, repositoryID); err != nil {
		if errors.Is(err, os.ErrNotExist) ||
			errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	chain, err := openRcloneVaultWindowsOwnedChain(root)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	defer closeWindowsChain(chain)
	directory, err := openWindowsLocked(
		filepath.Dir(configPath),
		windows.GENERIC_READ|windows.READ_CONTROL|windows.DELETE|windows.FILE_WRITE_ATTRIBUTES,
		true,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	defer directory.Close()
	if err := inspectWindowsRcloneObject(directory, true); err != nil {
		return err
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return deleteWindowsHandle(directory)
	}
	if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
		return fmt.Errorf("rclone vault directory contains ambiguous native state")
	}
	config, err := openWindowsLocked(
		configPath,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
		false,
	)
	if err != nil {
		return err
	}
	defer config.Close()
	if hook := rcloneWipeAfterOpenForTest; hook != nil {
		hook(configPath)
	}
	var configInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(config.Fd()), &configInfo,
	); err != nil {
		return err
	}
	if configInfo.NumberOfLinks != 1 ||
		configInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("rclone vault config is reparse or multiply linked")
	}
	if hook := rcloneVaultRemovalBeforeMutationForTest; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	if _, err := config.Seek(0, 0); err != nil {
		return err
	}
	if err := windows.SetEndOfFile(windows.Handle(config.Fd())); err != nil {
		return fmt.Errorf("wipe rclone vault config: %w", err)
	}
	if err := windows.FlushFileBuffers(windows.Handle(config.Fd())); err != nil {
		return err
	}
	if hook := rcloneVaultRemovalAfterWipeForTest; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	if err := deleteWindowsHandle(config); err != nil {
		return err
	}
	if err := config.Close(); err != nil {
		return err
	}
	return deleteWindowsHandle(directory)
}

func reconcileRcloneVaultConfigsSecure(ownerIDs map[string]bool) error {
	root, err := appdata.RcloneVaultConfigRoot()
	if err != nil {
		return err
	}
	chain, err := openRcloneVaultWindowsOwnedChain(root)
	if err != nil {
		return err
	}
	defer closeWindowsChain(chain)
	entries, err := chain[len(chain)-1].ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := entry.Name()
		if !validRepositoryUUID(id) || ownerIDs[id] {
			continue
		}
		directory := filepath.Join(root, id)
		info, statErr := os.Lstat(directory)
		if statErr != nil || !info.IsDir() ||
			info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		children, readErr := os.ReadDir(directory)
		if readErr != nil || len(children) > 1 ||
			(len(children) == 1 && children[0].Name() != "rclone.conf") {
			continue
		}
		if len(children) == 1 {
			if err := validateRcloneVaultFile(
				filepath.Join(directory, "rclone.conf"),
			); err != nil {
				continue
			}
		}
		// The held ancestor chain denies rename/delete of the selected root
		// while the removal helper reacquires and deletes this exact UUID.
		if err := removeRcloneVaultConfigSecure(id); err != nil {
			return err
		}
	}
	return nil
}
