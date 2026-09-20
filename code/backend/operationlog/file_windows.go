//go:build windows

package operationlog

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/local/replicaro/appdata"
	"golang.org/x/sys/windows"
)

const windowsOperationLogFileAllAccess windows.ACCESS_MASK = 0x001f01ff

var beforeWindowsOperationLogRelativeMutation func()

func operationLogDirectory() (string, error) {
	directory, err := appdata.OperationLogDirectoryPath()
	if err != nil {
		return "", err
	}
	if err := ensureWindowsOperationLogDirectory(filepath.Dir(directory)); err != nil {
		return "", err
	}
	if err := ensureWindowsOperationLogDirectory(directory); err != nil {
		return "", err
	}
	return directory, nil
}

func createStageFile(directory, operationID, kind, stream string) (*os.File, error) {
	safeKind := strings.NewReplacer("/", "_", "\\", "_", "\x00", "_").Replace(kind)
	pattern := "." + operationID + "." + safeKind + "." + stream + "-*.tmp"
	parent, err := openWindowsDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	for attempts := 0; attempts < 128; attempts++ {
		name, err := windowsOperationLogTempName(pattern)
		if err != nil {
			return nil, err
		}
		file, err := openWindowsRelativeLogFile(parent, name,
			uint32(windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|
				windows.WRITE_DAC|windows.WRITE_OWNER|windows.DELETE), windows.FILE_CREATE)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
				continue
			}
			return nil, err
		}
		if err := validateWindowsOperationLogFile(file); err != nil {
			_ = file.Close()
			_ = removeOperationLogFile(filepath.Join(directory, name))
			return nil, err
		}
		if err := secureWindowsOperationLogHandle(file, false); err != nil {
			_ = file.Close()
			_ = removeOperationLogFile(filepath.Join(directory, name))
			return nil, err
		}
		return file, nil
	}
	return nil, fmt.Errorf("allocate operation-log stage name")
}

func windowsOperationLogTempName(pattern string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return strings.Replace(pattern, "*", hex.EncodeToString(random), 1), nil
}

// Directory creation and ACL application stay relative to a continuously
// validated parent handle. A concurrent junction swap cannot redirect either
// operation to an external target.
func ensureWindowsOperationLogDirectory(path string) error {
	parent, err := openWindowsDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	directory, err := openWindowsRelativeLogDirectory(parent, filepath.Base(path), windows.FILE_OPEN_IF)
	if err != nil {
		return fmt.Errorf("create operation log directory: %w", err)
	}
	defer directory.Close()
	if err := validateWindowsOperationLogDirectory(directory); err != nil {
		return err
	}
	return secureWindowsOperationLogHandle(directory, true)
}

func openWindowsDirectory(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_DATA |
		windows.FILE_APPEND_DATA | windows.FILE_TRAVERSE | windows.READ_CONTROL | windows.WRITE_DAC |
		windows.WRITE_OWNER | windows.SYNCHRONIZE)
	handle, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open operation log directory")
	}
	if err := validateWindowsOperationLogDirectory(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openWindowsRelativeLogDirectory(parent *os.File, name string, disposition uint32) (*os.File, error) {
	return openWindowsRelativeLogObject(parent, name,
		uint32(windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_DATA|
			windows.FILE_APPEND_DATA|windows.FILE_TRAVERSE|windows.READ_CONTROL|windows.WRITE_DAC|
			windows.WRITE_OWNER|windows.SYNCHRONIZE), disposition,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT)
}

func validateWindowsOperationLogDirectory(file *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("operation log directory is unsafe")
	}
	return nil
}

func openAppend(path string) (*os.File, error) {
	parent, err := openWindowsDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if beforeWindowsOperationLogRelativeMutation != nil {
		beforeWindowsOperationLogRelativeMutation()
	}
	file, err := openWindowsRelativeLogFile(parent, filepath.Base(path),
		uint32(windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|
			windows.WRITE_DAC|windows.WRITE_OWNER), windows.FILE_OPEN_IF)
	if err != nil {
		return nil, err
	}
	if err := validateWindowsOperationLogFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := secureWindowsOperationLogHandle(file, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openRead(path string) (*os.File, error) {
	parent, err := openWindowsDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	file, err := openWindowsRelativeLogFile(parent, filepath.Base(path),
		uint32(windows.FILE_GENERIC_READ|windows.READ_CONTROL), windows.FILE_OPEN)
	if err != nil {
		return nil, err
	}
	if err := validateWindowsOperationLogFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openWindowsRelativeLogFile(parent *os.File, name string, access, disposition uint32) (*os.File, error) {
	return openWindowsRelativeLogObject(parent, name, access, disposition,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT)
}

func openWindowsRelativeLogObject(parent *os.File, name string, access, disposition, options uint32) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("operation-log child name is invalid")
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	allocation := int64(0)
	err = windows.NtCreateFile(&handle, access, attributes, &status, &allocation,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition, options, 0, 0)
	if err != nil {
		if disposition == windows.FILE_OPEN && windowsOperationLogObjectMissing(err) {
			return nil, &os.PathError{Op: "open", Path: filepath.Join(parent.Name(), name), Err: os.ErrNotExist}
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), filepath.Join(parent.Name(), name))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open operation log")
	}
	return file, nil
}

func windowsOperationLogObjectMissing(err error) bool {
	return errors.Is(err, windows.STATUS_NO_SUCH_FILE) ||
		errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) ||
		errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND)
}

func validateWindowsOperationLogFile(file *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.NumberOfLinks != 1 {
		return fmt.Errorf("operation log path is unsafe")
	}
	return nil
}

func removeOperationLogFile(path string) error {
	parent, err := openWindowsDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	file, err := openWindowsRelativeLogFile(parent, filepath.Base(path),
		uint32(windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE), windows.FILE_OPEN)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := validateWindowsOperationLogFile(file); err != nil {
		return err
	}
	deleteFile := byte(1)
	return windows.SetFileInformationByHandle(windows.Handle(file.Fd()), windows.FileDispositionInfo, &deleteFile, 1)
}

func cleanupStagedOutputFiles(directory string) error {
	parent, err := openWindowsDirectory(directory)
	if err != nil {
		return err
	}
	entries, err := parent.ReadDir(-1)
	closeErr := parent.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	var cleanupErrors []error
	for _, entry := range entries {
		if !stageFilename(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		if removeErr := removeOperationLogFile(filepath.Join(directory, entry.Name())); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, removeErr)
		}
	}
	return errors.Join(cleanupErrors...)
}

func secureWindowsOperationLogHandle(file *os.File, directory bool) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return fmt.Errorf("open process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read process user: %w", err)
	}
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, 3)
	for _, principal := range []struct {
		sid  *windows.SID
		kind windows.TRUSTEE_TYPE
	}{
		{user.User.Sid, windows.TRUSTEE_IS_USER},
		{system, windows.TRUSTEE_IS_USER},
		{admin, windows.TRUSTEE_IS_GROUP},
	} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windowsOperationLogFileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID,
				TrusteeType: principal.kind, TrusteeValue: windows.TrusteeValueFromSID(principal.sid)},
		})
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build protected ACL: %w", err)
	}
	return windows.SetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid, nil, dacl, nil)
}
