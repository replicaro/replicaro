//go:build windows

package command

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/local/replicaro/appdata"
	"golang.org/x/sys/windows"
)

const windowsFileAllAccess windows.ACCESS_MASK = 0x001f01ff

var beforeWindowsCapturedOutputRelativeMutation func()

func capturedOutputDirectory() (string, error) {
	logs, err := appdata.OperationLogDirectoryPath()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(logs)
	directory := filepath.Join(root, "command-output")
	if err := ensureWindowsCapturedOutputDirectory(root); err != nil {
		return "", err
	}
	if err := ensureWindowsCapturedOutputDirectory(directory); err != nil {
		return "", err
	}
	return directory, nil
}

// The final component is created and secured relative to a validated parent
// handle, so a pathname swap cannot redirect the ACL or child mutation.
func ensureWindowsCapturedOutputDirectory(path string) error {
	parent, err := openWindowsCapturedOutputDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	directory, err := openWindowsRelativeDirectory(parent, filepath.Base(path), windows.FILE_OPEN_IF)
	if err != nil {
		return fmt.Errorf("create captured-output directory: %w", err)
	}
	defer directory.Close()
	if err := validateWindowsCapturedOutputDirectory(directory); err != nil {
		return err
	}
	return secureWindowsCapturedOutputHandle(directory, true)
}

func openWindowsCapturedOutputDirectory(path string) (*os.File, error) {
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
		return nil, fmt.Errorf("open captured-output directory")
	}
	if err := validateWindowsCapturedOutputDirectory(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openWindowsRelativeDirectory(parent *os.File, name string, disposition uint32) (*os.File, error) {
	return openWindowsRelativeCapturedOutput(parent, name,
		uint32(windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_DATA|
			windows.FILE_APPEND_DATA|windows.FILE_TRAVERSE|windows.READ_CONTROL|windows.WRITE_DAC|
			windows.WRITE_OWNER|windows.SYNCHRONIZE), disposition,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT)
}

func validateWindowsCapturedOutputDirectory(file *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("captured-output directory is unsafe")
	}
	return nil
}

func createCapturedOutputFile(directory, pattern string) (*os.File, error) {
	parent, err := openWindowsCapturedOutputDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if beforeWindowsCapturedOutputRelativeMutation != nil {
		beforeWindowsCapturedOutputRelativeMutation()
	}
	for attempts := 0; attempts < 128; attempts++ {
		name, err := capturedOutputTempName(pattern)
		if err != nil {
			return nil, err
		}
		file, err := openWindowsRelativeCapturedOutput(parent, name,
			uint32(windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|
				windows.WRITE_DAC|windows.WRITE_OWNER|windows.DELETE),
			windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
				continue
			}
			return nil, err
		}
		if err := validateWindowsCapturedOutputFile(file); err != nil {
			_ = file.Close()
			_ = removeCapturedOutput(filepath.Join(directory, name))
			return nil, err
		}
		if err := secureWindowsCapturedOutputHandle(file, false); err != nil {
			_ = file.Close()
			_ = removeCapturedOutput(filepath.Join(directory, name))
			return nil, err
		}
		return file, nil
	}
	return nil, fmt.Errorf("allocate captured command output name")
}

func capturedOutputTempName(pattern string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return strings.Replace(pattern, "*", hex.EncodeToString(random), 1), nil
}

func openWindowsRelativeCapturedOutput(parent *os.File, name string, access, disposition, options uint32) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("captured-output child name is invalid")
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
		return nil, err
	}
	file := os.NewFile(uintptr(handle), filepath.Join(parent.Name(), name))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("open captured command output")
	}
	return file, nil
}

func openCapturedOutput(path string) (*os.File, error) {
	parent, err := openWindowsCapturedOutputDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	file, err := openWindowsRelativeCapturedOutput(parent, filepath.Base(path),
		uint32(windows.FILE_GENERIC_READ|windows.READ_CONTROL), windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if err != nil {
		return nil, err
	}
	if err := validateWindowsCapturedOutputFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func validateWindowsCapturedOutputFile(file *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.NumberOfLinks != 1 {
		return fmt.Errorf("captured command output must be a single-link regular non-reparse file")
	}
	return nil
}

func removeCapturedOutput(path string) error {
	parent, err := openWindowsCapturedOutputDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	file, err := openWindowsRelativeCapturedOutput(parent, filepath.Base(path),
		uint32(windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE), windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := validateWindowsCapturedOutputFile(file); err != nil {
		return err
	}
	deleteFile := byte(1)
	return windows.SetFileInformationByHandle(windows.Handle(file.Fd()), windows.FileDispositionInfo, &deleteFile, 1)
}

func cleanupCapturedOutputDirectory(directory string) error {
	parent, err := openWindowsCapturedOutputDirectory(directory)
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
		name := entry.Name()
		if (!strings.HasPrefix(name, ".stdout-") && !strings.HasPrefix(name, ".stderr-")) ||
			!strings.HasSuffix(name, ".tmp") || !entry.Type().IsRegular() {
			continue
		}
		if removeErr := removeCapturedOutput(filepath.Join(directory, name)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, removeErr)
		}
	}
	return errors.Join(cleanupErrors...)
}

func secureWindowsCapturedOutputHandle(file *os.File, directory bool) error {
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
			AccessPermissions: windowsFileAllAccess,
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
