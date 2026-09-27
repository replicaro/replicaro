//go:build windows

package storageidentity

import (
	"errors"
	"syscall"
)

const (
	windowsErrorFileNotFound = 2
	windowsErrorPathNotFound = 3
	windowsErrorAccessDenied = 5
	windowsErrorDirectory    = 267 // ERROR_DIRECTORY: "the directory name is invalid"

	// Credential failures on a network share. Windows reports these instead
	// of ERROR_ACCESS_DENIED when the saved or session credentials for the
	// server are wrong, expired, or locked out.
	windowsErrorInvalidPassword           = 86   // ERROR_INVALID_PASSWORD
	windowsErrorSessionCredentialConflict = 1219 // ERROR_SESSION_CREDENTIAL_CONFLICT
	windowsErrorLogonFailure              = 1326 // ERROR_LOGON_FAILURE
	windowsErrorPasswordExpired           = 1330 // ERROR_PASSWORD_EXPIRED
	windowsErrorAccountDisabled           = 1331 // ERROR_ACCOUNT_DISABLED
	windowsErrorPasswordMustChange        = 1907 // ERROR_PASSWORD_MUST_CHANGE
	windowsErrorAccountLockedOut          = 1909 // ERROR_ACCOUNT_LOCKED_OUT
)

const notDirectoryCode = int64(windowsErrorDirectory)

func osErrorCode(err error) (int64, bool) {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return int64(uint32(errno)), true
	}
	return 0, false
}

// classifyErrorCode matches exact Win32 codes instead of errors.Is(fs.ErrNotExist):
// Go maps ERROR_BAD_NETPATH (an offline server) to ErrNotExist, but an offline
// server must pause, not be treated as a deleted folder. ERROR_NOT_READY,
// ERROR_BAD_NETPATH, ERROR_NETNAME_DELETED, ERROR_UNEXP_NET_ERR and every
// other code fall through to "failed".
//
// The credential codes are "denied" on purpose, not "failed": the server
// answered and refused the user (for example lost or expired share
// credentials). That will not fix itself, so it is reported as an error the
// user must act on, the same as ERROR_ACCESS_DENIED. Treating them as
// "failed" would pause silently until the 30-day issue.
func classifyErrorCode(code int64) string {
	switch code {
	case windowsErrorFileNotFound, windowsErrorPathNotFound, windowsErrorDirectory:
		return AccessMissing
	case windowsErrorAccessDenied,
		windowsErrorInvalidPassword,
		windowsErrorSessionCredentialConflict,
		windowsErrorLogonFailure,
		windowsErrorPasswordExpired,
		windowsErrorAccountDisabled,
		windowsErrorPasswordMustChange,
		windowsErrorAccountLockedOut:
		return AccessDenied
	default:
		return AccessFailed
	}
}
