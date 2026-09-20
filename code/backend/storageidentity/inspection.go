package storageidentity

import (
	"errors"
	"os"
	"syscall"
)

// IsInspectionError identifies a failed local OS inspection, not failed
// identity proof. Call it only on errors from read-only filesystem observation;
// native command, credential and protected-metadata validation errors must keep
// their own meaning. Inspect every joined cause so an access failure cannot hide
// simultaneous conclusive identity evidence. Using native errno types rather
// than a portable errno allowlist covers disconnected mounts and Windows errors
// as well as missing, permission, and non-directory paths.
func IsInspectionError(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !IsInspectionError(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsInspectionError(wrapped.Unwrap())
	}
	switch err.(type) {
	case syscall.Errno, *MissingStorageError:
		return true
	}
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)
}

// A mounted file cannot contain a source or vault directory. This is only a
// candidate eligibility check; every admitted directory still needs its normal
// fresh identity proof. Failed inspection is not evidence of another identity.
func mountedDirectory(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}
