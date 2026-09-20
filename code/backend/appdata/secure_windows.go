//go:build windows

package appdata

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// FILE_ALL_ACCESS is the concrete file-object mask used for both files and
// directories. Avoid generic rights here: inheritable generic directory ACEs
// can be expanded into separate object and child ACEs by the filesystem.
const windowsFileAllAccess windows.ACCESS_MASK = 0x001f01ff

// SecurePath makes the current user the explicit owner and installs a protected
// DACL for that user, LocalSystem, and administrators. Directory ACEs inherit
// to connector configs and sidecars.
func SecurePath(path string, directory bool) error {
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
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  principal.kind,
				TrusteeValue: windows.TrusteeValueFromSID(principal.sid),
			},
		})
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build protected ACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid, nil, dacl, nil); err != nil {
		return fmt.Errorf("secure application data path: %w", err)
	}
	return nil
}
