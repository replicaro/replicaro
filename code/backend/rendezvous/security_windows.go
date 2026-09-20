//go:build windows

package rendezvous

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func validateOwnerOnly(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("rendezvous state is not a regular file")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return fmt.Errorf("read rendezvous ACL: %w", err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("rendezvous ACL is not protected")
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return fmt.Errorf("rendezvous owner is not the current user")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil || dacl == nil || defaulted || dacl.AceCount != 3 {
		return fmt.Errorf("rendezvous DACL is not the exact trusted-principal allowlist")
	}
	trusted := []*windows.SID{user.User.Sid, system, admins}
	seen := make([]bool, len(trusted))
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil || ace == nil ||
			ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&(windows.INHERITED_ACE|windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != 0 ||
			ace.Mask != windows.GENERIC_ALL && ace.Mask != 0x001f01ff {
			return fmt.Errorf("rendezvous DACL permits another principal or access form")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		matched := -1
		for principalIndex, principal := range trusted {
			if sid.Equals(principal) {
				matched = principalIndex
				break
			}
		}
		if matched < 0 || seen[matched] {
			return fmt.Errorf("rendezvous DACL permits another principal")
		}
		seen[matched] = true
	}
	for _, found := range seen {
		if !found {
			return fmt.Errorf("rendezvous DACL omits a trusted principal")
		}
	}
	return nil
}

func processAlive(pid int) bool {
	const stillActive = 259
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	return windows.GetExitCodeProcess(handle, &exitCode) == nil && exitCode == stillActive
}
